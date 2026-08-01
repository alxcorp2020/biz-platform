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

// IndexHTML — 기술 스펙 화면(#/admin/tech-spec)이 프론트엔드가 실제로 쓰는
// 외부 리소스(CDN 스크립트/스타일시트)를 정적 파일에서 직접 스캔하기 위해
// 내장된 index.html 원문을 읽는다.
func IndexHTML() ([]byte, error) {
	return staticFiles.ReadFile("static/index.html")
}
