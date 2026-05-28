package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Dump streams every document under indices matching `prefix*` as
// `_bulk`-format NDJSON, with every occurrence of the literal prefix
// in `_index` replaced by `{target_db}`. The output round-trips
// through Driver.Restore — `Restore(targetPrefix, dumpPath)`
// substitutes `{target_db}` back to whatever prefix the destination
// worktree uses.
//
// Uses the scroll API (5m TTL, 1000 docs per batch) so dumps of
// arbitrarily large indices stream linearly without OOMing the
// client or the server. Scroll context is released on every exit
// path so a crashing client doesn't leak ES heap.
func (d *Driver) Dump(ctx context.Context, prefix string, w io.Writer) error {
	if prefix == "" {
		return fmt.Errorf("es dump: empty prefix would scan the entire cluster")
	}
	scrollID, hits, err := d.openScroll(ctx, prefix)
	if err != nil {
		return err
	}
	defer d.releaseScroll(scrollID)

	if err := d.writeHits(w, prefix, hits); err != nil {
		return err
	}
	for len(hits) > 0 {
		var next []scrollHit
		scrollID, next, err = d.continueScroll(ctx, scrollID)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			break
		}
		if err := d.writeHits(w, prefix, next); err != nil {
			return err
		}
		hits = next
	}
	return nil
}

type scrollHit struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Source json.RawMessage `json:"_source"`
}

type scrollResp struct {
	ScrollID string `json:"_scroll_id"`
	Hits     struct {
		Hits []scrollHit `json:"hits"`
	} `json:"hits"`
}

func (d *Driver) openScroll(ctx context.Context, prefix string) (string, []scrollHit, error) {
	body := `{"size":1000,"query":{"match_all":{}}}`
	req, err := http.NewRequestWithContext(ctx, "POST",
		d.Base+"/"+prefix+"*/_search?scroll=5m", strings.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.doScroll(req)
}

func (d *Driver) continueScroll(ctx context.Context, scrollID string) (string, []scrollHit, error) {
	body, _ := json.Marshal(map[string]string{
		"scroll":    "5m",
		"scroll_id": scrollID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		d.Base+"/_search/scroll", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.doScroll(req)
}

func (d *Driver) doScroll(req *http.Request) (string, []scrollHit, error) {
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("es scroll: HTTP %d: %s", resp.StatusCode, truncate(string(body), 1024))
	}
	var r scrollResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", nil, fmt.Errorf("es scroll: parse: %w", err)
	}
	return r.ScrollID, r.Hits.Hits, nil
}

// releaseScroll best-effort frees the scroll context server-side.
// Failures are swallowed because we're already on an exit path.
func (d *Driver) releaseScroll(scrollID string) {
	if scrollID == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"scroll_id": scrollID})
	req, err := http.NewRequest("DELETE", d.Base+"/_search/scroll", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// writeHits emits one bulk-format action+doc pair per hit. Replaces
// the literal `prefix` at the start of `_index` with `{target_db}`
// so the dump file is portable across worktrees — Driver.Restore
// substitutes it back to the destination prefix.
func (d *Driver) writeHits(w io.Writer, prefix string, hits []scrollHit) error {
	for _, h := range hits {
		idx := h.Index
		if strings.HasPrefix(idx, prefix) {
			idx = "{target_db}" + idx[len(prefix):]
		}
		action, err := json.Marshal(map[string]any{
			"index": map[string]string{
				"_index": idx,
				"_id":    h.ID,
			},
		})
		if err != nil {
			return err
		}
		if _, err := w.Write(action); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
		if _, err := w.Write(h.Source); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}
