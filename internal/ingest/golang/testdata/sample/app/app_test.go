package app

import (
	"testing"

	"example.com/sample/store"
)

func TestRun(t *testing.T) {
	if err := Run(&store.Disk{}); err != nil {
		t.Fatal(err)
	}
}
