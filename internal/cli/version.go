package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/release"
)

// newReleaseClient builds the client used by `version --check` and `upgrade`. It
// is a package var so tests can point it at a local server.
var newReleaseClient = release.New

const (
	// versionCheckFile holds the cached result of the last update check, under
	// the shuck base dir (~/.cache/shuck).
	versionCheckFile = "version-check.json"
	// versionCheckTTL bounds how long a cached check is fresh enough for bare
	// `shuck version` to surface an "update available" hint offline.
	versionCheckTTL = 24 * time.Hour
)

// versionCheck records the newest release tag seen by the last `version --check`
// (or `upgrade`) and when it was observed.
type versionCheck struct {
	Latest    string    `json:"latest"`
	CheckedAt time.Time `json:"checked_at"`
}

// runVersion implements `shuck version [--check]`.
//
// Without --check it prints the running version and, if a recent check is
// cached, an "update available" hint — all offline. With --check it queries
// GitHub for the latest release, refreshes the cache, and reports whether an
// upgrade is available.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var check bool
	fs.BoolVar(&check, "check", false, "query GitHub for the latest release and report whether an upgrade is available")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "shuck version [--check] — print the installed version; with --check, report whether a newer release exists.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cur := versionString()
	fmt.Fprintln(stdout, "shuck", cur)

	if !check {
		if vc, ok := loadVersionCheck(); ok && time.Since(vc.CheckedAt) < versionCheckTTL {
			if release.IsSemver(cur) && release.Compare(vc.Latest, cur) > 0 {
				fmt.Fprintf(stdout, "update available: %s (run: shuck upgrade)\n", vc.Latest)
			}
		}
		return 0
	}

	latest, err := newReleaseClient(tokenFromEnv()).Latest(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "shuck: could not check for updates:", err)
		return 2
	}
	saveVersionCheck(latest)

	switch {
	case !release.IsSemver(cur):
		fmt.Fprintf(stdout, "latest release: %s (installed version %q is not comparable; run `shuck upgrade` to install the latest)\n", latest, cur)
	case release.Compare(latest, cur) > 0:
		fmt.Fprintf(stdout, "update available: %s (run: shuck upgrade)\n", latest)
	default:
		fmt.Fprintln(stdout, "up to date")
	}
	return 0
}

// tokenFromEnv returns a GitHub token from the environment if present. Unlike
// resolveToken it does not error when none is set: release checks work
// unauthenticated, subject to GitHub's lower anonymous rate limit.
func tokenFromEnv() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func versionCheckPath() (string, error) {
	base, err := cache.Base()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, versionCheckFile), nil
}

// loadVersionCheck reads the cached update check; a missing or unreadable record
// returns ok=false.
func loadVersionCheck() (versionCheck, bool) {
	path, err := versionCheckPath()
	if err != nil {
		return versionCheck{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return versionCheck{}, false
	}
	var vc versionCheck
	if err := json.Unmarshal(data, &vc); err != nil || vc.Latest == "" {
		return versionCheck{}, false
	}
	return vc, true
}

// saveVersionCheck records the latest tag and the current time. It is
// best-effort: a write failure is silently ignored, since the cache is only an
// optimization for the offline hint.
func saveVersionCheck(latest string) {
	path, err := versionCheckPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(versionCheck{Latest: latest, CheckedAt: time.Now()})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644) //nolint:gosec // version-check cache marker; 0644 is fine
}
