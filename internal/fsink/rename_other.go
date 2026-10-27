//go:build !linux

package fsink

import (
	"errors"
	"fmt"
	"os"
)

func renameNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// Recover a prior link+unlink attempt that stopped after creating dst.
		srcInfo, srcErr := os.Stat(src)
		dstInfo, dstErr := os.Stat(dst)
		if srcErr == nil && dstErr == nil && os.SameFile(srcInfo, dstInfo) {
			return os.Remove(src)
		}
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("removing source after linking destination: %w", err)
	}
	return nil
}
