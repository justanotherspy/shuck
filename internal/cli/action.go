package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/justanotherspy/shuck/internal/action"
	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/model"
)

// actionCacheTTL bounds how long a repo's cached tag list is reused before
// shuck re-validates it. Within the window the cache is reused only when the
// repo's default-branch SHA is unchanged; past it shuck always re-fetches (a new
// release tag can appear without a new default-branch commit). --refresh forces
// a re-fetch immediately.
const actionCacheTTL = time.Hour

// TagLister is the slice of gh.Client the action command needs: the repo's tag
// list plus the cheap default-branch SHA used for cache invalidation. It is an
// interface, and NewTagLister a package var, so embedders and tests can inject a
// client without hitting GitHub.
type TagLister interface {
	ListActionTags(ctx context.Context, owner, repo string) ([]model.ActionTag, error)
	DefaultBranchSHA(ctx context.Context, owner, repo string) (string, error)
}

// NewTagLister builds the client used by `shuck action`.
var NewTagLister = func(token string) TagLister { return gh.New(token) }

const actionUsage = `shuck action — resolve a GitHub Action to its latest tag and commit SHA, for SHA-pinning.

Usage:
  shuck action <owner>/<action>            latest stable release
  shuck action <owner>/<action>@<version>  latest release matching a version (e.g. @v3 or @3.1)
  shuck action <owner>/<action> <version>  same, with the version as a separate argument

It prints the resolved tag, the commit SHA, and a pin line ready to drop after
"uses:":
  owner/action@<sha> # <tag>

Auth is optional for public repos: set GITHUB_TOKEN/GH_TOKEN or pass --token to
lift the unauthenticated rate limit. Tags are cached under ~/.cache/shuck for an hour,
re-validated against the repo's default-branch commit.

Flags:
`

// runAction implements `shuck action <owner>/<action>[@<version>]`.
func runAction(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck action", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut bool
		token   string
		refresh bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text detail")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.BoolVar(&refresh, "refresh", false, "ignore the cache and re-fetch the action's tags")
	fs.Usage = func() {
		fmt.Fprint(stderr, actionUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ref, err := parseActionArgs(fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	resolved, err := Action(context.Background(), ref, ActionOptions{Token: token, Refresh: refresh})
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}

	if jsonOut {
		if err := action.EncodeJSON(stdout, resolved); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		return 0
	}
	action.Render(stdout, resolved)
	return 0
}

// ActionOptions tunes an action resolution.
type ActionOptions struct {
	Token   string
	Refresh bool
}

// Action resolves an action ref to its latest matching tag + commit SHA,
// sharing the CLI's tag cache and selection. It is exported so the MCP
// front-end reuses the pipeline. Auth is optional: an empty token falls back to
// GITHUB_TOKEN/GH_TOKEN, then to unauthenticated access.
func Action(ctx context.Context, ref action.Ref, opts ActionOptions) (action.Resolved, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	return resolveAction(ctx, ref, token, opts.Refresh)
}

// parseActionArgs accepts the action either as a single "owner/action[@version]"
// token or as "owner/action" plus a separate version argument.
func parseActionArgs(args []string) (action.Ref, error) {
	switch len(args) {
	case 1:
		return action.ParseRef(args[0])
	case 2:
		if strings.Contains(args[0], "@") {
			return action.Ref{}, fmt.Errorf("specify the version once: either %s or a separate argument, not both", args[0])
		}
		return action.ParseRef(args[0] + "@" + args[1])
	case 0:
		return action.Ref{}, fmt.Errorf("missing action (expected: shuck action owner/action[@version])")
	default:
		return action.Ref{}, fmt.Errorf("too many arguments (expected: shuck action owner/action[@version])")
	}
}

// resolveAction loads the action repo's tag list (cached when fresh, else from
// GitHub) and selects the latest tag matching the ref's constraint.
func resolveAction(ctx context.Context, ref action.Ref, token string, refresh bool) (action.Resolved, error) {
	if dir, err := cache.ActionDir(ref.Owner, ref.Repo); err == nil {
		_ = cache.Purge(actionCacheTTL, dir)
	}
	tags, err := loadOrFetchTags(ctx, ref.Owner, ref.Repo, token, refresh)
	if err != nil {
		return action.Resolved{}, err
	}
	if len(tags) == 0 {
		return action.Resolved{}, fmt.Errorf("%s has no tags to pin", ref.RepoSlug())
	}
	tag, err := action.Select(tags, ref.Constraint)
	if err != nil {
		return action.Resolved{}, fmt.Errorf("%s: %w", ref.RepoSlug(), err)
	}
	return action.Resolved{Ref: ref, Tag: tag.Name, SHA: tag.SHA}, nil
}

// loadOrFetchTags returns the repo's tags from the cache when a fresh entry
// exists, otherwise fetches them from GitHub and refreshes the cache.
func loadOrFetchTags(ctx context.Context, owner, repo, token string, refresh bool) ([]model.ActionTag, error) {
	lister := NewTagLister(token)
	var currentSHA string
	if !refresh {
		if tags, cachedSHA, fetchedAt, ok, err := cache.LoadActionTags(owner, repo); err == nil && ok && time.Since(fetchedAt) < actionCacheTTL {
			// Within the TTL, reuse the cache unless the default branch moved. If
			// the cheap SHA check itself fails (e.g. offline), the fresh cache
			// still stands rather than forcing a full re-fetch that would also fail.
			sha, shaErr := lister.DefaultBranchSHA(ctx, owner, repo)
			if shaErr != nil || sha == cachedSHA {
				return tags, nil
			}
			currentSHA = sha
		}
	}
	tags, err := lister.ListActionTags(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if currentSHA == "" {
		// Best-effort: a failed SHA lookup stores "", forcing the next run onto
		// the TTL path rather than the cheap SHA short-circuit.
		currentSHA, _ = lister.DefaultBranchSHA(ctx, owner, repo)
	}
	if err := cache.SaveActionTags(owner, repo, currentSHA, tags); err != nil {
		fmt.Fprintln(os.Stderr, "shuck: warning: could not write action cache:", err)
	}
	return tags, nil
}
