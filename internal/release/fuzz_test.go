package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// fuzzMaxArchive caps fuzzed archive inputs to keep the zip/tar parsers' own
// work bounded. It does NOT bound the decompressed output — DEFLATE expands up
// to ~1032:1, so a 32 KiB input can inflate to ~33 MiB. fuzzMaxBinary is what
// actually keeps each execution cheap, capping the extracted bytes so neither a
// successful read nor an over-limit rejection touches more than a few KiB.
// Without it a deflate bomb decompresses tens of MiB per execution, and the
// resulting allocation churn stalls the fuzzing workers until the run's deadline
// trips ("context deadline exceeded"). It is deliberately far below the
// production maxBinarySize: the goal here is to exercise extraction's cap logic
// on adversarial input, not to validate the production ceiling's value.
const (
	fuzzMaxArchive = 32 << 10
	fuzzMaxBinary  = 64 << 10
)

// FuzzExtractTarGz exercises the tar.gz binary extractor with arbitrary bytes.
// It must never panic, and a successful extraction must come from an archive
// that gzip+tar can actually read.
func FuzzExtractTarGz(f *testing.F) {
	f.Add([]byte{}, "shuck")
	f.Add([]byte("not a gzip stream"), "shuck")
	f.Add(buildTarGz(f, "shuck", []byte("#!/bin/sh\necho hi\n")), "shuck")
	f.Add(buildTarGz(f, "dir/shuck", []byte("binary-bytes")), "shuck")
	f.Add(buildTarGz(f, "other", []byte("nope")), "shuck")

	f.Fuzz(func(t *testing.T, data []byte, binName string) {
		if len(data) > fuzzMaxArchive {
			return
		}
		bin, err := extractTarGz(data, binName, fuzzMaxBinary)
		if err == nil && bin == nil {
			t.Fatalf("extractTarGz returned no error and no binary for %q", binName)
		}
	})
}

// FuzzExtractZip exercises the zip binary extractor with arbitrary bytes. It
// must never panic.
func FuzzExtractZip(f *testing.F) {
	f.Add([]byte{}, "shuck")
	f.Add([]byte("PK\x03\x04 not really a zip"), "shuck.exe")
	f.Add(buildZip(f, "shuck.exe", []byte("MZ binary")), "shuck.exe")
	f.Add(buildZip(f, "dir/shuck.exe", []byte("MZ binary")), "shuck.exe")

	f.Fuzz(func(t *testing.T, data []byte, binName string) {
		if len(data) > fuzzMaxArchive {
			return
		}
		bin, err := extractZip(data, binName, fuzzMaxBinary)
		if err == nil && bin == nil {
			t.Fatalf("extractZip returned no error and no binary for %q", binName)
		}
	})
}

// FuzzVerifyChecksum exercises the checksums.txt verifier. It must never panic;
// it must accept exactly when the checksums text lists the archive name next to
// the SHA-256 of the data, and it must always reject a wrong digest.
func FuzzVerifyChecksum(f *testing.F) {
	f.Add("shuck_linux_amd64.tar.gz", []byte("archive bytes"), "deadbeef  shuck_linux_amd64.tar.gz\n")
	f.Add("a.zip", []byte{}, "")
	f.Add("a.zip", []byte("x"), "not checksum lines at all")

	f.Fuzz(func(t *testing.T, archive string, data []byte, checksums string) {
		err := verifyChecksum(archive, data, []byte(checksums))

		sum := sha256.Sum256(data)
		want := hex.EncodeToString(sum[:])

		// Find the first line naming the archive: that line decides the outcome.
		for line := range strings.SplitSeq(checksums, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[1] != archive {
				continue
			}
			if matched := fields[0] == want; matched != (err == nil) {
				t.Fatalf("verifyChecksum(%q) = %v, but digest match = %v (line %q)", archive, err, matched, line)
			}
			return
		}
		// No line names the archive: verification must fail closed.
		if err == nil {
			t.Fatalf("verifyChecksum(%q) accepted an archive missing from checksums.txt", archive)
		}
	})
}

// FuzzReleaseCompare checks the version-comparison helpers used by
// `shuck version --check`: IsSemver must never panic, and Compare must be
// reflexive and antisymmetric.
func FuzzReleaseCompare(f *testing.F) {
	f.Add("v1.2.3", "v1.2.4")
	f.Add("1.0.0", "v1.0.0")
	f.Add("dev", "v0.5.0")
	f.Add("v2.0.0-rc.1+meta", "v2.0.0")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, a, b string) {
		_ = IsSemver(a)
		_ = IsSemver(b)

		if got := Compare(a, a); got != 0 {
			t.Fatalf("Compare(%q, %q) = %d, want 0", a, a, got)
		}
		ab, ba := Compare(a, b), Compare(b, a)
		if ab != -ba {
			t.Fatalf("Compare not antisymmetric: Compare(%q,%q)=%d, Compare(%q,%q)=%d", a, b, ab, b, a, ba)
		}
	})
}

// buildTarGz assembles a one-file .tar.gz archive for the seed corpus.
func buildTarGz(f *testing.F, name string, content []byte) []byte {
	f.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		f.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		f.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		f.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		f.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// buildZip assembles a one-file .zip archive for the seed corpus.
func buildZip(f *testing.F, name string, content []byte) []byte {
	f.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		f.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		f.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		f.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
