package wt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BringInFiles brings each entry in `paths` from repoRoot into
// wtPath via either symlink (mode="link") or recursive copy
// (mode="copy"). Glob meta-characters (`*?[`) expand against
// repoRoot. Idempotent — if the destination already exists the
// entry is skipped. Missing non-glob sources are reported via the
// sink as warnings; missing glob expansions are silent.
func BringInFiles(repoRoot, wtPath string, paths []string, mode string, sink Sink) error {
	if sink == nil {
		sink = NoopSink{}
	}
	for _, rel := range paths {
		var matches []string
		if strings.ContainsAny(rel, "*?[") {
			m, _ := filepath.Glob(filepath.Join(repoRoot, rel))
			matches = m
		} else {
			matches = []string{filepath.Join(repoRoot, rel)}
		}
		if len(matches) == 0 {
			continue
		}
		for _, src := range matches {
			info, err := os.Stat(src)
			if err != nil {
				if !strings.ContainsAny(rel, "*?[") {
					sink.Warn("%s source missing, skipping: %s", mode, src)
				}
				continue
			}
			relToRepo, err := filepath.Rel(repoRoot, src)
			if err != nil {
				relToRepo = filepath.Base(src)
			}
			dst := filepath.Join(wtPath, relToRepo)
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
			}
			switch mode {
			case "link":
				if err := os.Symlink(src, dst); err != nil {
					return fmt.Errorf("symlink %s → %s: %w", dst, src, err)
				}
			case "copy":
				if err := copyPath(src, dst, info); err != nil {
					return fmt.Errorf("copy %s → %s: %w", src, dst, err)
				}
			}
		}
	}
	return nil
}

// copyPath copies src → dst. Regular files are copied byte-for-byte
// with the source's mode preserved; directories are recursed;
// symlinks in the source tree are recreated as symlinks pointing at
// the same target.
func copyPath(src, dst string, info os.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case mode.IsDir():
		if err := os.MkdirAll(dst, mode.Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childSrc := filepath.Join(src, e.Name())
			childDst := filepath.Join(dst, e.Name())
			childInfo, err := os.Lstat(childSrc)
			if err != nil {
				return err
			}
			if err := copyPath(childSrc, childDst, childInfo); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		return copyRegularFile(src, dst, mode.Perm())
	default:
		return fmt.Errorf("unsupported file type for %s (mode=%v)", src, mode)
	}
}

func copyRegularFile(src, dst string, perm os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer df.Close()
	_, err = io.Copy(df, sf)
	return err
}

// PruneEmptyParents walks up from `start` removing now-empty
// directories until we leave `wtRoot` (the configured worktrees
// root). Best-effort: any rmdir error stops the walk.
func PruneEmptyParents(start, wtRoot string) {
	if start == "" || wtRoot == "" {
		return
	}
	parent := filepath.Dir(start)
	for {
		rel, err := filepath.Rel(wtRoot, parent)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		if err := os.Remove(parent); err != nil {
			return
		}
		parent = filepath.Dir(parent)
	}
}
