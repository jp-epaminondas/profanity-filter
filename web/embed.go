// Package web serves the playground: a single page where anyone can type a
// text and watch the filter censor it live, with the processing time placed
// on a latency scale. The files are embedded, so the binary stays the only
// thing needed to run the project.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html static
var files embed.FS

// contentSecurityPolicy allows only same-origin scripts, styles and API
// calls. Keeping JS and CSS in separate files (no inline code) is what makes
// this strict policy possible.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Page serves the playground HTML.
func Page() http.Handler {
	page, err := files.ReadFile("index.html")
	if err != nil {
		panic(err) // the file is embedded at build time
	}
	return secure(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
}

// Static serves the playground assets under /static/.
func Static() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return secure(http.StripPrefix("/static/", http.FileServerFS(sub)))
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
