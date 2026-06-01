package release

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newServerCapturing serves body for any request. When auth is non-nil it records
// the request's Authorization header into *auth.
func newServerCapturing(t *testing.T, auth *string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestPathBase(t *testing.T) {
	cases := map[string]string{
		"shuck":            "shuck",
		"dir/shuck":        "shuck",
		"a/b/c/shuck.exe":  "shuck.exe",
		"trailing/":        "",
		"":                 "",
		"nested/deep/file": "file",
	}
	for in, want := range cases {
		if got := pathBase(in); got != want {
			t.Errorf("pathBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractZipMissingBinary(t *testing.T) {
	// A zip with a differently named entry: the binary is not found.
	archive := makeZip(t, "notshuck", []byte("x"))
	if _, err := extractZip(archive, "shuck.exe"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestExtractZipNestedPath(t *testing.T) {
	// The binary lives under a directory prefix; pathBase must still match it.
	archive := makeZip(t, "shuck_1.0.0_windows_amd64/shuck.exe", []byte("MZ payload"))
	got, err := extractZip(archive, "shuck.exe")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if string(got) != "MZ payload" {
		t.Errorf("extractZip = %q", got)
	}
}

func TestExtractZipSkipsDirEntry(t *testing.T) {
	// A directory entry named like the binary is not a regular file, so it is
	// skipped and the real file (added after) is returned.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("shuck.exe/"); err != nil { // directory entry
		t.Fatal(err)
	}
	w, err := zw.Create("shuck.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("real")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := extractZip(buf.Bytes(), "shuck.exe")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if string(got) != "real" {
		t.Errorf("extractZip = %q, want real", got)
	}
}

func TestExtractZipCorruptData(t *testing.T) {
	if _, err := extractZip([]byte("not a zip at all"), "shuck.exe"); err == nil {
		t.Error("expected error opening corrupt zip")
	}
}

func TestExtractTarGzCorruptData(t *testing.T) {
	if _, err := extractTarGz([]byte("not gzip"), "shuck"); err == nil {
		t.Error("expected error opening corrupt gzip")
	}
}

func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	if !sameDir(dir, dir) {
		t.Errorf("sameDir(%q, %q) = false, want true", dir, dir)
	}
	if sameDir(dir, "") {
		t.Error("sameDir with empty b should be false")
	}
	if sameDir(dir, filepath.Join(dir, "sub")) {
		t.Error("different dirs should not be same")
	}
	// Relative vs absolute forms of the same path resolve equal.
	if !sameDir(".", ".") {
		t.Error(`sameDir(".", ".") should be true`)
	}
}

func TestGoBinDirsGOBIN(t *testing.T) {
	t.Setenv("GOBIN", "/my/gobin")
	dirs := goBinDirs()
	if len(dirs) != 1 || dirs[0] != "/my/gobin" {
		t.Errorf("goBinDirs with GOBIN = %v, want [/my/gobin]", dirs)
	}
}

func TestGoBinDirsGOPATHList(t *testing.T) {
	t.Setenv("GOBIN", "")
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("GOPATH", a+string(os.PathListSeparator)+b)
	dirs := goBinDirs()
	want := map[string]bool{filepath.Join(a, "bin"): true, filepath.Join(b, "bin"): true}
	if len(dirs) != 2 {
		t.Fatalf("goBinDirs = %v, want 2 entries", dirs)
	}
	for _, d := range dirs {
		if !want[d] {
			t.Errorf("unexpected dir %q in %v", d, dirs)
		}
	}
}

func TestGoBinDirsDefaultGOPATH(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	dirs := goBinDirs()
	want := filepath.Join(home, "go", "bin")
	found := false
	for _, d := range dirs {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("goBinDirs = %v, want it to contain %q", dirs, want)
	}
}

func TestReplaceRunningNonexistentDir(t *testing.T) {
	// The install directory does not exist, so CreateTemp fails.
	missing := filepath.Join(t.TempDir(), "does-not-exist", "shuck")
	if err := ReplaceRunning(missing, []byte("x")); err == nil {
		t.Error("expected error replacing a binary in a missing directory")
	}
}

func TestReplaceRunningReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "shuck")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := ReplaceRunning(exe, []byte("new"))
	if err == nil {
		t.Fatal("expected error writing into a read-only directory")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected a permission-denied message, got %v", err)
	}
}

func TestGetSetsAuthHeader(t *testing.T) {
	c := New("tok-123")
	var gotAuth string
	srv := newServerCapturing(t, &gotAuth, `{"tag_name":"v1.0.0"}`)
	defer srv.Close()
	c.APIBase = srv.URL
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
}

func TestLatestEmptyTagName(t *testing.T) {
	c := New("")
	srv := newServerCapturing(t, nil, `{"tag_name":""}`)
	defer srv.Close()
	c.APIBase = srv.URL
	if _, err := c.Latest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no tag_name") {
		t.Errorf("expected no-tag_name error, got %v", err)
	}
}

func TestLatestBadJSON(t *testing.T) {
	c := New("")
	srv := newServerCapturing(t, nil, `{not json`)
	defer srv.Close()
	c.APIBase = srv.URL
	if _, err := c.Latest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestDownloadArchiveMissing(t *testing.T) {
	// No assets served: the archive download 404s.
	srv := serveAssets(t, map[string][]byte{})
	defer srv.Close()
	c := New("")
	c.DownloadBase = srv.URL
	if _, err := c.Download(context.Background(), "v1.0.0", "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "download") {
		t.Errorf("expected download error, got %v", err)
	}
}

func TestDownloadChecksumsMissing(t *testing.T) {
	archive := makeTarGz(t, "shuck", []byte("bin"))
	srv := serveAssets(t, map[string][]byte{
		"shuck_1.0.0_linux_amd64.tar.gz": archive,
		// checksums.txt deliberately absent
	})
	defer srv.Close()
	c := New("")
	c.DownloadBase = srv.URL
	if _, err := c.Download(context.Background(), "v1.0.0", "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "checksums.txt") {
		t.Errorf("expected checksums download error, got %v", err)
	}
}

func TestGetContextCancelled(t *testing.T) {
	c := New("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Latest(ctx); err == nil {
		t.Error("expected error from a cancelled context")
	}
}
