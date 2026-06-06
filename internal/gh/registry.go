package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// registryHost is the GHCR OCI registry endpoint.
const registryHost = "https://ghcr.io"

// manifestAccept advertises the manifest media types GHCR may return: an OCI or
// Docker image index (multi-arch), or a single Docker v2 manifest. The
// Docker-Content-Digest header GHCR returns is the digest of whichever it picks,
// which is the correct value to pin (for multi-arch images, the index digest).
const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// RegistryTags lists the tags of a public image via the anonymous OCI registry
// v2 API — the tokenless path used when no GitHub token is available. When a
// token is set it is forwarded so private images resolve too.
func (c *Client) RegistryTags(ctx context.Context, owner, name string) ([]string, error) {
	token, err := c.ghcrToken(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	repo := owner + "/" + name
	url := c.registryURL + "/v2/" + repo + "/tags/list?n=100"
	var tags []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list tags for %s: %w", repo, err)
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		err = decodeRegistry(resp, &page, fmt.Sprintf("list tags for %s", repo))
		if err != nil {
			return nil, err
		}
		tags = append(tags, page.Tags...)
		url = nextLink(resp.Header.Get("Link"), c.registryURL)
		_ = resp.Body.Close()
	}
	return tags, nil
}

// RegistryDigest returns the manifest digest (sha256:...) a tag resolves to,
// read from the Docker-Content-Digest header of a manifest request.
func (c *Client) RegistryDigest(ctx context.Context, owner, name, tag string) (string, error) {
	token, err := c.ghcrToken(ctx, owner, name)
	if err != nil {
		return "", err
	}
	repo := owner + "/" + name
	url := c.registryURL + "/v2/" + repo + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get manifest for %s:%s: %w", repo, tag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get manifest for %s:%s: status %s", repo, tag, resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("get manifest for %s:%s: no digest in response", repo, tag)
	}
	return digest, nil
}

// ghcrToken fetches a pull-scoped bearer token from GHCR's token endpoint. With
// no GitHub token this yields an anonymous token good for public images; when a
// token is set it is exchanged (Basic auth) so private images resolve too.
func (c *Client) ghcrToken(ctx context.Context, owner, name string) (string, error) {
	repo := owner + "/" + name
	url := fmt.Sprintf("%s/token?service=ghcr.io&scope=repository:%s:pull", c.registryURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		// GHCR accepts the PAT as the Basic-auth password; the username is ignored.
		req.SetBasicAuth("shuck", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get registry token for %s: %w", repo, err)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeRegistry(resp, &body, fmt.Sprintf("get registry token for %s", repo)); err != nil {
		_ = resp.Body.Close()
		return "", err
	}
	_ = resp.Body.Close()
	return body.Token, nil
}

// decodeRegistry checks a registry response status and decodes its JSON body
// into out. It does not close the body (the caller may still read headers).
func decodeRegistry(resp *http.Response, out any, what string) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %s", what, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", what, err)
	}
	return nil
}

// nextLink extracts the URL of a rel="next" RFC 5988 Link header. A relative
// path is resolved against host; an absolute URL is only followed when it
// targets the same origin (scheme+host) as the configured registry. Each
// paginated request carries the registry bearer token, so refusing to follow a
// next link to a different host keeps that token from being forwarded to an
// origin the registry's Link header points at. It returns "" when there is no
// next page or the next link points off-host.
func nextLink(link, host string) string {
	for part := range strings.SplitSeq(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, s := range segs[1:] {
			if strings.Contains(strings.TrimSpace(s), `rel="next"`) {
				isNext = true
				break
			}
		}
		if !isNext {
			continue
		}
		u := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		if u == "" {
			continue
		}
		// A scheme-relative URL ("//host/path") names its own authority, so it
		// is not an in-registry path; reject it rather than treat it as one.
		if strings.HasPrefix(u, "//") {
			return ""
		}
		if strings.HasPrefix(u, "/") {
			return host + u
		}
		// Absolute URL: only follow it when it stays on the registry's own
		// origin, so the Authorization bearer token is never sent elsewhere.
		if sameOrigin(u, host) {
			return u
		}
		return ""
	}
	return ""
}

// sameOrigin reports whether rawURL has the same scheme and host as the
// configured registry base. A parse failure or any mismatch is treated as a
// different origin (fail closed).
func sameOrigin(rawURL, base string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	return u.Scheme == b.Scheme && u.Host == b.Host
}
