// Package web — веб-интерфейс: статика, вшитая в бинарник (без сборки и node).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

func Handler() http.Handler {
	sub, _ := fs.Sub(files, "static")
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Оболочка всегда свежая; данные приходят из API.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		fileServer.ServeHTTP(w, r)
	})
}
