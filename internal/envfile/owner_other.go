//go:build !unix

package envfile

import "io/fs"

func checkOwner(fs.FileInfo) error { return nil }
