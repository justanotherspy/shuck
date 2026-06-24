// Package release discovers, downloads, and verifies shuck's own GitHub
// releases, and replaces the running binary in place. It backs the
// `shuck version --check` and `shuck upgrade` commands so a shuck installed via
// install.sh (or a manual release download) can keep itself current without a
// package manager.
//
// The download path mirrors install.sh exactly: it resolves the latest release
// tag, fetches the goreleaser archive for the host OS/arch plus checksums.txt,
// verifies the archive's SHA-256 (failing closed on mismatch), and extracts the
// shuck binary.
package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository shuck releases are published to.
const Repo = "justanotherspy/shuck"

const (
	defaultAPIBase      = "https://api.github.com"
	defaultDownloadBase = "https://github.com"
)

// Client discovers and downloads shuck releases. The host fields are exported so
// tests can point them at a local server; New fills them with GitHub's hosts.
type Client struct {
	HTTP         *http.Client
	APIBase      string // GitHub REST API base, e.g. https://api.github.com
	DownloadBase string // release-asset host, e.g. https://github.com
	Repo         string // owner/repo
	Token        string // optional bearer token; release checks work unauthenticated
}

// New returns a Client targeting GitHub's public hosts with an optional auth
// token (used to lift the unauthenticated API rate limit when present).
func New(token string) *Client {
	return &Client{
		HTTP:         &http.Client{Timeout: 120 * time.Second},
		APIBase:      defaultAPIBase,
		DownloadBase: defaultDownloadBase,
		Repo:         Repo,
		Token:        token,
	}
}

// Latest returns the tag of the repository's latest published release. GitHub's
// "latest" endpoint already excludes drafts and prereleases.
func (c *Client) Latest(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.APIBase, c.Repo)
	body, err := c.get(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = body.Close() }()

	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(body).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("latest release has no tag_name")
	}
	return rel.TagName, nil
}

// Download fetches the release archive for tag/goos/goarch, verifies it against
// the release's checksums.txt, and returns the extracted shuck binary. goos and
// goarch use Go's runtime naming (e.g. "darwin", "amd64"), matching the asset
// names goreleaser produces.
func (c *Client) Download(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	version := strings.TrimPrefix(tag, "v")
	ext, binName := "tar.gz", "shuck"
	if goos == "windows" {
		ext, binName = "zip", "shuck.exe"
	}
	archive := fmt.Sprintf("shuck_%s_%s_%s.%s", version, goos, goarch, ext)
	base := fmt.Sprintf("%s/%s/releases/download/%s", c.DownloadBase, c.Repo, tag)

	archiveBytes, err := c.download(ctx, base+"/"+archive)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", archive, err)
	}
	sums, err := c.download(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("download checksums.txt: %w", err)
	}
	if err := verifyChecksum(archive, archiveBytes, sums); err != nil {
		return nil, err
	}
	return extractBinary(archive, archiveBytes, binName, maxBinarySize)
}

func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %s", url, resp.Status)
	}
	return resp.Body, nil
}

func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return io.ReadAll(body)
}

// verifyChecksum fails closed: the archive must be listed in checksums.txt and
// its SHA-256 must match the listed digest.
func verifyChecksum(archive string, data, checksums []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for line := range strings.SplitSeq(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archive {
			if fields[0] != got {
				return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", archive, fields[0], got)
			}
			return nil
		}
	}
	return fmt.Errorf("%s not listed in checksums.txt", archive)
}

// maxBinarySize bounds how many bytes extraction will read out of a release
// archive. Archive members are compressed, so a tiny deflate stream can inflate
// enormously (DEFLATE expands up to ~1032:1); without a ceiling a crafted or
// corrupt archive could drive extraction into an unbounded allocation. The real
// shuck binary is ~16 MiB, so 256 MiB is generous headroom for growth while
// still firmly capping a decompression bomb. Exceeding the cap is treated as a
// bad archive (see readCapped), never a silent truncation — a truncated binary
// would be unrunnable.
const maxBinarySize = 256 << 20

// extractBinary pulls the named binary out of a .tar.gz or .zip archive, reading
// at most maxBytes of the decompressed entry.
func extractBinary(archive string, data []byte, binName string, maxBytes int64) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		return extractZip(data, binName, maxBytes)
	}
	return extractTarGz(data, binName, maxBytes)
}

func extractTarGz(data []byte, binName string, maxBytes int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		// Only accept a regular file. A symlink (or other irregular entry) named
		// like the binary must not be followed: its tar body is empty, which would
		// otherwise overwrite the running binary with a zero-byte file.
		if pathBase(hdr.Name) == binName && hdr.FileInfo().Mode().IsRegular() {
			return readCapped(tr, maxBytes)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", binName)
}

func extractZip(data []byte, binName string, maxBytes int64) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, f := range zr.File {
		// Skip anything that is not a regular file (e.g. a symlink entry, whose
		// target would otherwise be read in place of the binary).
		if pathBase(f.Name) != binName || !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := readCapped(rc, maxBytes)
		_ = rc.Close()
		return b, err
	}
	return nil, fmt.Errorf("%s not found in archive", binName)
}

// readCapped reads all of r but never more than maxBytes. It reads one byte past
// the limit so it can tell "exactly maxBytes" from "more than maxBytes", and
// returns an error when the stream exceeds the limit. This bounds the allocation
// (to maxBytes+1) so a decompression bomb cannot exhaust memory, and fails
// closed rather than returning a truncated, unrunnable binary.
func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("decompressed binary exceeds %d-byte limit", maxBytes)
	}
	return b, nil
}

// pathBase returns the final element of an archive entry path. Archive members
// always use forward slashes, so it does not use filepath (which would split on
// backslashes when shuck upgrade runs on Windows).
func pathBase(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// Compare orders two version strings by semantic version. A leading "v" and any
// pre-release/build suffix beyond MAJOR.MINOR.PATCH are ignored. It returns -1,
// 0, or 1 for a<b, a==b, a>b.
func Compare(a, b string) int {
	an, bn := parseVersion(a), parseVersion(b)
	for i := range an {
		switch {
		case an[i] < bn[i]:
			return -1
		case an[i] > bn[i]:
			return 1
		}
	}
	return 0
}

// IsSemver reports whether v looks like vMAJOR.MINOR.PATCH so callers can
// special-case non-comparable builds (e.g. "dev" from a plain `go build`).
func IsSemver(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}
