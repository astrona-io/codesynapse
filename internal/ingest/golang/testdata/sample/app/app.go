// Package app wires things together.
package app

import "example.com/sample/store"

// Run saves through the interface.
func Run(s store.Saver) error {
	return helper(s)
}

func helper(s store.Saver) error {
	return s.Save("x")
}
