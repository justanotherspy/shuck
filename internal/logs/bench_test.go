package logs

import (
	"os"
	"path/filepath"
	"testing"
)

// benchFixture loads the shared job-log fixture once for the benchmarks below.
func benchFixture(b *testing.B) string {
	b.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "job_failure.log"))
	if err != nil {
		b.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// BenchmarkParse measures splitting a raw job log into per-step sections.
func BenchmarkParse(b *testing.B) {
	raw := benchFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = Parse(raw)
	}
}

// BenchmarkExtract measures reducing a section's body to its error excerpt.
func BenchmarkExtract(b *testing.B) {
	raw := benchFixture(b)
	secs := Parse(raw)
	var body []string
	for _, s := range secs {
		if s.HasError {
			body = s.Body
			break
		}
	}
	opts := DefaultOptions()
	b.ReportAllocs()
	for b.Loop() {
		_ = Extract(body, opts)
	}
}
