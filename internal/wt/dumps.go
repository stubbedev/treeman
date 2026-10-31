package wt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
)

func LinkPaths(ctx context.Context, cfg *config.Config, repoRoot, wtPath string) ([]string, error) {
	dumps, err := ignoredDumpLinks(ctx, cfg.Databases, repoRoot, wtPath)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(cfg.Worktrees.Links)
	for _, p := range dumps {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

type dumpRef struct {
	db, dump int
	spec     config.DumpSpec
}

func ignoredDumpLinks(ctx context.Context, dbs []config.DatabaseConfig, repoRoot, wtPath string) ([]string, error) {
	var refs []dumpRef
	for i, d := range dbs {
		for j, ds := range d.Dump {
			refs = append(refs, dumpRef{db: i, dump: j, spec: ds})
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.spec.Path
	}
	ignored, err := checkIgnored(ctx, wtPath, paths)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if _, ok := ignored[r.spec.Path]; !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(wtPath, r.spec.Path)); err == nil {
			continue
		}
		src := filepath.Join(repoRoot, r.spec.Path)
		if _, err := os.Stat(src); err != nil {
			if r.spec.Optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf(
				"databases[%d].dump[%d] %s is gitignored, so worktrees never check it out, and the main checkout has no copy to link: %w",
				r.db, r.dump, r.spec.Path, err,
			)
		}
		if !slices.Contains(out, r.spec.Path) {
			out = append(out, r.spec.Path)
		}
	}
	return out, nil
}

func checkIgnored(ctx context.Context, wtPath string, paths []string) (map[string]struct{}, error) {
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	b, err := gitcmd.OutputStdin(ctx, wtPath, &in, "check-ignore", "-z", "--stdin")
	if gerr, ok := errors.AsType[*gitcmd.Error](err); ok && gerr.ExitCode == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check which dump paths are gitignored in %s: %w", wtPath, err)
	}
	out := map[string]struct{}{}
	for p := range bytes.SplitSeq(bytes.TrimSuffix(b, []byte{0}), []byte{0}) {
		out[string(p)] = struct{}{}
	}
	return out, nil
}
