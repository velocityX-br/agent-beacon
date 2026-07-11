// Package web embeds the dashboard single-page app so the server ships as a
// single self-contained binary. The files under web/static are our own
// original UI (vanilla JS + xterm.js loaded from CDN).
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS returns the SPA file system rooted at the static directory.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // embedded path is a compile-time constant
	}
	return sub
}
