package taint

import (
	"net/http"
	"os"
)

// Blobs is implemented twice; only one implementation reaches a sink.
type Blobs interface {
	Fetch(name string) ([]byte, error)
}

type diskBlobs struct{ root string }

func (d diskBlobs) Fetch(name string) ([]byte, error) { return os.ReadFile(d.root + "/" + name) }

type memBlobs map[string][]byte

func (m memBlobs) Fetch(name string) ([]byte, error) { return m[name], nil }

var blobs Blobs = diskBlobs{root: "/blobs"}

func FetchThroughInterface(w http.ResponseWriter, r *http.Request) {
	_, _ = blobs.Fetch(r.PathValue("name"))
}
