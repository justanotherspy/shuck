// Package logs parses GitHub Actions job logs into per-step sections and
// extracts the high-signal error excerpt from a section's output.
package logs

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// tsPrefix matches the ISO-8601 timestamp GitHub prepends to every log line,
// e.g. "2024-01-02T03:04:05.1234567Z ".
var tsPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)

// actionRef matches an action reference like "actions/checkout@v4" or
// "owner/repo/sub@sha", used to classify a step's command.
var actionRef = regexp.MustCompile(`^[\w.-]+/[\w./-]+@\S+$`)

// stepMetaKey matches the step-config keys GitHub echoes right after a run
// step's command: "shell:", "env:", "with:". They mark the end of the echoed
// shell script, so FullCommand stops at the first one for a run step.
var stepMetaKey = regexp.MustCompile(`^(?:shell|env|with):`)

// Section is one step's region of a job log: from one ##[group] marker up to the
// next. Pre holds the echoed command/env (between group and endgroup); Body holds
// the actual output that follows ##[endgroup].
type Section struct {
	Header   string // text after "##[group]", e.g. `Run actions/checkout@v4`
	Pre      []string
	Body     []string
	HasError bool
}

// Command returns the step's command without the leading "Run " that GitHub adds.
// It is the single ##[group] header line, so for a multi-line shell run it holds
// only the first line; FullCommand recovers the rest.
func (s Section) Command() string {
	return strings.TrimPrefix(s.Header, "Run ")
}

// FullCommand returns the step's complete command. For a shell run GitHub echoes
// the whole (possibly multi-line) script in Pre, ahead of the shell:/env:
// metadata; FullCommand rejoins those echoed lines. For an action it returns the
// owner/action@ref header line followed by the inputs GitHub echoed into Pre —
// the with: block and any env: — so the call's arguments are visible too.
// (Secrets are already masked as *** in the logs.)
func (s Section) FullCommand() string {
	if s.Kind() == model.KindAction {
		if len(s.Pre) == 0 {
			return s.Command()
		}
		return s.Command() + "\n" + strings.Join(s.Pre, "\n")
	}
	var script []string
	for _, l := range s.Pre {
		if stepMetaKey.MatchString(l) {
			break
		}
		script = append(script, l)
	}
	if len(script) == 0 {
		return s.Command()
	}
	return strings.Join(script, "\n")
}

// DefaultMaxCommandLines is the default cap on how many lines of a failed step's
// command shuck shows; longer commands are truncated. Shared by the CLI flag and
// the MCP server so the two never drift. 0 would mean no limit.
const DefaultMaxCommandLines = 30

// ClampCommand limits cmd to at most maxLines lines. A non-positive maxLines
// means no limit. When cmd is longer, the first maxLines lines are kept and a
// trailing "… (N more lines) …" marker reports how many were dropped.
func ClampCommand(cmd string, maxLines int) string {
	if maxLines <= 0 || cmd == "" {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	if len(lines) <= maxLines {
		return cmd
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… (%d more lines) …", len(lines)-maxLines)
}

// Kind classifies the section's command as an action invocation or a shell run.
func (s Section) Kind() model.StepKind {
	cmd := s.Command()
	if cmd == "" {
		return model.KindUnknown
	}
	// A single-line "owner/action@ref" is an action; anything else is a shell run.
	if !strings.ContainsAny(cmd, " \t\n") && actionRef.MatchString(cmd) {
		return model.KindAction
	}
	return model.KindBash
}

// Parse splits a raw job log into ordered sections, stripping timestamp prefixes.
// Lines before the first ##[group] are collected into a leading section with an
// empty header so nothing is lost.
func Parse(raw string) []Section {
	var sections []Section
	cur := Section{}
	started := false // seen the first group yet?
	inGroup := false // between ##[group] and ##[endgroup]
	haveCur := false // cur holds content worth keeping

	flush := func() {
		if haveCur {
			sections = append(sections, cur)
		}
	}

	for _, raw := range strings.Split(raw, "\n") {
		line := tsPrefix.ReplaceAllString(raw, "")

		switch {
		case strings.HasPrefix(line, "##[group]"):
			flush()
			cur = Section{Header: strings.TrimPrefix(line, "##[group]")}
			haveCur = true
			started = true
			inGroup = true
			continue
		case strings.HasPrefix(line, "##[endgroup]"):
			inGroup = false
			continue
		}

		if strings.Contains(line, "##[error]") {
			cur.HasError = true
			haveCur = true
		}
		if !started {
			haveCur = true
		}

		if inGroup {
			cur.Pre = append(cur.Pre, line)
		} else {
			cur.Body = append(cur.Body, line)
		}
	}
	flush()
	return sections
}

// ErrorSections returns only the sections whose output contains an error marker.
func ErrorSections(sections []Section) []Section {
	var out []Section
	for _, s := range sections {
		if s.HasError {
			out = append(out, s)
		}
	}
	return out
}
