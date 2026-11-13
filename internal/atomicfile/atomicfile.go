// Package atomicfile replaces files so that a reader, a crash or a full
// disk sees either the old content or the new, never a truncated mix.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Write replaces the file at path with data. A symlink at path is
// followed and its target replaced, so the link itself survives; a
// dangling symlink is an error. An existing file keeps its permission
// bits; a new one gets perm. The data goes to a uniquely named temp
// file beside the target, is synced, and is renamed into place.
func Write(path string, data []byte, perm fs.FileMode) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(target); err == nil {
		perm = fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", target, err)
	}
	if err := fill(tmp, data, perm); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// Refresh moves the file at path onto a fresh inode holding the same
// bytes, permission bits and modification time, following a symlink to
// its target like Write. The modification time is set before the
// rename, so no reader ever sees the new inode with a different one.
func Refresh(path string) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	src, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	defer func() { _ = src.Close() }()
	fi, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", target, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", target, err)
	}
	err = fill(tmp, data, fi.Mode().Perm())
	if err == nil {
		err = os.Chtimes(tmp.Name(), time.Time{}, fi.ModTime())
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

func resolve(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if err == nil {
		return target, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("resolve %s: dangling symlink: %w", path, err)
	}
	return path, nil
}

func fill(f *os.File, data []byte, perm fs.FileMode) error {
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(perm)
	}
	if werr == nil {
		werr = f.Sync()
	}
	return errors.Join(werr, f.Close())
}
