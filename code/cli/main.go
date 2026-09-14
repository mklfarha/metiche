// Command metiche is a person's view of metiche: status, teams, invites, the
// .metiche binding and a doctor. See docs/CLI.md.
package main

import (
	"os"

	"github.com/mklfarha/metiche/cli/internal/cmd"
)

func main() {
	os.Exit(cmd.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
