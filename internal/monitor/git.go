package monitor

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justanotherspy/shuck/internal/target"
)

// Checkout is what a working tree currently points at: the repository behind
// its "origin" remote and the branch checked out in it.
//
// A detached HEAD leaves Branch empty and records the commit in Head instead.
// That is not an error — a detached checkout simply has no branch to match an
// open PR against, and the watch that owns it reports itself as idle.
type Checkout struct {
	// Owner and Repo come from the origin remote's URL.
	Owner string
	Repo  string
	// Branch is the checked-out branch's short name, or "" when HEAD is
	// detached.
	Branch string
	// Head is the commit HEAD resolves to, when it can be read cheaply. It is
	// advisory: the monitor asks GitHub for the PR's head SHA rather than
	// trusting the local one, which may not be pushed.
	Head string
}

// Same reports whether two checkouts point at the same repository and branch.
// Head deliberately does not participate: a local commit that has not been
// pushed does not retarget a watch, it just means the next poll may still see
// the previous head SHA on GitHub.
func (c Checkout) Same(o Checkout) bool {
	return c.Owner == o.Owner && c.Repo == o.Repo && c.Branch == o.Branch
}

// String renders a checkout the way the monitor reports it.
func (c Checkout) String() string {
	if c.Branch == "" {
		return fmt.Sprintf("%s/%s (detached)", c.Owner, c.Repo)
	}
	return fmt.Sprintf("%s/%s@%s", c.Owner, c.Repo, c.Branch)
}

// ReadCheckout inspects the working tree at dir and reports the repository and
// branch it is on.
//
// It reads the git files directly rather than going through a git library. The
// monitor asks this question of every watched tree on every tick — often once a
// second while a run is in flight — so it has to stay to a couple of small
// reads with no object-database access, and it has to work the same way in a
// linked worktree (where .git is a file pointing elsewhere) as in a normal
// clone. Only the remote URL is handed off, to target.ParseRemote, which
// already understands every form GitHub remotes come in.
func ReadCheckout(dir string) (Checkout, error) {
	gitDir, commonDir, err := resolveGitDir(dir)
	if err != nil {
		return Checkout{}, err
	}

	branch, head, err := readHead(gitDir)
	if err != nil {
		return Checkout{}, err
	}

	// A linked worktree keeps its own HEAD but shares the repository's config
	// (and therefore its remotes) with the main checkout, which is what
	// commonDir points at.
	url, err := originURL(filepath.Join(commonDir, "config"))
	if err != nil {
		return Checkout{}, err
	}
	owner, repo, err := target.ParseRemote(url)
	if err != nil {
		return Checkout{}, err
	}
	return Checkout{Owner: owner, Repo: repo, Branch: branch, Head: head}, nil
}

// resolveGitDir walks up from dir to the enclosing repository and returns its
// git directory plus the "common" directory that holds the shared config. For
// an ordinary clone the two are the same; for a linked worktree the git
// directory is .git/worktrees/<name> under the main repository and the common
// directory is that main .git.
func resolveGitDir(dir string) (gitDir, commonDir string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", dir, err)
	}

	for cur := abs; ; {
		candidate := filepath.Join(cur, ".git")
		info, statErr := os.Stat(candidate)
		switch {
		case statErr == nil && info.IsDir():
			return candidate, candidate, nil
		case statErr == nil:
			// A linked worktree (or a submodule): .git is a file whose
			// contents are "gitdir: <path>".
			gd, readErr := readGitFile(candidate)
			if readErr != nil {
				return "", "", readErr
			}
			return gd, commonDirOf(gd), nil
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", fmt.Errorf("%s is not inside a git repository", abs)
		}
		cur = parent
	}
}

// readGitFile resolves a ".git" file's "gitdir:" pointer to an absolute path.
func readGitFile(name string) (string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	line := strings.TrimSpace(string(raw))
	gd, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s does not name a git directory", name)
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(filepath.Dir(name), gd)
	}
	return filepath.Clean(gd), nil
}

// commonDirOf finds the repository-wide git directory for a worktree's git
// directory. Git records it in a "commondir" file; when that is missing (a
// submodule, say) the git directory is already the common one.
func commonDirOf(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	rel := strings.TrimSpace(string(raw))
	if rel == "" {
		return gitDir
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Clean(filepath.Join(gitDir, rel))
}

// readHead reads a git directory's HEAD, returning the short branch name for a
// symbolic HEAD or the commit for a detached one.
func readHead(gitDir string) (branch, head string, err error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", "", fmt.Errorf("read HEAD: %w", err)
	}
	line := strings.TrimSpace(string(raw))

	ref, ok := strings.CutPrefix(line, "ref:")
	if !ok {
		// Detached: HEAD holds the commit itself.
		return "", line, nil
	}
	ref = strings.TrimSpace(ref)
	branch = strings.TrimPrefix(ref, "refs/heads/")

	// The commit is a convenience, so a branch whose ref is only in
	// packed-refs (or does not exist yet, on a fresh repository) still yields
	// a usable checkout.
	//
	// HEAD is a local file, but it is one anything on this machine can write,
	// and a ref of "refs/heads/../../../../etc/passwd" would otherwise send
	// this read wherever it liked. A ref that tries to leave the git directory
	// is not a ref.
	if sha, ok := readRef(gitDir, ref); ok {
		head = sha
	}
	return branch, head, nil
}

// readRef reads a loose ref's commit, refusing any ref that would escape the
// git directory. It reports ok=false when the ref is unsafe or simply absent.
func readRef(gitDir, ref string) (sha string, ok bool) {
	if ref == "" || strings.Contains(ref, "..") || filepath.IsAbs(ref) {
		return "", false
	}
	path := filepath.Join(gitDir, filepath.FromSlash(ref))
	if !strings.HasPrefix(path, filepath.Clean(gitDir)+string(filepath.Separator)) {
		return "", false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is confined to gitDir above
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

// errNoOrigin reports a repository with no usable "origin" remote. It is
// distinguished so a watch can explain itself ("this tree has no origin")
// rather than reporting a parse failure.
var errNoOrigin = errors.New(`no "origin" remote with a URL`)

// originURL extracts the origin remote's first URL from a git config file.
//
// git config is INI-like: sections in brackets, keys indented beneath. Only the
// [remote "origin"] section's url matters here, so a hand-rolled scan is both
// sufficient and free of a config-parsing dependency on the hot path.
func originURL(configPath string) (string, error) {
	f, err := os.Open(configPath)
	if err != nil {
		return "", fmt.Errorf("read git config: %w", err)
	}
	defer f.Close()

	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inOrigin = isOriginSection(line)
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if url := strings.TrimSpace(val); url != "" {
			return url, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read git config: %w", err)
	}
	return "", errNoOrigin
}

// isOriginSection reports whether an INI section header opens the origin
// remote's section, accepting both the quoted subsection form git writes
// (`[remote "origin"]`) and the compact form (`[remote.origin]`).
func isOriginSection(line string) bool {
	inner := strings.TrimSpace(strings.Trim(line, "[]"))
	name, sub, ok := strings.Cut(inner, " ")
	if ok {
		return strings.EqualFold(strings.TrimSpace(name), "remote") &&
			strings.Trim(strings.TrimSpace(sub), `"`) == "origin"
	}
	name, sub, ok = strings.Cut(inner, ".")
	return ok && strings.EqualFold(name, "remote") && sub == "origin"
}
