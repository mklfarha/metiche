package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/view"
)

// The public docs (internal/view/docs.go).
//
//	GET /docs          the index
//	GET /docs/{page}   one page; an unknown page is the router's plain 404
//
// The pages are the same bytes for every visitor: no viewer, no team, no
// cookie read or written. So they are cacheable, and they carry a CSP that
// allows no script at all, because they have none.

// docsCSP allows this origin's stylesheets and images and nothing else.
const docsCSP = "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

func docsHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Security-Policy", docsCSP)
	h.Set("X-Content-Type-Options", "nosniff")
}

func (s *Server) docsIndex(w http.ResponseWriter, r *http.Request) {
	docsHeaders(w)
	s.render(w, r, view.DocsIndex())
}

// docsSlash sends /docs/ to /docs, the one spelling of the index.
func (s *Server) docsSlash(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/docs", http.StatusMovedPermanently)
}

func (s *Server) docPage(w http.ResponseWriter, r *http.Request) {
	p, ok := view.LookupDoc(chi.URLParam(r, "page"))
	if !ok {
		// The same answer as any path this site does not serve.
		http.NotFound(w, r)
		return
	}
	docsHeaders(w)
	s.render(w, r, view.DocsPage(p))
}
