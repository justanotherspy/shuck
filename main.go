// Command shuck shucks the husk and keeps the kernel: it returns the exact
// failing CI step logs for a pull request.
package main

import (
	"os"

	"github.com/justanotherspy/shuck/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
