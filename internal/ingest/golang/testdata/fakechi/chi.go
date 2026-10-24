// Package chi is a stand-in with the shapes of chi, gin, echo and
// gorilla/mux routers, so the fixture builds without the network.
package chi

import "net/http"

type Middleware = func(http.Handler) http.Handler

type Router interface {
	http.Handler
	Use(mw ...Middleware)
	Get(pattern string, h http.HandlerFunc)
	Handle(pattern string, h http.Handler)
	Route(pattern string, fn func(r Router)) Router
}

type Mux struct{}

func NewRouter() *Mux { return &Mux{} }

func (m *Mux) ServeHTTP(http.ResponseWriter, *http.Request)   {}
func (m *Mux) Use(mw ...Middleware)                           {}
func (m *Mux) Get(pattern string, h http.HandlerFunc)         {}
func (m *Mux) Handle(pattern string, h http.Handler)          {}
func (m *Mux) HandleFunc(pattern string, h http.HandlerFunc)  {}
func (m *Mux) Route(pattern string, fn func(r Router)) Router { return m }
func (m *Mux) With(mw ...Middleware) *Mux                     { return m }
func (m *Mux) Group(prefix string, mw ...Middleware) *Mux     { return m }
func (m *Mux) PathPrefix(prefix string) *Mux                  { return m }
func (m *Mux) Subrouter() *Mux                                { return m }
