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
	writeHeader(w, r)

	if r.ReviewsOnly {
		if len(r.Reviews) == 0 {
			fmt.Fprintln(w, "\n(no reviews)")
			return
		}
		writeReviews(w, r.Reviews)
		return
	}

	writeSummary(w, r)

	if !r.HasFailures() && len(r.CancelledJobs) == 0 {
		if r.IsTerminal() {
			fmt.Fprintf(w, "\n✓ %s\n", allClearLabel(r))
		} else {
			fmt.Fprintf(w, "\n⏳ no failures yet — some checks are still running\n")
		}
		writeRunning(w, r.RunningJobs)
		writeReviews(w, r.Reviews)
		return
	}

	for _, job := range r.FailedJobs {
		writeJob(w, job)
	}
	writeOther(w, r.OtherChecks)
	writeCancelled(w, r.CancelledJobs)
	writeRunning(w, r.RunningJobs)
	writeReviews(w, r.Reviews)
}

// writeSummary prints an upfront count of what was found and, when failures
// coexist with still-running jobs, a banner that the view may be incomplete.
func writeSummary(w io.Writer, r *model.Report) {
	var parts []string
	if n := len(r.FailedJobs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", n))
	}
	if n := len(r.CancelledJobs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled", n))
	}
	if n := len(r.OtherChecks); n > 0 {
		parts = append(parts, fmt.Sprintf("%d other failed", n))
	}
	if n := len(r.RunningJobs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d running", n))
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintf(w, "\nSummary: %s\n", strings.Join(parts, ", "))
	if r.HasFailures() && len(r.RunningJobs) > 0 {
		fmt.Fprintf(w, "⚠ %d still running — failures shown may be incomplete\n", len(r.RunningJobs))
	}
}

func writeJob(w io.Writer, job model.JobResult) {
	fmt.Fprintf(w, "\nWorkflow: %s (%s)\n", job.WorkflowName, job.WorkflowPath)
	fmt.Fprintf(w, "Job: %s  [%s]\n", job.Name, job.Conclusion)
	fmt.Fprintln(w, "Steps:")
	for _, s := range job.Steps {
		fmt.Fprintf(w, "  %d. %s (%s)\n", s.Number, s.Name, stepState(s))
	}
	cancelled := model.IsCancelledConclusion(job.Conclusion)
	for _, fs := range job.FailedSteps {
		writeFailedStep(w, fs, cancelled)
	}
}

func writeFailedStep(w io.Writer, fs model.FailedStep, cancelled bool) {
	verdict, logsLabel := "failed", "error logs:"
	if cancelled {
		verdict, logsLabel = "cancelled", "logs before cancellation:"
	}
	fmt.Fprintf(w, "\n  ▸ Step %d — %s (%s)\n", fs.Number, fs.Name, verdict)
	if fs.Command != "" {
		fmt.Fprintln(w, "    Step command:")
		fmt.Fprintf(w, "      * %s:\n", commandLabel(fs.Kind))
		writeFenced(w, "        ", fs.Command)
	}
	fmt.Fprintf(w, "    %s\n", logsLabel)
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

// writeCancelled renders the cancelled jobs. Jobs whose logs were drilled show
// the interrupted step and its last output like a failed job; jobs with no log
// detail (e.g. cancelled before the runner started) fall back to a one-line
// listing so they are still not silently dropped.
func writeCancelled(w io.Writer, jobs []model.JobResult) {
	if len(jobs) == 0 {
		return
	}
	var bare []model.JobResult
	for _, j := range jobs {
		if len(j.FailedSteps) > 0 {
			writeJob(w, j)
		} else {
			bare = append(bare, j)
		}
	}
	if len(bare) == 0 {
		return
	}
	fmt.Fprintln(w, "\nCancelled (no logs available):")
	for _, j := range bare {
		if j.WorkflowName != "" {
			fmt.Fprintf(w, "  ⊘ %s (%s)\n", j.Name, j.WorkflowName)
		} else {
			fmt.Fprintf(w, "  ⊘ %s\n", j.Name)
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

func writeReviews(w io.Writer, reviews []model.Review) {
	if len(reviews) == 0 {
		return
	}
	fmt.Fprintln(w, "\nReviews:")
	for _, rv := range reviews {
		fmt.Fprintf(w, "  %s %s — %s\n", reviewSymbol(rv.State), reviewStateLabel(rv.State), authorLabel(rv.Author, rv.AuthorType))
		for _, line := range bodyLines(rv.Body) {
			fmt.Fprintf(w, "      %s\n", line)
		}
		for _, t := range rv.Threads {
			writeThread(w, t)
		}
	}
}

func writeThread(w io.Writer, t model.ReviewThread) {
	loc := t.Path
	if t.Line > 0 {
		loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
	}
	if t.Collapsed {
		fmt.Fprintf(w, "      ▸ %s  (%s)\n", loc, t.CollapseReason)
		return
	}
	fmt.Fprintf(w, "      ▸ %s  (%s)\n", loc, commentCount(t.TotalComments))
	for _, c := range t.Comments {
		lines := bodyLines(c.Body)
		head := ""
		if len(lines) > 0 {
			head = lines[0]
		}
		fmt.Fprintf(w, "          %s: %s\n", authorLabel(c.Author, c.AuthorType), head)
		if len(lines) > 1 {
			for _, line := range lines[1:] {
				fmt.Fprintf(w, "            %s\n", line)
			}
		}
	}
	if t.HiddenComments > 0 {
		fmt.Fprintf(w, "          … %d more comment%s\n", t.HiddenComments, plural(t.HiddenComments))
	}
}

func commentCount(n int) string {
	return fmt.Sprintf("%d comment%s", n, plural(n))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// bodyLines splits a comment/review body into trimmed, non-empty display lines.
func bodyLines(body string) []string {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if s := strings.TrimRight(line, "\r "); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func authorLabel(author string, kind model.AuthorType) string {
	switch kind {
	case model.AuthorBot:
		return author + " [bot]"
	case model.AuthorAI:
		return author + " [AI]"
	default:
		return author
	}
}

func reviewSymbol(state string) string {
	switch state {
	case "approved":
		return "✔"
	case "changes_requested":
		return "✗"
	case "dismissed":
		return "⊘"
	default:
		return "💬"
	}
}

func reviewStateLabel(state string) string {
	switch state {
	case "approved":
		return "approved"
	case "changes_requested":
		return "changes requested"
	case "dismissed":
		return "dismissed"
	default:
		return "commented"
	}
}

func writeFenced(w io.Writer, indent, content string) {
	fmt.Fprintf(w, "%s```\n", indent)
	for line := range strings.SplitSeq(strings.TrimRight(content, "\n"), "\n") {
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

// writeHeader prints the one-line target header: a PR line for PR targets, or a
// run/job line when the report came from a GitHub Actions URL.
func writeHeader(w io.Writer, r *model.Report) {
	if r.Run == nil {
		fmt.Fprintf(w, "%s/%s PR #%d — %s   (commit %s)\n",
			r.PR.Owner, r.PR.Repo, r.PR.Number, r.PR.Title, shortSHA(r.PR.HeadSHA))
		return
	}
	rn := r.Run
	title := rn.Title
	if title == "" {
		title = rn.WorkflowName
	}
	fmt.Fprintf(w, "%s/%s %s — %s   (commit %s)\n",
		rn.Owner, rn.Repo, runLabel(rn), title, shortSHA(rn.HeadSHA))
}

// allClearLabel is the trailing clause of the "✓ …" line when nothing failed.
func allClearLabel(r *model.Report) string {
	if r.Run != nil {
		return "no failures in " + runLabel(r.Run)
	}
	return fmt.Sprintf("all checks passing for PR #%d", r.PR.Number)
}

// runLabel names a run/job target for headers and messages, e.g. "run 123",
// "job 456 (run 123)", or "run 123 (attempt 2)".
func runLabel(rn *model.RunInfo) string {
	if rn.JobID != 0 {
		return fmt.Sprintf("job %d (run %d)", rn.JobID, rn.RunID)
	}
	if rn.Attempt != 0 {
		return fmt.Sprintf("run %d (attempt %d)", rn.RunID, rn.Attempt)
	}
	return fmt.Sprintf("run %d", rn.RunID)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
