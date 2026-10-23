// Package app is the labelled evidence fixture: every resolution edge the
// extractor should find is listed in evidence_test.go.
package app

// Store is satisfied by disk.
type Store interface{ Save(string) error }

type disk struct{}

func (disk) Save(string) error { return nil }

// Map is generic; its call through f cannot be resolved.
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

func double(x int) int { return x * 2 }

func helper() {}

// Run mixes static, generic, interface and dynamic calls.
func Run(s Store) {
	_ = s.Save("x")
	_ = Map([]int{1}, double)
	fn := double
	_ = fn(2)
	func() {}()
	_ = len("x")
	_ = int64(3)
	helper()
}
