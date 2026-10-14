//go:build darwin || linux

package doctor

import (
	"fmt"
	"syscall"
)

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // field types differ by OS
}
