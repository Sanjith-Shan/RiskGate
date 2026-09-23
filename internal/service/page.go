package service

import (
	_ "embed"
	"net/http"
)

// The rule-authoring page: one static HTML file, no framework and no build
// step, embedded so the binary is the whole deployment. It talks to the
// same JSON API anyone else would.
//
//go:embed web/index.html
var pageHTML []byte

func (s *Service) handlePage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	// Everything the page needs is inline or same-origin.
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
	_, _ = w.Write(pageHTML)
}
