//go:build unix

package envfile

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func checkOwner(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("owned by uid %d, not the user running bosund (uid %d)", st.Uid, uid)
	}
	return nil
}
