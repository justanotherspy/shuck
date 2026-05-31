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

	"github.com/justanotherspy/shuck/internal/cache"
	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/image"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/target"
)

// imageCacheTTL bounds how long an owner's cached image listing is reused before
// shuck re-validates it. Within the window the cache is reused only when the
// owner's (repo's) default-branch SHA is unchanged; --refresh forces a re-fetch.
const imageCacheTTL = time.Hour

// ImageLister is the slice of gh.Client the image command needs: the GitHub
// Packages API (enumerate images, list a package's versions+digests — needs a
// token), the anonymous OCI registry API (tags + a tag's digest, for public
// images with no token), and the cheap default-branch SHA used for cache
// invalidation. It is an interface, and NewImageLister a package var, so
// embedders and tests can inject a client without hitting the network.
type ImageLister interface {
	ListContainerPackages(ctx context.Context, owner string) ([]string, error)
	ListImageVersions(ctx context.Context, owner, name string) ([]model.ImageVersion, error)
	RegistryTags(ctx context.Context, owner, name string) ([]string, error)
	RegistryDigest(ctx context.Context, owner, name, tag string) (string, error)
	DefaultBranchSHA(ctx context.Context, owner, repo string) (string, error)
}

// NewImageLister builds the client used by `shuck image`.
var NewImageLister = func(token string) ImageLister { return gh.New(token) }

const imageUsage = `shuck image — show GHCR container images, their latest version, and digest.

Usage:
  shuck image                       list every image under the local repo's owner
  shuck image <owner>               list every image under an owner
  shuck image <owner>/<repo>        list every image under that repo's owner
  shuck image <github-url>          same, from a github.com/<owner>/<repo> URL
  shuck image ghcr.io/<owner>/<name>        resolve one image to its latest digest
  shuck image ghcr.io/<owner>/<name>:<tag>  resolve a version (e.g. :v3, :1.2) or exact tag (:latest)

Listing every image uses the GitHub Packages API and needs a token with the
read:packages scope. Resolving a single public ghcr.io image works without a
token via the anonymous registry API; private images need a token.

The digest is the immutable manifest digest (sha256:…). For a multi-arch image
it is the image-index digest, which is the correct value to pin.

Auth: set GITHUB_TOKEN (or GH_TOKEN), or pass --token. Results are cached under
~/.shuck for an hour; --refresh re-fetches.

Flags:
`

// runImage implements `shuck image [ref]`.
func runImage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shuck image", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut bool
		token   string
		refresh bool
	)
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text detail")
	fs.StringVar(&token, "token", "", "GitHub token (overrides GITHUB_TOKEN/GH_TOKEN)")
	fs.BoolVar(&refresh, "refresh", false, "ignore the cache and re-fetch")
	fs.Usage = func() {
		fmt.Fprint(stderr, imageUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := fs.Args()
	opts := ImageOptions{Token: token, Refresh: refresh}

	// An explicit ghcr.io/<owner>/<name> reference resolves a single image;
	// anything else (an owner, owner/repo, URL, or no args) lists every image
	// under the resolved owner.
	if len(rest) == 1 && looksLikeImageRef(rest[0]) {
		ref, err := image.ParseRef(rest[0])
		if err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		if ref.ListAll() {
			fmt.Fprintln(stderr, "shuck: an image reference needs a name (ghcr.io/owner/name)")
			return 2
		}
		resolved, err := Image(context.Background(), ref, opts)
		if err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		if jsonOut {
			if err := image.EncodeJSON(stdout, resolved); err != nil {
				fmt.Fprintln(stderr, "shuck:", err)
				return 2
			}
			return 0
		}
		image.Render(stdout, resolved)
		return 0
	}

	owner, err := resolveOwner(rest)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	pkgs, err := Images(context.Background(), owner, opts)
	if err != nil {
		fmt.Fprintln(stderr, "shuck:", err)
		return 2
	}
	if jsonOut {
		if err := image.EncodeListJSON(stdout, image.DefaultRegistry, owner, pkgs); err != nil {
			fmt.Fprintln(stderr, "shuck:", err)
			return 2
		}
		return 0
	}
	image.RenderList(stdout, image.DefaultRegistry, owner, pkgs)
	return 0
}

// ImageOptions tunes an image resolution or listing.
type ImageOptions struct {
	Token   string
	Refresh bool
}

// Image resolves a single image ref to its latest matching tag + manifest
// digest. With a token it uses the GitHub Packages API (and the shared owner
// cache); without one it falls back to the anonymous GHCR registry API for
// public images. It is exported so the MCP front-end reuses the pipeline.
func Image(ctx context.Context, ref image.Ref, opts ImageOptions) (image.Resolved, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	lister := NewImageLister(token)

	if token == "" {
		return resolveImageAnonymous(ctx, lister, ref)
	}

	pkgs, err := loadOrFetchImages(ctx, lister, ref.Owner, opts.Refresh)
	if err != nil {
		return image.Resolved{}, err
	}
	versions, ok := findPackage(pkgs, ref.Name)
	if !ok {
		// Not in the owner-wide listing (e.g. a package not linked to a repo);
		// fetch just this image's versions directly.
		versions, err = lister.ListImageVersions(ctx, ref.Owner, ref.Name)
		if err != nil {
			return image.Resolved{}, err
		}
	}
	if len(versions) == 0 {
		return image.Resolved{}, fmt.Errorf("%s has no published versions", ref.Slug())
	}
	v, tag, err := image.Select(versions, ref.Constraint)
	if err != nil {
		return image.Resolved{}, fmt.Errorf("%s: %w", ref.Slug(), err)
	}
	return image.Resolved{Ref: ref, Tag: tag, Digest: v.Digest}, nil
}

// resolveImageAnonymous resolves a public image with no token: list its tags via
// the registry, pick the best one, then read that tag's manifest digest.
func resolveImageAnonymous(ctx context.Context, lister ImageLister, ref image.Ref) (image.Resolved, error) {
	tags, err := lister.RegistryTags(ctx, ref.Owner, ref.Name)
	if err != nil {
		return image.Resolved{}, err
	}
	if len(tags) == 0 {
		return image.Resolved{}, fmt.Errorf("%s has no tags", ref.Slug())
	}
	versions := make([]model.ImageVersion, len(tags))
	for i, t := range tags {
		versions[i] = model.ImageVersion{Tags: []string{t}}
	}
	_, tag, err := image.Select(versions, ref.Constraint)
	if err != nil {
		return image.Resolved{}, fmt.Errorf("%s: %w", ref.Slug(), err)
	}
	digest, err := lister.RegistryDigest(ctx, ref.Owner, ref.Name, tag)
	if err != nil {
		return image.Resolved{}, err
	}
	return image.Resolved{Ref: ref, Tag: tag, Digest: digest}, nil
}

// Images lists every container image under an owner, each with its versions.
// It requires a token (the Packages API has no anonymous enumeration) and
// shares the owner cache with single-image resolution.
func Images(ctx context.Context, owner string, opts ImageOptions) ([]model.ImagePackage, error) {
	token := opts.Token
	if token == "" {
		token = tokenFromEnv()
	}
	if token == "" {
		return nil, fmt.Errorf("listing images needs a token with the read:packages scope (set GITHUB_TOKEN/GH_TOKEN or pass --token); a single public image (ghcr.io/owner/name) resolves without one")
	}
	lister := NewImageLister(token)
	return loadOrFetchImages(ctx, lister, owner, opts.Refresh)
}

// loadOrFetchImages returns an owner's image packages from the cache when a
// fresh entry exists, otherwise fetches them from the Packages API and refreshes
// the cache — mirroring the action command's loadOrFetchTags.
func loadOrFetchImages(ctx context.Context, lister ImageLister, owner string, refresh bool) ([]model.ImagePackage, error) {
	if dir, err := cache.ImageDir(owner); err == nil {
		_ = cache.Purge(imageCacheTTL, dir)
	}
	var currentSHA string
	if !refresh {
		if pkgs, cachedSHA, fetchedAt, ok, err := cache.LoadImages(owner); err == nil && ok && time.Since(fetchedAt) < imageCacheTTL {
			// Within the TTL, reuse the cache unless the owner's default branch
			// moved. If the cheap SHA check itself fails (e.g. offline), the fresh
			// cache still stands rather than forcing a full re-fetch.
			sha, shaErr := lister.DefaultBranchSHA(ctx, owner, owner)
			if shaErr != nil || sha == cachedSHA {
				return pkgs, nil
			}
			currentSHA = sha
		}
	}
	pkgs, err := fetchImages(ctx, lister, owner)
	if err != nil {
		return nil, err
	}
	if currentSHA == "" {
		// Best-effort: a failed SHA lookup stores "", forcing the next run onto
		// the TTL path rather than the cheap SHA short-circuit.
		currentSHA, _ = lister.DefaultBranchSHA(ctx, owner, owner)
	}
	if err := cache.SaveImages(image.DefaultRegistry, owner, currentSHA, pkgs); err != nil {
		fmt.Fprintln(os.Stderr, "shuck: warning: could not write image cache:", err)
	}
	return pkgs, nil
}

// fetchImages enumerates an owner's container packages and each one's versions.
func fetchImages(ctx context.Context, lister ImageLister, owner string) ([]model.ImagePackage, error) {
	names, err := lister.ListContainerPackages(ctx, owner)
	if err != nil {
		return nil, err
	}
	pkgs := make([]model.ImagePackage, 0, len(names))
	for _, name := range names {
		versions, err := lister.ListImageVersions(ctx, owner, name)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, model.ImagePackage{Owner: owner, Name: name, Versions: versions})
	}
	return pkgs, nil
}

// findPackage returns the versions of the named package within pkgs.
func findPackage(pkgs []model.ImagePackage, name string) ([]model.ImageVersion, bool) {
	for _, p := range pkgs {
		if p.Name == name {
			return p.Versions, true
		}
	}
	return nil, false
}

// resolveOwner resolves the owner to list images for. A single bare token (no
// '/', not a URL) is taken as the owner directly; everything else (owner/repo, a
// github.com URL, or no args for the local repo) goes through target.ResolveRepo,
// which yields the owner of the repo.
func resolveOwner(args []string) (string, error) {
	if len(args) == 1 {
		s := strings.TrimSpace(args[0])
		if s != "" && !strings.Contains(s, "/") && !strings.Contains(s, "://") {
			return s, nil
		}
	}
	owner, _, err := target.ResolveRepo(args)
	return owner, err
}

// looksLikeImageRef reports whether s is an explicit registry reference
// (ghcr.io/…), which selects single-image resolution. A bare owner, owner/repo,
// or github.com URL is not — those list every image under the owner.
func looksLikeImageRef(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	first, _, ok := strings.Cut(s, "/")
	return ok && strings.EqualFold(first, image.DefaultRegistry)
}
