// Package render formats an inspection Report into shuck's high-signal text
// output.
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/justanotherspy/shuck/internal/model"
)

// Report writes the human-readable summary for r to w.
func Report(w io.Writer, r *model.Report) {
	fmt.Fprintf(w, "%s/%s PR #%d — %s   (commit %s)\n",
		r.PR.Owner, r.PR.Repo, r.PR.Number, r.PR.Title, shortSHA(r.PR.HeadSHA))

	if !r.HasFailures() {
		if r.IsTerminal() {
			fmt.Fprintf(w, "\n✓ all checks passing for PR #%d\n", r.PR.Number)
		} else {
			fmt.Fprintf(w, "\n⏳ no failures yet — some checks are still running\n")
		}
		writeRunning(w, r.RunningJobs)
		return
	}

	for _, job := range r.FailedJobs {
		writeJob(w, job)
	}
	writeOther(w, r.OtherChecks)
	writeRunning(w, r.RunningJobs)
}

func writeJob(w io.Writer, job model.JobResult) {
	fmt.Fprintf(w, "\nWorkflow: %s (%s)\n", job.WorkflowName, job.WorkflowPath)
	fmt.Fprintf(w, "Job: %s  [%s]\n", job.Name, job.Conclusion)
	fmt.Fprintln(w, "Steps:")
	for _, s := range job.Steps {
		fmt.Fprintf(w, "  %d. %s (%s)\n", s.Number, s.Name, stepState(s))
	}
	for _, fs := range job.FailedSteps {
		writeFailedStep(w, fs)
	}
}

func writeFailedStep(w io.Writer, fs model.FailedStep) {
	fmt.Fprintf(w, "\n  ▸ Step %d — %s (failed)\n", fs.Number, fs.Name)
	if fs.Command != "" {
		fmt.Fprintln(w, "    Step command:")
		fmt.Fprintf(w, "      * %s:\n", commandLabel(fs.Kind))
		writeFenced(w, "        ", fs.Command)
	}
	fmt.Fprintln(w, "    error logs:")
	writeFenced(w, "      ", fs.Excerpt)
}

func writeOther(w io.Writer, checks []model.OtherCheck) {
	if len(checks) == 0 {
		return
	}
	fmt.Fprintln(w, "\nOther checks (no logs available):")
	for _, c := range checks {
		if c.URL != "" {
			fmt.Fprintf(w, "  ✗ %s (%s) — %s\n", c.Name, c.Conclusion, c.URL)
		} else {
			fmt.Fprintf(w, "  ✗ %s (%s)\n", c.Name, c.Conclusion)
		}
	}
}

func writeRunning(w io.Writer, jobs []model.RunningJob) {
	if len(jobs) == 0 {
		return
	}
	fmt.Fprintln(w, "\nStill running:")
	for _, j := range jobs {
		fmt.Fprintf(w, "  ⏳ Job %q (%s)\n", j.Name, j.Status)
	}
}

func writeFenced(w io.Writer, indent, content string) {
	fmt.Fprintf(w, "%s```\n", indent)
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
	fmt.Fprintf(w, "%s```\n", indent)
}

func commandLabel(kind model.StepKind) string {
	switch kind {
	case model.KindAction:
		return "action called"
	case model.KindBash:
		return "bash run"
	default:
		return "command"
	}
}

func stepState(s model.StepOverview) string {
	if s.Conclusion != "" {
		return s.Conclusion
	}
	return s.Status
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
