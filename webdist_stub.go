//go:build !embed_web

package webdist

import "io/fs"

func Embedded() fs.FS { return nil }
