package release

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceRunning(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "shuck")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newBin := []byte("new binary contents")
	if err := ReplaceRunning(exe, newBin); err != nil {
		t.Fatalf("ReplaceRunning: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBin) {
		t.Errorf("contents = %q, want %q", got, newBin)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("replaced binary is not executable: mode %v", info.Mode())
	}

	// No leftover temp files should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "shuck" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contents = %v, want only [shuck]", names)
	}
}

func TestManagedExternally(t *testing.T) {
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)

	inGobin := filepath.Join(gobin, "shuck")
	if tool, yes := ManagedExternally(inGobin); !yes || tool != "go install" {
		t.Errorf("ManagedExternally(%q) = (%q, %v), want (\"go install\", true)", inGobin, tool, yes)
	}

	elsewhere := filepath.Join(t.TempDir(), "shuck")
	if _, yes := ManagedExternally(elsewhere); yes {
		t.Errorf("ManagedExternally(%q) = true, want false", elsewhere)
	}
}

func TestManagedExternallyGopathBin(t *testing.T) {
	t.Setenv("GOBIN", "")
	gopath := t.TempDir()
	t.Setenv("GOPATH", gopath)

	inGopathBin := filepath.Join(gopath, "bin", "shuck")
	if _, yes := ManagedExternally(inGopathBin); !yes {
		t.Errorf("ManagedExternally(%q) = false, want true (under GOPATH/bin)", inGopathBin)
	}
}

func TestExecutablePath(t *testing.T) {
	got, err := ExecutablePath()
	if err != nil {
		t.Fatalf("ExecutablePath: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Errorf("ExecutablePath = %q, want a non-empty absolute path", got)
	}
}
