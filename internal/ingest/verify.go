package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SignatureHeader is the GitHub webhook signature header Verify checks.
const SignatureHeader = "X-Hub-Signature-256"

// Verify reports whether sig — the X-Hub-Signature-256 header value, in the
// form "sha256=<hex>" — is a valid HMAC SHA-256 of body under secret. It
// fails closed: an empty secret, a missing or malformed header, and any
// mismatch are all false. The comparison is constant-time.
func Verify(secret, body []byte, sig string) bool {
	if len(secret) == 0 {
		return false
	}
	hexDigest, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexDigest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign computes the X-Hub-Signature-256 header value for body under secret.
// It exists for tests and local tooling; GitHub computes the real one.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
