// Command shuck shucks the husk and keeps the kernel: it returns the exact
// failing CI step logs for a pull request.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	"github.com/justanotherspy/shuck/internal/cli"
	"github.com/justanotherspy/shuck/internal/mcp"
	"github.com/justanotherspy/shuck/internal/setup"
)

// shuckSkill is the canonical SKILL.md, embedded so `shuck setup` can install it
// into the user's Claude config without the plugin marketplace. It is the same
// file the plugin ships, so the two stay in sync.
//
//go:embed plugins/shuck/skills/shuck/SKILL.md
var shuckSkill string

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			if err := mcp.Serve(context.Background(), args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "shuck:", err)
				os.Exit(2)
			}
			return
		case "setup":
			os.Exit(setup.Run(args[1:], shuckSkill, os.Stdin, os.Stdout, os.Stderr))
		}
	}
	os.Exit(cli.Run(args, os.Stdout, os.Stderr))
}
