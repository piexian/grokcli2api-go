//go:build embed_web

package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend/dist
var embedded embed.FS

func Embedded() fs.FS {
	dist, err := fs.Sub(embedded, "frontend/dist")
	if err != nil {
		return nil
	}
	return dist
}
