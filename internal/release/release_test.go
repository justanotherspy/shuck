package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v0.3.0", "v0.4.0", -1},
		{"v0.3.0-5-gabc-dirty", "v0.3.0", 0}, // suffix beyond MAJOR.MINOR.PATCH ignored
		{"v1.2", "v1.2.0", 0},                // missing component reads as 0
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsSemver(t *testing.T) {
	cases := map[string]bool{
		"v0.3.0":        true,
		"0.3.0":         true,
		"v0.3.0-5-gabc": true, // pre-release suffix tolerated; core is comparable
		"dev":           false,
		"(devel)":       false,
		"v1.2":          false,
		"v1.2.3.4":      false,
		"":              false,
	}
	for in, want := range cases {
		if got := IsSemver(in); got != want {
			t.Errorf("IsSemver(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v1.4.2","name":"v1.4.2"}`)
	}))
	defer srv.Close()

	c := New("")
	c.APIBase = srv.URL
	got, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got != "v1.4.2" {
		t.Errorf("Latest = %q, want v1.4.2", got)
	}
}

func TestLatestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New("")
	c.APIBase = srv.URL
	if _, err := c.Latest(context.Background()); err == nil {
		t.Error("expected error for non-200 response")
	}
}

func TestDownloadVerifiesAndExtracts(t *testing.T) {
	const (
		tag  = "v1.4.2"
		goos = "linux"
		arch = "amd64"
	)
	wantBin := []byte("#!/bin/sh\necho i am shuck\n")
	archive := makeTarGz(t, "shuck", wantBin)
	archiveName := fmt.Sprintf("shuck_1.4.2_%s_%s.tar.gz", goos, arch)
	checksums := fmt.Sprintf("%s  %s\n%s  shuck_1.4.2_darwin_arm64.tar.gz\n", sha256Hex(archive), archiveName, sha256Hex([]byte("other")))

	srv := serveAssets(t, map[string][]byte{
		archiveName:     archive,
		"checksums.txt": []byte(checksums),
	})
	defer srv.Close()

	c := New("")
	c.DownloadBase = srv.URL
	got, err := c.Download(context.Background(), tag, goos, arch)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got, wantBin) {
		t.Errorf("extracted binary = %q, want %q", got, wantBin)
	}
}

func TestDownloadZip(t *testing.T) {
	wantBin := []byte("MZ fake windows binary")
	archive := makeZip(t, "shuck.exe", wantBin)
	archiveName := "shuck_1.4.2_windows_amd64.zip"
	checksums := fmt.Sprintf("%s  %s\n", sha256Hex(archive), archiveName)

	srv := serveAssets(t, map[string][]byte{
		archiveName:     archive,
		"checksums.txt": []byte(checksums),
	})
	defer srv.Close()

	c := New("")
	c.DownloadBase = srv.URL
	got, err := c.Download(context.Background(), "v1.4.2", "windows", "amd64")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got, wantBin) {
		t.Errorf("extracted binary = %q, want %q", got, wantBin)
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	archive := makeTarGz(t, "shuck", []byte("real"))
	archiveName := "shuck_1.4.2_linux_amd64.tar.gz"
	// Advertise a deliberately wrong checksum: verification must fail closed.
	checksums := fmt.Sprintf("%s  %s\n", sha256Hex([]byte("tampered")), archiveName)

	srv := serveAssets(t, map[string][]byte{
		archiveName:     archive,
		"checksums.txt": []byte(checksums),
	})
	defer srv.Close()

	c := New("")
	c.DownloadBase = srv.URL
	if _, err := c.Download(context.Background(), "v1.4.2", "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected checksum mismatch error, got %v", err)
	}
}

func TestDownloadNotInChecksums(t *testing.T) {
	archive := makeTarGz(t, "shuck", []byte("real"))
	archiveName := "shuck_1.4.2_linux_amd64.tar.gz"

	srv := serveAssets(t, map[string][]byte{
		archiveName:     archive,
		"checksums.txt": []byte("deadbeef  some_other_file.tar.gz\n"),
	})
	defer srv.Close()

	c := New("")
	c.DownloadBase = srv.URL
	if _, err := c.Download(context.Background(), "v1.4.2", "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "not listed in checksums.txt") {
		t.Errorf("expected not-listed error, got %v", err)
	}
}

// serveAssets returns a server that maps the final path element of a request to
// the bytes in files, mimicking GitHub's release-download host.
func serveAssets(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeTarGzSymlink builds an archive whose only "shuck" entry is a symlink, used
// to verify extraction refuses to follow it instead of producing a 0-byte binary.
func makeTarGzSymlink(t *testing.T, name, target string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadRejectsSymlinkBinary(t *testing.T) {
	archive := makeTarGzSymlink(t, "shuck", "/etc/passwd")
	archiveName := "shuck_1.4.2_linux_amd64.tar.gz"
	checksums := fmt.Sprintf("%s  %s\n", sha256Hex(archive), archiveName)

	srv := serveAssets(t, map[string][]byte{
		archiveName:     archive,
		"checksums.txt": []byte(checksums),
	})
	defer srv.Close()

	c := New("")
	c.DownloadBase = srv.URL
	// The archive passes checksum verification but contains no regular "shuck"
	// file, so extraction must report it as not found rather than returning the
	// (empty) symlink body.
	if _, err := c.Download(context.Background(), "v1.4.2", "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' for a symlink-only archive, got %v", err)
	}
}

func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
