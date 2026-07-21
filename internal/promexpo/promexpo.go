// Package promexpo renders in-process counters into the Prometheus text
// exposition format and serves them over HTTP. It is deliberately
// dependency-free — shuck hand-rolls its small clients rather than pulling
// in heavyweight libraries (see the GraphQL/OCI clients in internal/gh), and
// the exposition format is simple enough that the client_golang runtime is
// not worth the added dependency surface on the backend binaries.
//
// It backs the opt-in /metrics endpoint on the resident backend binaries
// (gateway server, worker poll loop, ingest server, portal server). It is
// never linked by the portable shuck CLI.
//
// Each backend package owns the names and help text of its own metrics via a
// Snapshot() []promexpo.Sample method next to the counter definitions; this
// package only formats and serves what it is given.
package promexpo

import (
	"bufio"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Type is a Prometheus metric type. Only the two shuck uses are modeled.
type Type string

const (
	// Counter is a monotonically increasing value; by convention its name
	// ends in _total.
	Counter Type = "counter"
	// Gauge is a value that can go up or down.
	Gauge Type = "gauge"
)

// ContentType is the exposition-format media type Prometheus scrapers accept.
// It matches what client_golang emits for the text format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Sample is one metric reading: a fully-qualified name, its help text and
// type, and the current value. Names should be Prometheus-valid
// ([a-zA-Z_:][a-zA-Z0-9_:]*); the caller is expected to namespace them
// (e.g. shuck_gateway_connections_live).
type Sample struct {
	Name  string
	Help  string
	Type  Type
	Value int64
}

// Write renders samples in the Prometheus text exposition format. Each
// distinct name emits its HELP and TYPE lines once, before the first sample
// carrying that name, so repeated names (a family) are grouped correctly.
// Samples are emitted in the order given; Write does not reorder them.
func Write(w io.Writer, samples []Sample) error {
	bw := bufio.NewWriter(w)
	seen := make(map[string]struct{}, len(samples))
	for _, s := range samples {
		if _, ok := seen[s.Name]; !ok {
			seen[s.Name] = struct{}{}
			if s.Help != "" {
				if _, err := bw.WriteString("# HELP " + s.Name + " " + escapeHelp(s.Help) + "\n"); err != nil {
					return err
				}
			}
			t := s.Type
			if t == "" {
				t = Counter
			}
			if _, err := bw.WriteString("# TYPE " + s.Name + " " + string(t) + "\n"); err != nil {
				return err
			}
		}
		if _, err := bw.WriteString(s.Name + " " + strconv.FormatInt(s.Value, 10) + "\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// escapeHelp escapes the two characters that are special in a HELP line:
// backslash and newline (a literal newline would terminate the line).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

// Handler serves the samples returned by collect in the exposition format.
// collect is called per scrape so values are always current; a nil collect
// serves an empty body. The handler only answers GET/HEAD.
func Handler(collect func() []Sample) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		if r.Method == http.MethodHead || collect == nil {
			return
		}
		// Errors writing to the ResponseWriter are unrecoverable here and
		// intentionally dropped, consistent with the repo's stdout/stderr
		// Fprint convention.
		_ = Write(w, collect())
	})
}

// Merge concatenates sample slices, preserving order. A small convenience for
// callers that assemble a snapshot from several sub-components.
func Merge(groups ...[]Sample) []Sample {
	var n int
	for _, g := range groups {
		n += len(g)
	}
	out := make([]Sample, 0, n)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// SortedNames returns the distinct metric names in a snapshot, sorted. Test
// and diagnostic helper; not used on the serving path.
func SortedNames(samples []Sample) []string {
	set := make(map[string]struct{}, len(samples))
	for _, s := range samples {
		set[s.Name] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
