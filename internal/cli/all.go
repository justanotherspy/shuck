package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/justanotherspy/shuck/internal/jsonout"
	"github.com/justanotherspy/shuck/internal/model"
	"github.com/justanotherspy/shuck/internal/render"
	"github.com/justanotherspy/shuck/internal/security"
	"github.com/justanotherspy/shuck/internal/target"
)

// combinedSchemaVersion is the version of the `shuck` / `shuck all` --json
// envelope. The embedded inspection and security documents carry their own
// schema versions; this one is bumped only on a breaking change to the envelope.
const combinedSchemaVersion = 1

// combinedResult bundles a PR inspection with the repo's security report for the
// default / `all` view. sec and secErr are both nil for run/job and offline
// targets, which have no security half.
type combinedResult struct {
	report *model.Report
	sec    *model.SecurityReport
	secErr error
}

// combinedDocument is the stable JSON shape of the default / `all` view: a PR
// inspection plus the repo's security report, each its own versioned
// sub-document. security is omitted (and security_error set) when the security
// fetch failed.
type combinedDocument struct {
	SchemaVersion int                `json:"schema_version"`
	Inspection    jsonout.Document   `json:"inspection"`
	Security      *security.Document `json:"security,omitempty"`
	SecurityError string             `json:"security_error,omitempty"`
}

// inspectAll runs the CI + reviews pipeline and, for PR targets, also fetches
// the repo's security alerts. A failure of the inspection itself is fatal; the
// security half degrades independently (see withSecurity).
func inspectAll(ctx context.Context, tgt target.Target, o options) (*combinedResult, error) {
	report, err := inspectWith(ctx, tgt, o)
	if err != nil {
		return nil, err
	}
	return withSecurity(ctx, tgt, o, report), nil
}

// withSecurity wraps an inspection report, attaching the repo's security report
// for PR targets. Run/job targets and offline runs have no security half.
// Security degrades independently: a fetch error is captured for a one-line
// note rather than failing the command (only an all-sources failure errors —
// see Security).
func withSecurity(ctx context.Context, tgt target.Target, o options, report *model.Report) *combinedResult {
	res := &combinedResult{report: report}
	if tgt.RunID == 0 && !o.offline {
		res.sec, res.secErr = Security(ctx, tgt.Owner, tgt.Repo, SecurityOptions{State: o.state, Token: o.token, Refresh: o.refresh})
	}
	return res
}

// emitAll renders a combined result. Run/job and offline targets carry no
// security half and fall back to the plain single-document output; otherwise
// the text output appends a security section (or a one-line note) and --json
// emits the combined envelope. The exit code reflects the CI/PR verdict only —
// security findings do not flip it (use `shuck security --exit-code` for that).
func emitAll(stdout io.Writer, res *combinedResult, jsonOut bool) (int, error) {
	if res.sec == nil && res.secErr == nil {
		return emit(stdout, res.report, jsonOut)
	}
	if jsonOut {
		doc := combinedDocument{
			SchemaVersion: combinedSchemaVersion,
			Inspection:    jsonout.NewDocument(res.report),
		}
		if res.secErr != nil {
			doc.SecurityError = res.secErr.Error()
		} else {
			d := security.NewDocument(res.sec)
			doc.Security = &d
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return 0, err
		}
		return exitFor(res.report), nil
	}
	render.Report(stdout, res.report)
	fmt.Fprintln(stdout)
	if res.secErr != nil {
		fmt.Fprintf(stdout, "security alerts: unavailable (%v)\n", res.secErr)
	} else {
		security.Render(stdout, res.sec)
	}
	return exitFor(res.report), nil
}
