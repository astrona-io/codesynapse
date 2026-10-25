package taint

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readUpload is reached with request input from DownloadThroughHelper.
func readUpload(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("/uploads", name))
}

func DownloadThroughHelper(w http.ResponseWriter, r *http.Request) {
	_, _ = readUpload(r.FormValue("name"))
}

// Two levels down.
func lookup(name string) { query("name = '" + name + "'") }

func query(where string) { _, _ = db.Query("SELECT id FROM users WHERE " + where) }

func SearchTwoLevels(w http.ResponseWriter, r *http.Request) {
	lookup(r.URL.Query().Get("name"))
}

// normalise hands its input back: the result is still request input.
func normalise(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func FetchNormalised(w http.ResponseWriter, r *http.Request) {
	u := normalise(r.FormValue("u"))
	_, _ = http.Get(u)
}

// parseID returns a number: nothing of the input is left.
func parseID(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func ConcatOfParsedID(w http.ResponseWriter, r *http.Request) {
	id := parseID(r.FormValue("id"))
	_, _ = db.Query(fmt.Sprintf("SELECT name FROM users WHERE id = %d", id))
}

// readStatic is only ever called with constants.
func readStatic(name string) ([]byte, error) { return os.ReadFile(name) }

func CleanCaller(w http.ResponseWriter, r *http.Request) {
	_, _ = readStatic("/etc/app.conf")
}

// walkTree calls itself; the analysis must still end.
func walkTree(p string, depth int) {
	if depth > 0 {
		walkTree(p, depth-1)
	}
	_, _ = os.Open(p)
}

func RecursiveHelper(w http.ResponseWriter, r *http.Request) {
	walkTree(r.PathValue("dir"), 3)
}
