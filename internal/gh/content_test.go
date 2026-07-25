package gh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v89/github"
)

// contentJSON is the shape GitHub returns for a file: base64 content plus the
// encoding that decodes it.
func contentJSON(body string) string {
	return fmt.Sprintf(`{"type":"file","name":"foo.go","path":"internal/foo/foo.go","encoding":"base64","content":%q}`,
		base64.StdEncoding.EncodeToString([]byte(body)))
}

// TestFileContentPassesTheRef is the property the monitor depends on. A review
// comment's line numbers refer to the commit it was left on, so the fetch must
// happen at that ref — falling back to the default branch would quote whatever
// is on main today, with total confidence and no way for a reader to tell.
func TestFileContentPassesTheRef(t *testing.T) {
	const src = "package foo\n\nfunc Foo() error {\n\treturn nil\n}\n"

	tests := []struct {
		name    string
		ref     string
		wantRef string // "" means the parameter must be absent entirely
	}{
		{"an explicit commit is requested by SHA", "abc123", "abc123"},
		{"a branch name is passed through", "feature/x", "feature/x"},
		// No ref means "the repository's default branch"; sending ref= empty
		// would be a different request, so the parameter must not appear.
		{"an empty ref sends no parameter", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotRef string
			var sawRefParam bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if want := "/repos/acme/widgets/contents/internal/foo/foo.go"; r.URL.Path != want {
					t.Errorf("path = %q, want %q", r.URL.Path, want)
				}
				gotRef = r.URL.Query().Get("ref")
				_, sawRefParam = r.URL.Query()["ref"]
				_, _ = w.Write([]byte(contentJSON(src)))
			}))
			defer srv.Close()

			got, err := testClient(t, srv).FileContent(context.Background(), "acme", "widgets", "internal/foo/foo.go", tc.ref)
			if err != nil {
				t.Fatalf("FileContent: %v", err)
			}
			if string(got) != src {
				t.Errorf("content = %q, want %q", got, src)
			}
			if tc.wantRef == "" && sawRefParam {
				t.Errorf("an empty ref must not send ?ref=, got %q", gotRef)
			}
			if tc.wantRef != "" && gotRef != tc.wantRef {
				t.Errorf("ref = %q, want %q", gotRef, tc.wantRef)
			}
		})
	}
}

// TestFileContentDirectoryIsNotAFile guards the case that would otherwise read
// as an empty file: GitHub answers a directory path with a JSON array, which
// decodes to a nil file entry. Returning empty content there would tell the
// monitor the file exists and is blank.
func TestFileContentDirectoryIsNotAFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"type":"file","name":"foo.go","path":"internal/foo/foo.go"}]`))
	}))
	defer srv.Close()

	got, err := testClient(t, srv).FileContent(context.Background(), "acme", "widgets", "internal/foo", "")
	if err == nil {
		t.Fatalf("a directory should be an error, got %q", got)
	}
	if got != nil {
		t.Errorf("an error must not also return content, got %q", got)
	}
	if !strings.Contains(err.Error(), "not a file") {
		t.Errorf("error = %v, want it to say the path is not a file", err)
	}
}

// TestFileContentErrorsAreRecognisable pairs the fetch with the classifier the
// monitor uses on its result: a deleted file must be distinguishable from a
// broken API, because the first costs an event its code context and the second
// is a fault worth surfacing.
func TestFileContentErrorsAreRecognisable(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantNotFound  bool
		wantIsNotFnd  bool
		wantInMessage string
	}{
		{"a file deleted in a later commit", http.StatusNotFound, true, true, "get internal/foo/foo.go"},
		{"a token without access", http.StatusForbidden, false, false, "get internal/foo/foo.go"},
		{"the API having a bad day", http.StatusInternalServerError, false, false, "get internal/foo/foo.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			}))
			defer srv.Close()

			_, err := testClient(t, srv).FileContent(context.Background(), "acme", "widgets", "internal/foo/foo.go", "abc")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantInMessage) {
				t.Errorf("error %v should name what was being fetched", err)
			}
			if got := FileNotFound(err); got != tc.wantNotFound {
				t.Errorf("FileNotFound(%v) = %v, want %v", err, got, tc.wantNotFound)
			}
			if got := IsNotFound(err); got != tc.wantIsNotFnd {
				t.Errorf("IsNotFound(%v) = %v, want %v", err, got, tc.wantIsNotFnd)
			}
		})
	}
}

// TestFileContentUndecodableContent covers the decode arm: a file GitHub claims
// is base64 but is not.
func TestFileContentUndecodableContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"file","encoding":"weird-encoding","content":"!!!"}`))
	}))
	defer srv.Close()

	if _, err := testClient(t, srv).FileContent(context.Background(), "acme", "widgets", "x.go", ""); err == nil {
		t.Fatal("an undecodable body should be an error, not empty content")
	}
}

// TestIsNotFoundVsFileNotFound pins where the two classifiers deliberately
// differ. IsNotFound is strict — `shuck security` uses it to decide a repository
// genuinely does not exist, and a false positive there turns a real repo into a
// hard error. FileNotFound adds a substring fallback because the contents
// endpoint has been seen to surface a 404 the type assertion does not reach, and
// erring toward "absent" there only costs an event its code context.
func TestIsNotFoundVsFileNotFound(t *testing.T) {
	ghErr := func(status int) error {
		return &github.ErrorResponse{Response: &http.Response{StatusCode: status}}
	}

	tests := []struct {
		name             string
		err              error
		wantIsNotFound   bool
		wantFileNotFound bool
	}{
		{"a typed 404", ghErr(http.StatusNotFound), true, true},
		{"a typed 404 wrapped by FileContent", fmt.Errorf("get x: %w", ghErr(http.StatusNotFound)), true, true},
		{"a typed 403", ghErr(http.StatusForbidden), false, false},
		{"a typed 500", ghErr(http.StatusInternalServerError), false, false},
		// The fallback's whole reason for existing, and its cost: an untyped
		// error merely mentioning 404 reads as absent to FileNotFound only.
		{"an untyped error mentioning 404", errors.New("unexpected status 404 from proxy"), false, true},
		{"an unrelated error", errors.New("connection reset by peer"), false, false},
		// A 404 in the message for an unrelated reason is the fallback's known
		// false positive; recording it here means a future tightening of the
		// rule shows up as a deliberate change rather than a surprise.
		{"a path that merely contains 404", errors.New("open /tmp/404/x.go: no such file"), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.wantIsNotFound {
				t.Errorf("IsNotFound = %v, want %v", got, tc.wantIsNotFound)
			}
			if got := FileNotFound(tc.err); got != tc.wantFileNotFound {
				t.Errorf("FileNotFound = %v, want %v", got, tc.wantFileNotFound)
			}
		})
	}
}

// TestIsNotFoundOnNilResponse covers the guard that keeps the classifiers from
// panicking on a *github.ErrorResponse that never carried an HTTP response —
// which is what a transport-level failure produces.
func TestIsNotFoundOnNilResponse(t *testing.T) {
	err := &github.ErrorResponse{Message: "no response"}
	if IsNotFound(err) {
		t.Error("an ErrorResponse with no HTTP response is not a 404")
	}
	if FileNotFound(err) {
		t.Error("an ErrorResponse with no HTTP response is not a missing file")
	}
	// Sanity: json.Unmarshal errors and the like must not classify either.
	var target struct{ X int }
	if jerr := json.Unmarshal([]byte("{"), &target); IsNotFound(jerr) || FileNotFound(jerr) {
		t.Error("a JSON error is neither a 404 nor a missing file")
	}
}
