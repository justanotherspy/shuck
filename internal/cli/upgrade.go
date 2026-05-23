package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"

	"github.com/justanotherspy/shuck/internal/release"
)

// runUpgrade implements `shuck upgrade`: it always queries GitHub for the latest
// release (never the cache) and replaces the running binary in place, keeping it
// wherever install.sh put it. If shuck was installed by another tool (e.g.
// `go install`), it cannot safely overwrite the binary and tells the user to
// upgrade with that tool instead.
func runUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "shuck upgrade — download and install the latest shuck release in place.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "shuck: upgrade takes no arguments, got %q\n", fs.Arg(0))
		return 2
	}

	exe, err := release.ExecutablePath()
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if tool, external := release.ManagedExternally(exe); external {
		fmt.Fprintf(stdout, "shuck looks installed via %s; upgrade it with:\n  go install github.com/justanotherspy/shuck@latest\n", tool)
		return 0
	}

	ctx := context.Background()
	rc := newReleaseClient(tokenFromEnv())
	latest, err := rc.Latest(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "shuck: could not resolve the latest release:", err)
		return 2
	}

	cur := versionString()
	if release.IsSemver(cur) && release.Compare(latest, cur) <= 0 {
		fmt.Fprintf(stdout, "shuck is already up to date (%s)\n", cur)
		saveVersionCheck(latest)
		return 0
	}

	fmt.Fprintf(stdout, "downloading shuck %s for %s/%s ...\n", latest, runtime.GOOS, runtime.GOARCH)
	bin, err := rc.Download(ctx, latest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fmt.Fprintln(stderr, "shuck: download failed:", err)
		return 2
	}
	if err := release.ReplaceRunning(exe, bin); err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	saveVersionCheck(latest)
	fmt.Fprintf(stdout, "upgraded shuck %s -> %s (%s)\n", cur, latest, exe)
	return 0
}
