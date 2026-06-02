package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v88/github"

	"github.com/justanotherspy/shuck/internal/model"
)

// ListContainerPackages returns the names of every container package published
// under owner. The owner may be an organization or a user; the org endpoint is
// tried first and the user endpoint is used when it 404s. These endpoints
// require a token with the read:packages scope — an unauthenticated call yields
// an error, which the caller surfaces as guidance.
func (c *Client) ListContainerPackages(ctx context.Context, owner string) ([]string, error) {
	containerType := "container"
	var names []string
	page := 1
	for {
		opts := &github.PackageListOptions{
			PackageType: &containerType,
			ListOptions: github.ListOptions{PerPage: 100, Page: page},
		}
		pkgs, resp, err := c.listPackages(ctx, owner, opts)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			if n := p.GetName(); n != "" {
				names = append(names, n)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return names, nil
}

// listPackages calls the org packages endpoint, falling back to the user
// endpoint when the owner is not an organization (404).
func (c *Client) listPackages(ctx context.Context, owner string, opts *github.PackageListOptions) ([]*github.Package, *github.Response, error) {
	pkgs, resp, err := c.gh.Organizations.ListPackages(ctx, owner, opts)
	if isNotFound(err) {
		uPkgs, uResp, uErr := c.gh.Users.ListPackages(ctx, owner, opts)
		if uErr != nil {
			return nil, uResp, fmt.Errorf("list container packages for %s: %w", owner, uErr)
		}
		return uPkgs, uResp, nil
	}
	if err != nil {
		return nil, resp, fmt.Errorf("list container packages for %s: %w", owner, err)
	}
	return pkgs, resp, nil
}

// ListImageVersions returns every published version of a container image,
// each carrying its manifest digest (the version Name) and the tags pointing at
// it. The org endpoint (paginated) is tried first; on a 404 the owner is treated
// as a user. Like ListContainerPackages this requires a read:packages token.
func (c *Client) ListImageVersions(ctx context.Context, owner, name string) ([]model.ImageVersion, error) {
	versions, err := c.listVersions(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := make([]model.ImageVersion, 0, len(versions))
	for _, v := range versions {
		digest := v.GetName()
		if digest == "" {
			continue
		}
		out = append(out, model.ImageVersion{
			Tags:      containerTags(v),
			Digest:    digest,
			UpdatedAt: v.GetUpdatedAt().Time,
		})
	}
	return out, nil
}

// listVersions pages the org container-versions endpoint, falling back to the
// user endpoint when the owner is not an organization (404). The user endpoint
// (ListUserPackageVersions) is not paginated by go-github; GitHub returns the
// most recent versions, which is sufficient for picking the latest.
func (c *Client) listVersions(ctx context.Context, owner, name string) ([]*github.PackageVersion, error) {
	var all []*github.PackageVersion
	page := 1
	for {
		opts := &github.PackageListOptions{ListOptions: github.ListOptions{PerPage: 100, Page: page}}
		versions, resp, err := c.gh.Organizations.PackageGetAllVersions(ctx, owner, "container", name, opts)
		if isNotFound(err) {
			uVers, _, uErr := c.gh.Users.ListUserPackageVersions(ctx, owner, "container", name)
			if uErr != nil {
				return nil, fmt.Errorf("list versions for %s/%s: %w", owner, name, uErr)
			}
			return uVers, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list versions for %s/%s: %w", owner, name, err)
		}
		all = append(all, versions...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return all, nil
}

// containerTags extracts the container image tags from a package version. The
// Metadata field is raw JSON (it is polymorphic across package types and webhook
// events); a parse failure or non-container package simply yields no tags.
func containerTags(v *github.PackageVersion) []string {
	if len(v.Metadata) == 0 {
		return nil
	}
	var meta github.PackageMetadata
	if err := json.Unmarshal(v.Metadata, &meta); err != nil {
		return nil
	}
	if meta.Container == nil {
		return nil
	}
	return meta.Container.Tags
}

// isNotFound reports whether err is a GitHub 404, used to fall back from the org
// packages endpoint to the user endpoint.
func isNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

// IsAuthError reports whether err is a GitHub API authentication/authorization
// failure (401 or 403) — e.g. a token without the read:packages scope, or a
// fine-grained token (which the Packages API does not support at all). Callers
// use it to fall back to the anonymous registry API for public images.
func IsAuthError(err error) bool {
	var ghErr *github.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	return ghErr.Response.StatusCode == http.StatusUnauthorized || ghErr.Response.StatusCode == http.StatusForbidden
}
