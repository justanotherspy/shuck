package distil

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultContextLines is the default number of file lines rendered above and
// below a review comment's referenced range in the summary.
const DefaultContextLines = 10

// ThreadComment is one earlier comment of the thread a review comment
// replies to.
type ThreadComment struct {
	// Author is the commenter's login; "" falls back to "reviewer".
	Author string
	// Body is the comment text as written.
	Body string
}

// ReviewCommentInput is one pull-request review comment plus the fetched
// material that gives it context. Fetching stays with the caller — the
// package stays pure.
type ReviewCommentInput struct {
	// Reviewer is the comment author's login; "" falls back to "reviewer".
	Reviewer string
	// Path is the file the comment is anchored to.
	Path string
	// Line is the last line of the commented range on the comment's side;
	// 0 means the comment has no line anchor (a file-level comment).
	Line int
	// StartLine is the first line of a multi-line comment range; 0 means a
	// single-line comment.
	StartLine int
	// Side is the diff side the comment anchors to ("RIGHT" or "LEFT").
	// Surrounding file context is only rendered for the RIGHT (head) side,
	// where FileContent's line numbers line up.
	Side string
	// Body is the comment text as written.
	Body string
	// DiffHunk is the diff hunk GitHub anchors the comment to, verbatim.
	DiffHunk string
	// Thread holds the earlier comments of the thread, oldest first, when
	// the comment is a reply; nil otherwise.
	Thread []ThreadComment
	// FileContent is the whole file at the PR head SHA; "" degrades the
	// summary to the diff hunk alone.
	FileContent string
	// ContextLines is how many file lines above and below the commented
	// range survive into the summary; 0 means DefaultContextLines.
	ContextLines int
}

// ReviewCommentResult is the distilled comment: the agent-ready summary plus
// the fields a consumer might key on.
type ReviewCommentResult struct {
	Reviewer string `json:"reviewer"`
	Path     string `json:"path"`
	// Lines is the commented range ("12" or "10–14"); "" when the comment
	// has no line anchor.
	Lines   string `json:"lines"`
	Summary string `json:"summary"`
}

// ReviewComment distills one review comment into an agent-ready summary:
// "Reviewer {login} commented on {path}:{lines}:" followed by the comment
// body, the earlier thread comments when replying, the diff hunk, and the
// surrounding file lines at the PR head. The only error is a negative
// ContextLines; callers that pre-validate can treat it as impossible.
func ReviewComment(in ReviewCommentInput) (ReviewCommentResult, error) {
	if in.ContextLines < 0 {
		return ReviewCommentResult{}, fmt.Errorf("distil: context lines must be non-negative, got %d", in.ContextLines)
	}
	reviewer := loginOr(in.Reviewer)

	var b strings.Builder
	fmt.Fprintf(&b, "Reviewer %s commented on %s:", reviewer, commentLoc(in.Path, in.StartLine, in.Line))
	writeCommentBody(&b, in.Body)
	writeThread(&b, in.Thread)
	writeHunk(&b, in.DiffHunk)
	if ctx := contextWindow(in); ctx != "" {
		fmt.Fprintf(&b, "\n\nFile context at head (±%d lines):\n%s", contextLinesOr(in.ContextLines), ctx)
	}

	return ReviewCommentResult{
		Reviewer: reviewer,
		Path:     in.Path,
		Lines:    linesLabel(in.StartLine, in.Line),
		Summary:  b.String(),
	}, nil
}

// ReviewInput is one submitted pull-request review with its inline comments.
type ReviewInput struct {
	// Reviewer is the review author's login; "" falls back to "reviewer".
	Reviewer string
	// State is the review verdict as GitHub reports it ("approved",
	// "changes_requested", "commented", any case).
	State string
	// Body is the review's top-level text; may be empty.
	Body string
	// Comments are the review's inline comments in order. They are rendered
	// hunk-only — per-comment file context belongs to the single-comment
	// path, which fetches one file, not N.
	Comments []ReviewCommentInput
}

// ReviewResult is the distilled review: the agent-ready summary plus the
// fields a consumer might key on.
type ReviewResult struct {
	Reviewer string `json:"reviewer"`
	// Verdict is the normalized (lowercased) review state.
	Verdict string `json:"verdict"`
	// Comments is the number of inline comments the review carries.
	Comments int    `json:"comments"`
	Summary  string `json:"summary"`
}

// Review distills one submitted review into an agent-ready summary: a
// header naming the reviewer and verdict, the top-level body, then every
// inline comment with its location, body, thread, and diff hunk — one
// coherent event, never N fragments. The only error is a negative
// ContextLines on a comment; callers that pre-validate can treat it as
// impossible.
func Review(in ReviewInput) (ReviewResult, error) {
	for _, c := range in.Comments {
		if c.ContextLines < 0 {
			return ReviewResult{}, fmt.Errorf("distil: context lines must be non-negative, got %d", c.ContextLines)
		}
	}
	reviewer := loginOr(in.Reviewer)
	verdict := normalizeVerdict(in.State)

	var b strings.Builder
	fmt.Fprintf(&b, "Reviewer %s %s — %d inline comment(s)", reviewer, verdictPhrase(verdict), len(in.Comments))
	if body := strings.TrimSpace(in.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
	}
	for _, c := range in.Comments {
		fmt.Fprintf(&b, "\n\n%s:", commentLoc(c.Path, c.StartLine, c.Line))
		writeCommentBody(&b, c.Body)
		writeThread(&b, c.Thread)
		writeHunk(&b, c.DiffHunk)
	}

	return ReviewResult{
		Reviewer: reviewer,
		Verdict:  verdict,
		Comments: len(in.Comments),
		Summary:  b.String(),
	}, nil
}

// writeCommentBody appends the comment text on its own line, with a marker
// for the (legal) empty comment.
func writeCommentBody(b *strings.Builder, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		body = "(no comment text)"
	}
	b.WriteString("\n")
	b.WriteString(body)
}

// writeThread appends the earlier comments of the thread, quoted.
func writeThread(b *strings.Builder, thread []ThreadComment) {
	if len(thread) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\nIn reply to %d earlier comment(s) in this thread:", len(thread))
	for _, tc := range thread {
		quoted := strings.ReplaceAll(strings.TrimSpace(tc.Body), "\n", "\n> ")
		fmt.Fprintf(b, "\n> %s: %s", loginOr(tc.Author), quoted)
	}
}

// writeHunk appends the diff hunk verbatim (minus trailing newlines).
func writeHunk(b *strings.Builder, hunk string) {
	hunk = strings.TrimRight(hunk, "\n")
	if hunk == "" {
		return
	}
	b.WriteString("\n\nDiff hunk:\n")
	b.WriteString(hunk)
}

// contextWindow slices the ±ContextLines window around the commented range
// out of the file content, with 1-based line numbers. It returns "" when
// there is nothing trustworthy to show: no file content, no line anchor, a
// LEFT-side comment (head line numbers don't line up), or a range past the
// file's end.
func contextWindow(in ReviewCommentInput) string {
	if in.FileContent == "" || in.Line <= 0 || strings.EqualFold(in.Side, "LEFT") {
		return ""
	}
	lines := strings.Split(in.FileContent, "\n")
	if lines[len(lines)-1] == "" {
		// The empty split after a newline-terminated file is not a line.
		lines = lines[:len(lines)-1]
	}
	start := in.Line
	if in.StartLine > 0 && in.StartLine < start {
		start = in.StartLine
	}
	if start > len(lines) {
		return ""
	}
	n := contextLinesOr(in.ContextLines)
	lo := max(1, start-n)
	hi := min(len(lines), in.Line+n)
	width := len(strconv.Itoa(hi))
	var b strings.Builder
	for i := lo; i <= hi; i++ {
		if i > lo {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%*d | %s", width, i, lines[i-1])
	}
	return b.String()
}

// contextLinesOr resolves the ContextLines knob: 0 means the default.
func contextLinesOr(n int) int {
	if n == 0 {
		return DefaultContextLines
	}
	return n
}

// commentLoc renders the comment's anchor as path:lines, degrading to the
// bare path for file-level comments.
func commentLoc(path string, start, end int) string {
	if path == "" {
		path = "(unknown file)"
	}
	if l := linesLabel(start, end); l != "" {
		return path + ":" + l
	}
	return path
}

// linesLabel renders a commented range as "12" or "10–14"; "" when there is
// no line anchor.
func linesLabel(start, end int) string {
	switch {
	case end <= 0:
		return ""
	case start > 0 && start != end:
		return fmt.Sprintf("%d–%d", start, end)
	default:
		return strconv.Itoa(end)
	}
}

// loginOr falls back to a generic label for a missing login.
func loginOr(login string) string {
	if login == "" {
		return "reviewer"
	}
	return login
}

// normalizeVerdict lowercases a review state, defaulting to "commented".
func normalizeVerdict(state string) string {
	v := strings.ToLower(strings.TrimSpace(state))
	if v == "" {
		return "commented"
	}
	return v
}

// verdictPhrase renders a normalized verdict as the summary's verb phrase.
func verdictPhrase(verdict string) string {
	switch verdict {
	case "approved":
		return "approved the pull request"
	case "changes_requested":
		return "requested changes"
	case "commented":
		return "commented on the pull request"
	default:
		return fmt.Sprintf("submitted a %q review", verdict)
	}
}
