//go:build !linux

package wt

import (
	"errors"
	"os"
)

// cloneFile is the non-Linux stub: no reflink ioctl is available, so
// always report unsupported and let the caller take the io.Copy path.
func cloneFile(_, _ *os.File) error {
	return errors.ErrUnsupported
}
