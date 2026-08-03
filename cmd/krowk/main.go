// Command krowk uploads agent output to the krowk registry and prints a
// permalink that unfurls wherever the team is already talking.
package main

import (
	"os"

	"github.com/krowkcom/cli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, isTerminal(os.Stdout)))
}

// isTerminal decides whether to colour output and default to the human format.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
