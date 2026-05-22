// Package target resolves which PR shuck should inspect from the command-line
// arguments and, when needed, the local git repository (via go-git).
package target

import (
	"fmt"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// Target identifies the PR to inspect. When Number is 0 the PR must be found by
// matching Branch against open PRs.
type Target struct {
	Owner  string
	Repo   string
	Number int
	Branch string
}

// Resolve interprets the positional args:
//
//	shuck <owner>/<repo> <pr>  -> explicit owner/repo + number
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
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return Target{}, fmt.Errorf("invalid PR number %q (expected: shuck <owner>/<repo> <pr> | shuck <pr> | shuck)", args[0])
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

func splitSlug(slug string) (string, string, error) {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q (expected owner/repo)", slug)
	}
	return parts[0], parts[1], nil
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
