package classify

import (
	"testing"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		excerpt    string
		conclusion string
		want       model.FailureClass
	}{
		{
			name:       "timed-out job conclusion wins over excerpt",
			command:    "go test ./...",
			excerpt:    "--- FAIL: TestThing",
			conclusion: "timed_out",
			want:       model.ClassTimeout,
		},
		{
			name:    "timeout from excerpt beats test command",
			command: "go test ./...",
			excerpt: "panic: test timed out after 10m0s",
			want:    model.ClassTimeout,
		},
		{
			name:    "context deadline is a timeout",
			command: "./run-integration.sh",
			excerpt: "Error: context deadline exceeded",
			want:    model.ClassTimeout,
		},
		{
			name:    "oom signal killed",
			command: "go test -race ./...",
			excerpt: "signal: killed\nmake: *** [test] Error 1",
			want:    model.ClassOOM,
		},
		{
			name:    "java OOM",
			command: "./gradlew test",
			excerpt: "java.lang.OutOfMemoryError: Java heap space",
			want:    model.ClassOOM,
		},
		{
			name:    "infra dns failure",
			command: "actions/checkout@v4",
			excerpt: "fatal: unable to access: Could not resolve host: github.com",
			want:    model.ClassInfra,
		},
		{
			name:    "infra rate limit",
			command: "docker pull ghcr.io/x/y",
			excerpt: "Error response: 429 Too Many Requests",
			want:    model.ClassInfra,
		},
		{
			name:    "lint by command golangci-lint",
			command: "golangci-lint run",
			excerpt: "main.go:10:2: ineffassign",
			want:    model.ClassLint,
		},
		{
			name:    "lint by command eslint",
			command: "npx eslint .",
			excerpt: "error  Unexpected console statement",
			want:    model.ClassLint,
		},
		{
			name:    "test by command go test",
			command: "go test ./...",
			excerpt: "--- FAIL: TestFoo (0.00s)\nFAIL",
			want:    model.ClassTest,
		},
		{
			name:    "test by command pytest",
			command: "pytest -q",
			excerpt: "1 failed, 3 passed",
			want:    model.ClassTest,
		},
		{
			name:    "build by command go build",
			command: "go build ./...",
			excerpt: "main.go:3:8: undefined: foo",
			want:    model.ClassBuild,
		},
		{
			name:    "build by command tsc",
			command: "tsc --noEmit",
			excerpt: "src/x.ts(1,1): error TS2322",
			want:    model.ClassBuild,
		},
		{
			name:    "lint from text when command unknown",
			command: "make check",
			excerpt: "gofmt found unformatted files; would reformat main.go",
			want:    model.ClassLint,
		},
		{
			name:    "test from text when command unknown",
			command: "make verify",
			excerpt: "--- FAIL: TestParse\nFAIL\texample.com/pkg\t0.1s",
			want:    model.ClassTest,
		},
		{
			name:    "build from text when command unknown",
			command: "make all",
			excerpt: "./x.go:9:2: cannot find package \"y\"",
			want:    model.ClassBuild,
		},
		{
			name:    "unclassified",
			command: "echo hi",
			excerpt: "something inexplicable happened",
			want:    model.ClassUnknown,
		},
		{
			name:    "empty",
			command: "",
			excerpt: "",
			want:    model.ClassUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := model.FailedStep{Command: tt.command, Excerpt: tt.excerpt}
			if got := Classify(step, tt.conclusion); got != tt.want {
				t.Errorf("Classify(%q / %q, %q) = %q, want %q",
					tt.command, tt.excerpt, tt.conclusion, got, tt.want)
			}
		})
	}
}
