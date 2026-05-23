// Command shuck shucks the husk and keeps the kernel: it returns the exact
// failing CI step logs for a pull request.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/mcp"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "mcp" {
		if err := mcp.Serve(context.Background(), args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "shuck:", err)
			os.Exit(2)
		}
		return
	}
	os.Exit(cli.Run(args, os.Stdout, os.Stderr))
}
