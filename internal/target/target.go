// Package target resolves which PR shuck should inspect from the command-line
// arguments and, when needed, the local git repository (via go-git).
package target

import (
	"fmt"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// Target identifies what to inspect. For a PR target, Number is the PR (or 0
// when it must be found by matching Branch against open PRs). For a run/job
// target (a GitHub Actions URL), RunID is set and JobID is the specific job
// (0 means inspect the whole run); Attempt selects a specific run attempt (0
// means the latest). CheckRunID is set for a PR "Checks" tab URL
// (.../pull/<n>/checks?check_run_id=<id>): Number still names the PR, and the
// check run is resolved to its underlying Actions job at inspection time.
type Target struct {
	Owner      string
	Repo       string
	Number     int
	Branch     string
	RunID      int64
	JobID      int64
	Attempt    int
	CheckRunID int64
}

// Resolve interprets the positional args:
//
//	shuck <owner>/<repo> <pr>  -> explicit owner/repo + number
//	shuck <pr-url>             -> owner/repo + number from a GitHub PR URL
//	shuck <run-url>            -> owner/repo + run ID from a GitHub Actions run URL
//	shuck <job-url>            -> owner/repo + run/job ID from a job URL
//	shuck <pr>                 -> number, owner/repo from the local repo
//	shuck                      -> owner/repo + current branch from the local repo
func Resolve(args []string) (Target, error) {
	switch len(args) {
	case 2:
		owner, repo, err := splitSlug(args[0])
		if err != nil {
			return Target{}, err
		}
		pr := args[1] //nolint:gosec // len(args) == 2 in this switch case
		n, err := strconv.Atoi(pr)
		if err != nil {
			return Target{}, fmt.Errorf("invalid PR number %q", pr)
		}
		return Target{Owner: owner, Repo: repo, Number: n}, nil

	case 1:
		// A PR "Checks" tab URL carries a check_run_id query param; recognize it
		// before the plain PR form, which would drop the param and lose the link
		// to the specific check.
		if owner, repo, n, checkRunID, ok := parseChecksURL(args[0]); ok {
			return Target{Owner: owner, Repo: repo, Number: n, CheckRunID: checkRunID}, nil
		}
		if owner, repo, n, ok := parsePRURL(args[0]); ok {
			return Target{Owner: owner, Repo: repo, Number: n}, nil
		}
		if ref, ok := parseActionsURL(args[0]); ok {
			return Target{Owner: ref.Owner, Repo: ref.Repo, RunID: ref.RunID, JobID: ref.JobID, Attempt: ref.Attempt}, nil
		}
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return Target{}, fmt.Errorf("invalid PR number %q (expected: shuck <owner>/<repo> <pr> | shuck <pr-url> | shuck <run-url> | shuck <job-url> | shuck <pr> | shuck)", args[0])
		}
		owner, repo, _, err := localRepo()
		if err != nil {
			return Target{}, err
		}
		return Target{Owner: owner, Repo: repo, Number: n}, nil

	case 0:
		owner, repo, branch, err := localRepo()
		if err != nil {
			return Target{}, err
		}
		return Target{Owner: owner, Repo: repo, Branch: branch}, nil

	default:
		return Target{}, fmt.Errorf("too many arguments (expected: shuck <owner>/<repo> <pr> | shuck <pr> | shuck)")
	}
}

// ResolveRepo resolves just an owner/repo (no PR) for repo-scoped commands like
// `shuck security`. It accepts an "owner/repo" slug, any github.com/<owner>/<repo>
// URL (PR, run, or plain repo), or — with no args — the local repo's origin.
func ResolveRepo(args []string) (owner, repo string, err error) {
	switch len(args) {
	case 0:
		owner, repo, _, err = localRepo()
		return owner, repo, err
	case 1:
		// URL forms first: splitSlug would mis-parse a URL as owner/repo.
		if o, r, _, _, ok := parseChecksURL(args[0]); ok {
			return o, r, nil
		}
		if o, r, _, ok := parsePRURL(args[0]); ok {
			return o, r, nil
		}
		if ref, ok := parseActionsURL(args[0]); ok {
			return ref.Owner, ref.Repo, nil
		}
		if o, r, ok := parseRepoURL(args[0]); ok {
			return o, r, nil
		}
		if !strings.Contains(args[0], "://") {
			if o, r, err := splitSlug(args[0]); err == nil {
				return o, r, nil
			}
		}
		return "", "", fmt.Errorf("invalid repository %q (expected owner/repo or a github.com URL)", args[0])
	default:
		return "", "", fmt.Errorf("too many arguments (expected: shuck security [owner/repo | url])")
	}
}

// parseRepoURL extracts owner/repo from a plain repository URL such as
// https://github.com/owner/repo (with or without a scheme, trailing path, or a
// query/fragment). Unlike ParseRemote it takes the first two path segments after
// the host, so a trailing /pull/<n> or /tree/<branch> does not shift the result.
func parseRepoURL(s string) (owner, repo string, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false // need host/owner/repo
	}
	return parts[1], strings.TrimSuffix(parts[2], ".git"), true
}

func splitSlug(slug string) (owner, repo string, err error) {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q (expected owner/repo)", slug)
	}
	return parts[0], parts[1], nil
}

// parsePRURL extracts owner, repo, and number from a GitHub PR URL such as
// https://github.com/owner/repo/pull/12 (with or without a scheme, trailing
// path segments, or a query/fragment). It returns ok=false for anything that
// is not a recognizable .../<owner>/<repo>/pull/<number> path.
func parsePRURL(s string) (owner, repo string, number int, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	for i := 2; i+1 < len(parts); i++ {
		if parts[i] != "pull" && parts[i] != "pulls" {
			continue
		}
		n, err := strconv.Atoi(parts[i+1])
		if err != nil || n <= 0 {
			return "", "", 0, false
		}
		if parts[i-2] == "" || parts[i-1] == "" {
			return "", "", 0, false
		}
		return parts[i-2], parts[i-1], n, true
	}
	return "", "", 0, false
}

// ActionsRef is a parsed GitHub Actions run/job URL: the run (RunID) and,
// optionally, a single job (JobID) or a specific run attempt (Attempt). JobID
// and Attempt are 0 when their URL segment is absent.
type ActionsRef struct {
	Owner   string
	Repo    string
	RunID   int64
	JobID   int64
	Attempt int
}

// ParseActionsURL parses a GitHub Actions URL into an ActionsRef. It is exported
// so other packages (e.g. the check-run resolver in internal/gh) can turn a
// check run's details URL back into a run/job target. See parseActionsURL for
// the recognized forms.
func ParseActionsURL(s string) (ActionsRef, bool) {
	return parseActionsURL(s)
}

// parseActionsURL extracts owner, repo, run ID, and (optionally) job ID or run
// attempt from a GitHub Actions URL such as
//
//	https://github.com/owner/repo/actions/runs/123
//	https://github.com/owner/repo/actions/runs/123/job/456
//	https://github.com/owner/repo/actions/runs/123/attempts/2
//
// (with or without a scheme, trailing path segments, or a query/fragment). It
// returns ok=false for anything that is not a recognizable
// .../<owner>/<repo>/actions/runs/<run> path. The segment after the run ID is
// either job/<id> (a single job) or attempts/<n> (a specific run attempt).
func parseActionsURL(s string) (ActionsRef, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	for i := 2; i+2 < len(parts); i++ {
		if parts[i] != "actions" || parts[i+1] != "runs" {
			continue
		}
		if parts[i-2] == "" || parts[i-1] == "" {
			return ActionsRef{}, false
		}
		rid, err := strconv.ParseInt(parts[i+2], 10, 64)
		if err != nil || rid <= 0 {
			return ActionsRef{}, false
		}
		ref := ActionsRef{Owner: parts[i-2], Repo: parts[i-1], RunID: rid}
		if i+4 < len(parts) {
			switch parts[i+3] {
			case "job", "jobs":
				jid, err := strconv.ParseInt(parts[i+4], 10, 64)
				if err != nil || jid <= 0 {
					return ActionsRef{}, false
				}
				ref.JobID = jid
			case "attempts", "attempt":
				att, err := strconv.Atoi(parts[i+4])
				if err != nil || att <= 0 {
					return ActionsRef{}, false
				}
				ref.Attempt = att
			}
		}
		return ref, true
	}
	return ActionsRef{}, false
}

// parseChecksURL extracts owner, repo, PR number, and check-run ID from a PR
// "Checks" tab URL such as
//
//	https://github.com/owner/repo/pull/42/checks?check_run_id=12345
//
// It returns ok=false unless the path is a .../<owner>/<repo>/pull/<n>[/checks]
// form *and* a positive check_run_id query parameter is present, so a plain PR
// URL falls through to parsePRURL.
func parseChecksURL(s string) (owner, repo string, number int, checkRunID int64, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	path, query := s, ""
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		path, query = s[:i], s[i+1:]
	}
	id := checkRunIDParam(query)
	if id <= 0 {
		return "", "", 0, 0, false
	}
	o, r, n, prOK := parsePRURL(path)
	if !prOK {
		return "", "", 0, 0, false
	}
	return o, r, n, id, true
}

// checkRunIDParam pulls a positive check_run_id out of a raw URL query string
// (e.g. "check_run_id=123&foo=bar"). It returns 0 when absent or non-numeric.
func checkRunIDParam(query string) int64 {
	for kv := range strings.SplitSeq(query, "&") {
		key, val, _ := strings.Cut(kv, "=")
		if key != "check_run_id" {
			continue
		}
		id, err := strconv.ParseInt(val, 10, 64)
		if err != nil || id <= 0 {
			return 0
		}
		return id
	}
	return 0
}

// localRepo reads owner/repo and the current branch from the repo containing the
// working directory.
func localRepo() (owner, repo, branch string, err error) {
	r, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", "", "", fmt.Errorf("not inside a git repository: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		return "", "", "", fmt.Errorf("read HEAD: %w", err)
	}
	rem, err := r.Remote("origin")
	if err != nil {
		return "", "", "", fmt.Errorf("read 'origin' remote: %w", err)
	}
	urls := rem.Config().URLs
	if len(urls) == 0 {
		return "", "", "", fmt.Errorf("'origin' remote has no URL")
	}
	owner, repo, err = ParseRemote(urls[0])
	if err != nil {
		return "", "", "", err
	}
	return owner, repo, head.Name().Short(), nil
}

// ParseRemote extracts owner and repo from a GitHub remote URL in scp-like
// (git@github.com:owner/repo.git), HTTPS, or ssh:// form.
func ParseRemote(raw string) (owner, repo string, err error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")

	switch {
	case strings.Contains(s, "://"):
		rest := s[strings.Index(s, "://")+3:]
		if _, after, ok := strings.Cut(rest, "/"); ok {
			s = after
		}
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		s = s[strings.Index(s, ":")+1:]
	}

	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-1] == "" || parts[len(parts)-2] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote %q", raw)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}
