// Package webui serves the single-file static frontend embedded into the
// apiserver binary, so deployment stays a single Go binary with no separate
// build step (Render free tier constraint — see README).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFiles embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
