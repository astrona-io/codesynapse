// Package broken has a type error; the rest must still be analysed.
package broken

func ok() {}

// Broken calls an undefined function and a defined one.
func Broken() {
	undefined()
	ok()
}
