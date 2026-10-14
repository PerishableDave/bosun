//go:build !darwin && !linux

package doctor

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("free space check is not supported on this platform")
}
