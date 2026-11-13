//go:build unix

package patcher

import (
	"fmt"
	"os"
	"syscall"
)

func indexInode(path string) (uint32, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, fmt.Errorf("stat %s: no inode in %T", path, fi.Sys())
	}
	return uint32(st.Ino), true, nil //nolint:gosec // git's index stores the low 32 bits of st_ino
}
