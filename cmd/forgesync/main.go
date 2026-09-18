// Command forgesync is the command-line client for the ForgeSync controller's
// admin API.
package main

import (
	"os"

	"scenegit.org/forgesync/internal/cli"
)

func main() {
	if err := cli.NewRootCommand(os.Stdout).Execute(); err != nil {
		os.Exit(1)
	}
}
