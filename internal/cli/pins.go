package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/pins"
)

const pinsUsage = `shuck pins — find the GitHub Actions in your workflows that are not SHA-pinned, or whose pin has gone stale.

A workflow that says "uses: actions/checkout@v4" runs whatever commit that tag
points at today. Pinning to a SHA fixes that, but a pin left alone drifts
behind the releases it was taken from — so both halves matter, and shuck
reports both: references that are still on a mutable tag, and pins whose
"# v4.2.2" comment names a release that has since been superseded.

Every finding comes with the line to paste.

Usage:
  shuck pins [dir]      audit a checkout's .github/workflows (default: the current directory)

Auth is optional for public actions: set GITHUB_TOKEN/GH_TOKEN or pass --token
to lift the unauthenticated rate limit. Tag lists are cached under ~/.cache/shuck
for an hour.

Flags:
`

// runPins implements `shuck pins [dir]`.
func runPins(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck pins", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, pinsUsage)
		fs.PrintDefaults()
	}
	var (
		jsonOut  bool
		exitCode bool
		refresh  bool
		offline  bool
		token    string
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON (stable schema) instead of text")
	fs.BoolVar(&exitCode, "exit-code", false, "exit 1 when an unpinned or stale reference is found (for CI gating)")
	fs.BoolVar(&refresh, "refresh", false, "ignore and rebuild the cached tag lists")
	fs.BoolVar(&offline, "offline", false, "list the references without resolving their latest releases")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "shuck: too many arguments (expected: shuck pins [dir])")
		return 2
	}

	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	report, err := Pins(context.Background(), root, PinsOptions{
		Token:   token,
		Refresh: refresh,
		Offline: offline,
	})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if jsonOut {
		if err := pins.EncodeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
	} else {
		pins.Render(stdout, report)
	}
	if exitCode && report.HasIssues() {
		return 1
	}
	return 0
}

// PinsOptions controls a pin audit. It is the front-end-agnostic input to
// [Pins], shared by the CLI and the MCP server.
type PinsOptions struct {
	// Token authenticates the tag lookups. Empty falls back to
	// GITHUB_TOKEN/GH_TOKEN, and then to unauthenticated requests — which work
	// for public actions, at a much lower rate limit.
	Token string
	// Refresh rebuilds the cached tag lists instead of reusing them.
	Refresh bool
	// Offline skips resolution entirely: the report then says which references
	// exist and which are unpinned, without a suggested fix.
	Offline bool
}

// Pins audits a checkout's workflow files and returns the report. It is the
// reusable core behind `shuck pins` and the check_pins MCP tool.
func Pins(ctx context.Context, root string, opts PinsOptions) (pins.Report, error) {
	if _, err := os.Stat(root); err != nil {
		return pins.Report{}, fmt.Errorf("read %s: %w", root, err)
	}
	if opts.Offline {
		return pins.Repository(ctx, root, nil)
	}

	// A token is genuinely optional here: resolving a public action's tags
	// works unauthenticated, just with far less headroom. So an absent token
	// is not an error, only a smaller budget.
	token := opts.Token
	if token == "" {
		token, _ = resolveToken("")
	}
	return pins.Repository(ctx, root, newPinResolver(token, opts.Refresh))
}

// newPinResolver builds the resolver the pin audit uses. It is also what the
// background monitor is handed, so a pin suggestion is identical wherever it
// comes from.
func newPinResolver(token string, refresh bool) pins.Resolver {
	return &cachedTagResolver{token: token, refresh: refresh, seen: map[string][]model.ActionTag{}}
}

// cachedTagResolver resolves action references for the pin audit, reusing the
// same on-disk tag cache `shuck action` fills.
type cachedTagResolver struct {
	token   string
	refresh bool
	seen    map[string][]model.ActionTag
}

// Resolve implements pins.Resolver.
func (r *cachedTagResolver) Resolve(ctx context.Context, ref action.Ref) (action.Resolved, error) {
	tags, err := r.tags(ctx, ref)
	if err != nil {
		return action.Resolved{}, err
	}
	tag, err := action.Select(tags, ref.Constraint)
	if err != nil {
		return action.Resolved{}, fmt.Errorf("resolve %s: %w", ref.Slug(), err)
	}
	return action.Resolved{Ref: ref, Tag: tag.Name, SHA: tag.SHA}, nil
}

// tags fetches an action repository's tags, memoized for this audit on top of
// the TTL'd on-disk cache `shuck action` fills. A repository's workflows
// reference the same handful of actions over and over, so the in-process map
// matters as much as the disk cache: one audit resolves each action once.
func (r *cachedTagResolver) tags(ctx context.Context, ref action.Ref) ([]model.ActionTag, error) {
	slug := ref.RepoSlug()
	if tags, ok := r.seen[slug]; ok {
		return tags, nil
	}
	owner, repo, _ := strings.Cut(slug, "/")
	// loadOrFetchTags already names the repository in its error, so wrapping
	// again here would only stutter.
	tags, err := loadOrFetchTags(ctx, owner, repo, r.token, r.refresh)
	if err != nil {
		return nil, err
	}
	r.seen[slug] = tags
	return tags, nil
}
