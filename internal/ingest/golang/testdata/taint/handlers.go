// Package taint holds request handlers for the taint tests: each function
// is one case, named after what it should report.
package taint

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

var db *sql.DB

func SQLThroughVariable(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("q")
	where := "name = '" + q + "'"
	_, _ = db.Query("SELECT id FROM users WHERE " + where)
}

func CleanSQLParameter(w http.ResponseWriter, r *http.Request) {
	_, _ = db.Query("SELECT id FROM users WHERE name = $1", r.FormValue("q"))
}

func ConcatOfANumber(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.FormValue("n"))
	_, _ = db.Query(fmt.Sprintf("SELECT id FROM users LIMIT %d", n))
}

func ConcatAfterReassign(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("q")
	q = "fixed"
	_, _ = db.Query("SELECT id FROM users WHERE name = '" + q + "'")
}

func PathThroughJoin(w http.ResponseWriter, r *http.Request) {
	p := filepath.Join("/data", r.URL.Query().Get("file"))
	_, _ = os.ReadFile(p)
}

func CleanPathBase(w http.ResponseWriter, r *http.Request) {
	p := filepath.Join("/data", filepath.Base(r.FormValue("file")))
	_, _ = os.ReadFile(p)
}

func SSRFThroughParse(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(r.FormValue("u"))
	if err != nil {
		return
	}
	_, _ = http.Get(u.String())
}

func SSRFThroughJSONBody(w http.ResponseWriter, r *http.Request) {
	var req struct{ URL string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return
	}
	out, _ := http.NewRequest(http.MethodGet, req.URL, nil)
	_ = out
}

func ExecArgument(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	_ = exec.Command("git", "log", ref).Run()
}

func ExecThroughRange(w http.ResponseWriter, r *http.Request) {
	for _, f := range r.Form["files"] {
		_ = exec.Command("cat", f).Run()
	}
}

func CleanExec(w http.ResponseWriter, r *http.Request) {
	_ = r.FormValue("ignored")
	_ = exec.Command("git", "status").Run()
}

// A URL from configuration is not request input.
func CleanConfiguredURL(w http.ResponseWriter, r *http.Request) {
	u, _ := url.Parse("https://example.com/api")
	_, _ = http.Get(u.Path)
}

func SSRFFromHeader(w http.ResponseWriter, r *http.Request) {
	_, _ = http.Get(r.Header.Get("X-Callback"))
}

func PathFromQuery(w http.ResponseWriter, r *http.Request) {
	_, _ = os.ReadFile(r.URL.Query().Get("f"))
}
