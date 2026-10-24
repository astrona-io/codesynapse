// Package cobra stands in for github.com/spf13/cobra.
package cobra

// Command is a command line command.
type Command struct {
	Use  string
	Run  func(*Command, []string)
	RunE func(*Command, []string) error
}
