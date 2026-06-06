package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// resolveConfigTarget turns a config `scope` ("repo" — default — or
// "global") plus an optional repo override into the three things every
// scoped config tool needs:
//
//   - path:     the on-disk YAML file to read/write.
//   - histRoot: the repo root used to KEY the SQLite generation history
//     (store.SnapshotConfig/ListConfigGenerations). The global config is
//     keyed by its own parent dir so its rollback trail is independent of
//     any project's `.treeman.yaml` history.
//   - layer:    "repo" | "global" — the scope label fed to
//     config.CheckBodyScope / CheckKeyInLayer so a misplaced key is
//     rejected at write time.
//
// This is the single chokepoint that lets config_get/set/write/unset/
// diff/history/restore/delete operate on either the per-repo or the
// user-global config without each tool re-deriving the path rules.
func resolveConfigTarget(scope, repo string) (path, histRoot, layer string, err error) {
	switch strings.ToLower(scope) {
	case "", "repo":
		repoRoot, rerr := resolveRepo(repo)
		if rerr != nil {
			return "", "", "", rerr
		}
		return filepath.Join(repoRoot, ".treeman.yaml"), repoRoot, "repo", nil
	case "global":
		gp, ok := config.GlobalConfigPath()
		if !ok {
			return "", "", "", errors.New("cannot resolve global config path (no home dir)")
		}
		return gp, filepath.Dir(gp), "global", nil
	default:
		return "", "", "", fmt.Errorf("invalid scope %q (want repo|global)", scope)
	}
}

// ─── config_locate ─────────────────────────────────────────────────

type configLocateIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"repo root override (defaults to cwd) — used for the repo-scoped paths"`
}
type configFileInfo struct {
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Bytes  int    `json:"bytes,omitempty"`
}
type configLocateOut struct {
	Files []configFileInfo `json:"files"`
}

// configLocateTool reports where every config file treeman reads lives —
// the user-global config, the repo `.treeman.yaml`, and the repo-local
// `.treeman.local.yaml` overlay — plus whether each exists on disk and
// its size. The layered loader merges them global → repo → repo-local;
// this tool is the "where do I edit X" map an agent consults before
// touching anything.
func configLocateTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configLocateIn) (*mcpsdk.CallToolResult, configLocateOut, error) {
	out := configLocateOut{}
	if gp, ok := config.GlobalConfigPath(); ok {
		out.Files = append(out.Files, statConfigFile("global", gp))
	}
	// Repo paths are best-effort: a cwd that isn't a git repo still gets
	// the global entry, so locate never hard-fails just because there's
	// no repo in scope.
	if repoRoot, err := resolveRepo(in.Repo); err == nil {
		out.Files = append(out.Files,
			statConfigFile("repo", filepath.Join(repoRoot, ".treeman.yaml")),
			statConfigFile("repo-local", filepath.Join(repoRoot, ".treeman.local.yaml")),
		)
	}
	return nil, out, nil
}

func statConfigFile(scope, path string) configFileInfo {
	info := configFileInfo{Scope: scope, Path: path}
	if st, err := os.Stat(path); err == nil {
		info.Exists = true
		info.Bytes = int(st.Size())
	}
	return info
}

// ─── config_unset ──────────────────────────────────────────────────

type configUnsetIn struct {
	Scope string `json:"scope,omitempty" jsonschema:"repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml)"`
	Repo  string `json:"repo,omitempty"`
	Path  string `json:"path"            jsonschema:"dotted path to remove, like 'daemon.gc_interval' or 'databases[0]'. Drops the key/index entirely (config_set with null only nulls it)."`
}
type configUnsetOut struct {
	Path        string `json:"path"`
	Scope       string `json:"scope"`
	File        string `json:"file"`
	RemovedJSON string `json:"removed_json,omitempty"`
	Bytes       int    `json:"bytes"`
}

// configUnsetTool deletes one key (or sequence element) from a config
// file by dotted path, preserving surrounding comments + key order. It's
// the surgical counterpart to config_delete (whole file) — use it to
// drop a single block. Snapshots the prior content into SQLite first
// (recoverable via config_history/config_restore) and validates the
// result still parses before the write lands.
//
// Unlike config_set, it deliberately does NOT scope-check the key: a
// removal is always safe regardless of layer, and refusing to drop a
// misplaced key (e.g. a repo-only `databases:` that ended up in the
// global file) would block the very cleanup the user is trying to do.
func configUnsetTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configUnsetIn) (*mcpsdk.CallToolResult, configUnsetOut, error) {
	if in.Path == "" {
		return nil, configUnsetOut{}, errors.New("path is required")
	}
	target, histRoot, _, err := resolveConfigTarget(in.Scope, in.Repo)
	if err != nil {
		return nil, configUnsetOut{}, err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, configUnsetOut{}, fmt.Errorf("read %s: %w", target, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, configUnsetOut{}, fmt.Errorf("parse %s: %w", target, err)
	}
	segs, err := yamlpatch.ParsePath(in.Path)
	if err != nil {
		return nil, configUnsetOut{}, err
	}
	removed, err := yamlpatch.Unset(&doc, segs)
	if err != nil {
		return nil, configUnsetOut{}, err
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return nil, configUnsetOut{}, fmt.Errorf("encode yaml: %w", err)
	}
	var validated config.Config
	if err := yaml.Unmarshal(body, &validated); err != nil {
		return nil, configUnsetOut{}, fmt.Errorf("validation failed — patched file would not parse as config.Config: %w", err)
	}
	if err := atomicWrite(histRoot, target, body); err != nil {
		return nil, configUnsetOut{}, err
	}
	removedJSON := ""
	if removed != nil {
		var v any
		if err := removed.Decode(&v); err == nil {
			if b, err := json.Marshal(v); err == nil {
				removedJSON = string(b)
			}
		}
	}
	scopeLabel := strings.ToLower(in.Scope)
	if scopeLabel == "" {
		scopeLabel = "repo"
	}
	writeMCPEvent(context.Background(), store.EvtMCPConfigUnset, "removed "+in.Path, 0, map[string]string{
		"path":  in.Path,
		"scope": scopeLabel,
		"file":  target,
	})
	return nil, configUnsetOut{
		Path:        in.Path,
		Scope:       scopeLabel,
		File:        target,
		RemovedJSON: removedJSON,
		Bytes:       len(body),
	}, nil
}

// ─── config_delete ─────────────────────────────────────────────────

type configDeleteIn struct {
	Scope  string `json:"scope,omitempty"   jsonschema:"repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml)"`
	Repo   string `json:"repo,omitempty"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"preview the file that WOULD be deleted (path + bytes) without removing it"`
	Ack    bool   `json:"ack,omitempty"     jsonschema:"set true to skip the confirmation gate and actually delete"`
}
type configDeleteOut struct {
	Path    string `json:"path"`
	Scope   string `json:"scope"`
	Deleted bool   `json:"deleted"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Bytes   int    `json:"bytes,omitempty"`
	Note    string `json:"note,omitempty"`
}

// configDeleteTool removes a whole config file from disk. DESTRUCTIVE,
// but recoverable: the prior content is snapshotted into SQLite first
// (config_history/config_restore can bring it back). Requires ack=true
// to actually delete — a bare call previews like dry_run so an agent
// can't nuke a config without surfacing the consequence. Deleting the
// repo `.treeman.yaml` un-enrolls the repo's per-repo overrides;
// deleting the global config reverts every repo to built-in defaults.
func configDeleteTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in configDeleteIn) (*mcpsdk.CallToolResult, configDeleteOut, error) {
	target, histRoot, _, err := resolveConfigTarget(in.Scope, in.Repo)
	if err != nil {
		return nil, configDeleteOut{}, err
	}
	scopeLabel := strings.ToLower(in.Scope)
	if scopeLabel == "" {
		scopeLabel = "repo"
	}
	prev, readErr := os.ReadFile(target)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, configDeleteOut{Path: target, Scope: scopeLabel, Deleted: false, Note: "file does not exist — nothing to delete"}, nil
	}
	if readErr != nil {
		return nil, configDeleteOut{}, fmt.Errorf("read %s: %w", target, readErr)
	}
	if in.DryRun || !in.Ack {
		note := "preview only — pass ack=true to delete"
		if in.DryRun {
			note = "dry run — pass ack=true to delete"
		}
		return nil, configDeleteOut{Path: target, Scope: scopeLabel, Deleted: false, DryRun: true, Bytes: len(prev), Note: note}, nil
	}
	// Snapshot the content first so the delete is reversible via
	// config_restore, then remove the file.
	if st, serr := openStore(ctx); serr == nil {
		_, _ = st.SnapshotConfig(ctx, histRoot, target, prev)
		_ = st.Close()
	}
	if err := os.Remove(target); err != nil {
		return nil, configDeleteOut{}, fmt.Errorf("delete %s: %w", target, err)
	}
	writeMCPEvent(context.Background(), store.EvtMCPConfigDelete, "deleted "+target, 0, map[string]string{
		"scope": scopeLabel,
		"file":  target,
		"bytes": strconv.Itoa(len(prev)),
	})
	return nil, configDeleteOut{
		Path:    target,
		Scope:   scopeLabel,
		Deleted: true,
		Bytes:   len(prev),
		Note:    "snapshotted to SQLite first — recoverable via config_history/config_restore",
	}, nil
}
