// Command shuck shucks the husk and keeps the kernel: it returns the exact
// failing CI step logs for a pull request.
package main

import (
	_ "embed"
	"os"

	"github.com/justanotherspy/shuck/internal/cli"
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
	if len(args) > 0 && args[0] == "setup" {
		os.Exit(setup.Run(args[1:], shuckSkill, os.Stdout, os.Stderr))
	}
	os.Exit(cli.Run(args, os.Stdout, os.Stderr))
}
