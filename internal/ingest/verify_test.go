package ingest

import "testing"

// The known-answer vector from GitHub's webhook validation docs.
const (
	docsSecret  = "It's a Secret to Everybody"
	docsPayload = "Hello, World!"
	docsSig     = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
)

func TestVerify(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		body   string
		sig    string
		want   bool
	}{
		{"github docs vector", docsSecret, docsPayload, docsSig, true},
		{"round trip", "s3cret", `{"action":"completed"}`, Sign([]byte("s3cret"), []byte(`{"action":"completed"}`)), true},
		{"tampered body", docsSecret, docsPayload + "!", docsSig, false},
		{"tampered signature", docsSecret, docsPayload, "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e18", false},
		{"wrong secret", "not the secret", docsPayload, docsSig, false},
		{"missing prefix", docsSecret, docsPayload, "757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17", false},
		{"sha1 prefix", docsSecret, docsPayload, "sha1=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17", false},
		{"bad hex", docsSecret, docsPayload, "sha256=zz7107ea", false},
		{"truncated digest", docsSecret, docsPayload, "sha256=757107ea", false},
		{"empty header", docsSecret, docsPayload, "", false},
		{"empty secret fails closed", "", docsPayload, docsSig, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Verify([]byte(tc.secret), []byte(tc.body), tc.sig); got != tc.want {
				t.Fatalf("Verify() = %v, want %v", got, tc.want)
			}
		})
	}
}
