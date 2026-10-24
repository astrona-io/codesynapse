package app

import (
	"io/fs"
	"net/http"
	"os/exec"
	"path/filepath"
)

type Server struct{ mux *http.ServeMux }

// route is the repository's own registration wrapper.
func (s *Server) route(pattern, policy string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }

func (s *Server) Routes() {
	s.route("GET /items", "viewer", s.listItems)
	s.route("POST /run", "admin", s.withAudit(s.run))
	s.mux.HandleFunc("/open", func(http.ResponseWriter, *http.Request) {})
	s.mux.Handle("GET /self", s)
	s.mux.Handle("GET /made", s.factory())
	_ = filepath.Walk("/tmp", func(string, fs.FileInfo, error) error { return nil })
}

func (s *Server) listItems(http.ResponseWriter, *http.Request) {}

func (s *Server) run(http.ResponseWriter, *http.Request) { _ = exec.Command("true").Run() }

func (s *Server) withAudit(h http.HandlerFunc) http.HandlerFunc { return h }

func (s *Server) ServeHTTP(http.ResponseWriter, *http.Request) {}

func (s *Server) factory() http.Handler { return s }
