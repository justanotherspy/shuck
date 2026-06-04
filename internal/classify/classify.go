// Package classify assigns a coarse, heuristic FailureClass to a failed CI
// step from its command and extracted error excerpt. The class is a routing
// hint — "is this a code fix or a re-run?" — never an authoritative verdict, so
// it errs toward model.ClassUnknown rather than guessing wrong.
package classify

import (
	"regexp"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// Operational signals win over tool/text signals: a test run that timed out or
// was OOM-killed should read as "re-run", not "fix the test". These match the
// excerpt (and command) case-insensitively.
var (
	timeoutRe = regexp.MustCompile(`(?i)\b(timed out|timeout|context deadline exceeded|deadline exceeded|has timed out|has exceeded the maximum execution time)\b`)
	oomRe     = regexp.MustCompile(`(?i)(out of memory|oomkilled|signal: killed|cannot allocate memory|runtime: out of memory|java\.lang\.OutOfMemoryError|killed process|fatal error: runtime: out of memory)`)
	infraRe   = regexp.MustCompile(`(?i)(could not resolve host|connection refused|connection reset|i/o timeout|tls handshake timeout|econnreset|etimedout|503 service|429 too many requests|secondary rate limit|no space left on device|failed to download action|unable to resolve action|error: the runner has received a shutdown signal|received request to deprovision)`)
)

// Tool signals keyed off the step command (the strongest, least ambiguous
// hint). Each maps a set of substrings to the class they imply.
var (
	lintCmd  = []string{"golangci-lint", "gofmt", "goimports", "go vet", "staticcheck", "eslint", "prettier", "ruff", "flake8", "pylint", "black ", "rubocop", "shellcheck", "actionlint", "clippy", "mypy", "stylelint", "biome"}
	testCmd  = []string{"go test", "gotestsum", "pytest", "jest", "vitest", "mocha", "cargo test", "rspec", "phpunit", "dotnet test", "ctest", "npm test", "npm run test", "yarn test", "pnpm test", "go-junit-report"}
	buildCmd = []string{"go build", "go install", "cargo build", "tsc", "webpack", "rollup", "vite build", "npm run build", "yarn build", "make build", "cmake", "gradle build", "mvn package", "mvn compile", "dotnet build", "cc ", "gcc ", "clang ", "g++ "}
)

// Text signals in the excerpt, used only when the command was inconclusive.
var (
	// The generic "file:line:col:" form is deliberately not a lint signal: it is
	// just as common in compiler output, so it would steal build failures.
	lintText  = regexp.MustCompile(`(?i)(\blint\b|lint(er|ing)|gofmt|not formatted|would reformat|::error.*eslint)`)
	testText  = regexp.MustCompile(`(?i)(--- FAIL|^FAIL\b|\bFAILED\b|assertionerror|tests? failed|\d+ (failed|failing)|expect\(.*\)\.to|test suite failed)`)
	buildText = regexp.MustCompile(`(?i)(cannot find package|undefined:|undeclared name|syntax error|compilation (failed|error)|cannot compile|build failed|error: cannot find|ld: |linker command failed|no such module|unresolved import|type error ts\d+)`)
)

// Classify returns a heuristic FailureClass for a failed step. jobConclusion is
// the job's overall conclusion ("timed_out", "failure", …); a timed-out job is
// classified as a timeout regardless of its excerpt. The precedence is
// deliberate: operational causes first (they suggest a re-run), then the
// command's tool, then excerpt keywords.
func Classify(step model.FailedStep, jobConclusion string) model.FailureClass {
	if jobConclusion == "timed_out" {
		return model.ClassTimeout
	}

	hay := strings.ToLower(step.Command + "\n" + step.Excerpt)
	switch {
	case timeoutRe.MatchString(hay):
		return model.ClassTimeout
	case oomRe.MatchString(hay):
		return model.ClassOOM
	case infraRe.MatchString(hay):
		return model.ClassInfra
	}

	cmd := strings.ToLower(step.Command)
	if cmd != "" {
		switch {
		case containsAny(cmd, lintCmd):
			return model.ClassLint
		case containsAny(cmd, testCmd):
			return model.ClassTest
		case containsAny(cmd, buildCmd):
			return model.ClassBuild
		}
	}

	switch {
	case lintText.MatchString(step.Excerpt):
		return model.ClassLint
	case testText.MatchString(step.Excerpt):
		return model.ClassTest
	case buildText.MatchString(step.Excerpt):
		return model.ClassBuild
	}
	return model.ClassUnknown
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
