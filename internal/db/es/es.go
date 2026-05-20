// Package es is the treeman Elasticsearch / OpenSearch driver.
// Ported from `crates/treeman-db/src/elasticsearch.rs`. Pure HTTP
// via net/http — no fat client dependency.
package es

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps an http.Client + base URL.
type Driver struct {
	Base string
	HTTP *http.Client
}

// Connect probes reachability + returns a Driver. Auth (api key /
// basic) is left for a future patch; kontainer uses unauthenticated
// local ES.
func Connect(ctx context.Context, cfg config.EsConn) (*Driver, error) {
	if err := reachability.ProbeURL("elasticsearch", cfg.URL); err != nil {
		return nil, err
	}
	return &Driver{
		Base: strings.TrimRight(cfg.URL, "/"),
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// EngineVersion fetches the cluster root, parses the `version.number`
// field. Empty string + nil error if missing.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	body, err := d.get(ctx, "/")
	if err != nil {
		return "", err
	}
	var v struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	return v.Version.Number, nil
}

// DropMatching deletes every index whose name starts with prefix.
// ES rejects wildcard DELETE when
// action.destructive_requires_name=true (the cluster default), so
// list-then-delete-by-name.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	names, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if _, err := d.delete(ctx, "/"+n); err != nil {
			return nil, fmt.Errorf("DELETE %s: %w", n, err)
		}
	}
	return names, nil
}

// ListMatching returns every index whose name starts with prefix.
func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	body, err := d.get(ctx, "/_cat/indices/"+prefix+"*?h=index&format=json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		// Fallback: text format (older ES versions return blank).
		var out []string
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Index)
	}
	return out, nil
}

func (d *Driver) get(ctx context.Context, path string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.Base+path, nil)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("GET %s → HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

func (d *Driver) delete(ctx context.Context, path string) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, d.Base+path, nil)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		// Already gone — treat as success for idempotent teardown.
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("DELETE %s → HTTP %d", path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
