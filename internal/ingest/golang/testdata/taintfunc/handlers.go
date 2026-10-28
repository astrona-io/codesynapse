// Package taintfunc passes request input through function values.
package taintfunc

import (
	"database/sql"
	"net/http"
	"os/exec"
	"strings"
)

var db *sql.DB

func runCommand(name string) { _ = exec.Command(name).Run() }

func findUser(name string) {
	_, _ = db.Query("SELECT id FROM users WHERE name = '" + name + "'")
}

func logName(name string) { _ = strings.ToUpper(name) }

// apply calls whatever it is given with v.
func apply(f func(string), v string) { f(v) }

type service struct{ table string }

func (s *service) Delete(id string) {
	_, _ = db.Exec("DELETE FROM " + s.table + " WHERE id = " + id)
}

type hooks struct{ onName func(string) }

// listeners is a separate type so that its field holds only logName.
type listeners struct{ onName func(string) }

// ThroughClosure passes request input to a closure that runs it.
func ThroughClosure(w http.ResponseWriter, r *http.Request) {
	run := func(prog string) { _ = exec.Command(prog).Run() }
	run(r.FormValue("prog"))
}

// ThroughParameter hands runCommand to apply with request input.
func ThroughParameter(w http.ResponseWriter, r *http.Request) {
	apply(runCommand, r.FormValue("cmd"))
}

// ThroughField calls findUser through a struct field.
func ThroughField(w http.ResponseWriter, r *http.Request) {
	h := hooks{onName: findUser}
	h.onName(r.FormValue("name"))
}

// ThroughMethodValue calls a method value with request input.
func ThroughMethodValue(w http.ResponseWriter, r *http.Request) {
	s := &service{table: "items"}
	del := s.Delete
	del(r.PathValue("id"))
}

// CleanThroughClosure passes a constant through the same kind of closure.
func CleanThroughClosure() {
	run := func(prog string) { _ = exec.Command(prog).Run() }
	run("true")
}

// CleanThroughParameter passes a constant to apply with a sink behind it.
func CleanThroughParameter() { apply(runCommand, "date") }

// CleanTarget passes request input to function values that reach no sink.
func CleanTarget(w http.ResponseWriter, r *http.Request) {
	l := listeners{onName: logName}
	l.onName(r.FormValue("name"))
	f := logName
	f(r.FormValue("other"))
}
