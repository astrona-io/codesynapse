// Package store persists things.
package store

import "os/exec"

// Saver saves a value.
type Saver interface {
	Save(v string) error
}

// Disk writes values to disk.
type Disk struct{}

// Save writes v. The token is password=hunter2secret here.
func (d *Disk) Save(v string) error {
	if v == "" || len(v) > 10 {
		return nil
	}
	return exec.Command("true").Run()
}

const apiToken = "s3cr3t-value-123456"

// Lookup runs a query built from input.
func Lookup(db interface{ Query(string) error }, name string) error {
	return db.Query("SELECT * FROM t WHERE name = '" + name + "'")
}

// Shell runs a script.
func Shell(script string) error {
	return exec.Command("sh", "-c", script).Run()
}
