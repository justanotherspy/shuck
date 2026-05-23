package release

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ExecutablePath returns the absolute, symlink-resolved path of the running
// binary — the file `shuck upgrade` replaces in place.
func ExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe, nil
}

// ManagedExternally reports whether the binary at exePath looks like it was
// installed by another tool that should own upgrades — currently the Go
// toolchain (`go install`, which drops binaries in GOBIN or $GOPATH/bin). When
// it returns true, shuck refuses to overwrite the binary and points the user at
// that tool instead. The returned name is the tool to mention.
func ManagedExternally(exePath string) (tool string, yes bool) {
	dir := filepath.Dir(exePath)
	for _, d := range goBinDirs() {
		if sameDir(dir, d) {
			return "go install", true
		}
	}
	return "", false
}

// goBinDirs lists the directories `go install` writes binaries to, derived from
// the environment without shelling out to `go` (which may be absent for a
// release-installed shuck): GOBIN if set, otherwise <GOPATH>/bin for each GOPATH
// entry, defaulting GOPATH to ~/go.
func goBinDirs() []string {
	if b := os.Getenv("GOBIN"); b != "" {
		return []string{b}
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	var dirs []string
	for _, p := range filepath.SplitList(gopath) {
		if p != "" {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	}
	return dirs
}

func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	ap, err1 := filepath.Abs(a)
	bp, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ap == bp
}

// ReplaceRunning atomically replaces the binary at exePath with newBinary,
// preserving its location (mimicking where install.sh placed it). It writes a
// temp file in the same directory, makes it executable, and renames it over the
// target, so a crash can never leave a half-written binary on PATH. A
// non-writable install directory yields an actionable error.
func ReplaceRunning(exePath string, newBinary []byte) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".shuck-upgrade-*")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("cannot write to %s (permission denied); re-run with sufficient permissions (e.g. sudo) or reinstall shuck there", dir)
		}
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(newBinary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close new binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("make new binary executable: %w", err)
	}

	if err := os.Rename(tmpName, exePath); err != nil {
		// Windows refuses to rename over a running executable; move the current
		// one aside first, then put the new one in place.
		if runtime.GOOS == "windows" {
			old := exePath + ".old"
			_ = os.Remove(old)
			if os.Rename(exePath, old) == nil && os.Rename(tmpName, exePath) == nil {
				return nil
			}
		}
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", exePath, err)
	}
	return nil
}
