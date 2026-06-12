package cli

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// attachArtifacts lists the run's uploaded artifacts onto the report and, when
// --download-artifacts was given, downloads and extracts each one. The listing
// is best-effort like annotations — except when a download was requested, in
// which case a listing failure is an operational error (the user asked for
// files, so silently producing none would be a false result).
func (a *app) attachArtifacts(ctx context.Context, owner, repo string, runID int64, report *model.Report) error {
	arts, err := a.client.RunArtifacts(ctx, owner, repo, runID)
	if err != nil {
		if a.artifactsDir != "" {
			return err
		}
		fmt.Fprintln(os.Stderr, "shuck: warning: could not list artifacts:", err)
		return nil
	}
	if a.artifactsDir != "" {
		if err := a.downloadArtifacts(ctx, owner, repo, arts); err != nil {
			return err
		}
	}
	report.Artifacts = arts
	return nil
}

// downloadArtifacts fetches each artifact's zip archive and extracts it to
// <artifactsDir>/<artifact-name>/, recording that path on the artifact.
// Expired artifacts have no downloadable archive and are skipped (they stay
// listed, marked expired). Any download or extraction failure is fatal: an
// explicit --download-artifacts must not half-succeed silently.
func (a *app) downloadArtifacts(ctx context.Context, owner, repo string, arts []model.Artifact) error {
	for i := range arts {
		art := &arts[i]
		if art.Expired {
			continue
		}
		dest := filepath.Join(a.artifactsDir, safeArtifactName(art.Name))
		if err := a.downloadArtifact(ctx, owner, repo, art.ID, dest); err != nil {
			return fmt.Errorf("download artifact %q: %w", art.Name, err)
		}
		art.Path = dest
	}
	return nil
}

func (a *app) downloadArtifact(ctx context.Context, owner, repo string, artifactID int64, dest string) error {
	rc, err := a.client.ArtifactArchive(ctx, owner, repo, artifactID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	return extractZip(rc, dest)
}

// extractZip spools the archive stream to a temporary file (the zip central
// directory lives at the end, so random access is required) and extracts it
// under destDir. Every write goes through an os.Root opened at destDir, so a
// crafted entry name (absolute, or escaping via ..) fails closed instead of
// writing outside the directory; symlink entries are dropped, never followed.
func extractZip(archive io.Reader, destDir string) error {
	tmp, err := os.CreateTemp("", "shuck-artifact-*.zip")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, archive)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	for _, f := range zr.File {
		if err := extractEntry(root, f); err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}
	}
	return nil
}

// extractEntry writes one zip entry inside root. Only directories and regular
// files are materialized; anything else (symlinks, devices) is skipped so an
// archive cannot plant a link for a later entry to traverse.
func extractEntry(root *os.Root, f *zip.File) error {
	name := filepath.FromSlash(f.Name)
	mode := f.Mode()
	switch {
	case mode.IsDir():
		return root.MkdirAll(name, 0o755)
	case !mode.IsRegular():
		return nil
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	declared := f.UncompressedSize64
	if declared > math.MaxInt64 {
		return fmt.Errorf("declared size %d too large", declared)
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := root.Create(name)
	if err != nil {
		return err
	}
	// Copy at most the declared uncompressed size, then refuse any excess: a
	// crafted entry must not be able to inflate unbounded (decompression bomb).
	if _, err := io.CopyN(dst, src, int64(declared)); err != nil && !errors.Is(err, io.EOF) {
		_ = dst.Close()
		return err
	}
	// This read must reach EOF: it both rejects excess data and is the read on
	// which archive/zip verifies the entry's CRC-32 (a mismatch is ErrChecksum).
	n, err := io.CopyN(io.Discard, src, 1)
	if n != 0 {
		_ = dst.Close()
		return fmt.Errorf("inflates past its declared size %d", declared)
	}
	if !errors.Is(err, io.EOF) {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// safeArtifactName maps an artifact name (untrusted API data) onto a single
// path element for its extraction directory: path separators are replaced and
// names that would resolve to the directory itself or its parent are renamed.
// GitHub already rejects these characters at upload time; this is belt and
// braces for the join with --download-artifacts.
func safeArtifactName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == os.PathSeparator {
			return '_'
		}
		return r
	}, name)
	switch name {
	case "", ".", "..":
		return "artifact"
	}
	return name
}
