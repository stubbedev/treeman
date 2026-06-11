//go:build linux

package wt

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile attempts a kernel-side reflink (FICLONE) of src's data into
// dst — a constant-time copy-on-write clone on filesystems that support
// it (btrfs, XFS with reflink=1, bcachefs). Callers fall back to a
// byte-for-byte io.Copy when this returns an error (EOPNOTSUPP on ext4,
// EXDEV across filesystems), so failure here is expected and cheap.
func cloneFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
