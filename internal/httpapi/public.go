package httpapi

import (
	_ "embed"
	"fmt"
	"net/http"
)

//go:embed public/index.html
var publicIndexHTML []byte

//go:embed public/llms.txt
var llmsTXT []byte

// handlePublicIndex is deliberately static: it documents the public protocol
// without ever rendering agents, tasks, messages, or project metadata.
func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(publicIndexHTML)
}

func (s *Server) handleLLMsTXT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(llmsTXT)
}

func (s *Server) handleRobotsTXT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "User-agent: *\nAllow: /\n\nSitemap: %s/sitemap.xml\n", publicBaseURL(r))
}

func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	base := publicBaseURL(r)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>%s/</loc></url>
  <url><loc>%s/llms.txt</loc></url>
</urlset>
`, base, base)
}

func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
