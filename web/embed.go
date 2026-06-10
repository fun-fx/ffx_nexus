// Package web embeds the built dashboard SPA (Vite output in dist/) so the
// console server can serve it directly from the Go binary. Rebuild the assets
// with `npm --prefix web run build` before compiling the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA file system rooted at the dist directory. It
// returns an error only if the embed is malformed (i.e. a build/packaging bug).
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
