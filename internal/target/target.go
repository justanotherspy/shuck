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
// (0 means inspect the whole run).
type Target struct {
	Owner  string
	Repo   string
	Number int
	Branch string
	RunID  int64
	JobID  int64
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
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return Target{}, fmt.Errorf("invalid PR number %q", args[1])
		}
		return Target{Owner: owner, Repo: repo, Number: n}, nil

	case 1:
		if owner, repo, n, ok := parsePRURL(args[0]); ok {
			return Target{Owner: owner, Repo: repo, Number: n}, nil
		}
		if owner, repo, runID, jobID, ok := parseActionsURL(args[0]); ok {
			return Target{Owner: owner, Repo: repo, RunID: runID, JobID: jobID}, nil
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
		if o, r, _, ok := parsePRURL(args[0]); ok {
			return o, r, nil
		}
		if o, r, _, _, ok := parseActionsURL(args[0]); ok {
			return o, r, nil
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

func splitSlug(slug string) (string, string, error) {
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

// parseActionsURL extracts owner, repo, run ID, and (optionally) job ID from a
// GitHub Actions URL such as
//
//	https://github.com/owner/repo/actions/runs/123
//	https://github.com/owner/repo/actions/runs/123/job/456
//
// (with or without a scheme, trailing path segments, or a query/fragment). It
// returns ok=false for anything that is not a recognizable
// .../<owner>/<repo>/actions/runs/<run> path.
func parseActionsURL(s string) (owner, repo string, runID, jobID int64, ok bool) {
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
			return "", "", 0, 0, false
		}
		rid, err := strconv.ParseInt(parts[i+2], 10, 64)
		if err != nil || rid <= 0 {
			return "", "", 0, 0, false
		}
		owner, repo, runID = parts[i-2], parts[i-1], rid
		if i+4 < len(parts) && (parts[i+3] == "job" || parts[i+3] == "jobs") {
			jid, err := strconv.ParseInt(parts[i+4], 10, 64)
			if err != nil || jid <= 0 {
				return "", "", 0, 0, false
			}
			jobID = jid
		}
		return owner, repo, runID, jobID, true
	}
	return "", "", 0, 0, false
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
func ParseRemote(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")

	switch {
	case strings.Contains(s, "://"):
		rest := s[strings.Index(s, "://")+3:]
		if k := strings.Index(rest, "/"); k >= 0 {
			s = rest[k+1:]
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
