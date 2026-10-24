package app

import (
	"net/http"

	chi "github.com/go-chi/chi/v5"
)

func requireAuth(next http.Handler) http.Handler { return next }
func logger(next http.Handler) http.Handler      { return next }
func adminOnly(next http.Handler) http.Handler   { return next }

func health(http.ResponseWriter, *http.Request)  {}
func items(http.ResponseWriter, *http.Request)   {}
func stats(http.ResponseWriter, *http.Request)   {}
func users(http.ResponseWriter, *http.Request)   {}
func me(http.ResponseWriter, *http.Request)      {}
func open(http.ResponseWriter, *http.Request)    {}
func things(http.ResponseWriter, *http.Request)  {}
func wrapped(http.ResponseWriter, *http.Request) {}

// Chi: Use counts from where it is called; Route nests prefix and guards.
func Chi() http.Handler {
	r := chi.NewRouter()
	r.Get("/health", health)
	r.Use(logger, requireAuth)
	r.Get("/items", items)
	r.Route("/admin", func(r chi.Router) {
		r.Use(adminOnly)
		r.Get("/stats", stats)
	})
	r.With(adminOnly).Get("/users", users)
	return r
}

// Groups with a prefix and middleware, as gin and echo make them.
func Groups() {
	r := chi.NewRouter()
	api := r.Group("/api", requireAuth)
	api.Get("/me", me)
	pub := r.Group("/public")
	pub.Get("/open", open)
}

// Subrouters, as gorilla/mux makes them.
func Subrouters() {
	r := chi.NewRouter()
	s := r.PathPrefix("/v1").Subrouter()
	s.Use(requireAuth)
	s.HandleFunc("/things", things)
}

// A handler wrapped around the whole mux guards every route on it.
func Std() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wrapped", wrapped)
	_ = http.ListenAndServe(":0", requireAuth(mux))
}
