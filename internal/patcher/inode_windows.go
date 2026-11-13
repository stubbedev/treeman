//go:build windows

package patcher

import "os"

func indexInode(path string) (uint32, bool, error) {
	_, err := os.Stat(path)
	return 0, false, err
}
