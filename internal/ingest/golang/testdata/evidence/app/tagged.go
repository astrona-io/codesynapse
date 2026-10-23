//go:build evidence_never

package app

// Tagged is excluded by its build tag and must not be in the graph.
func Tagged() { helper() }
