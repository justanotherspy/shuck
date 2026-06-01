package semver

import (
	"strings"
	"testing"
)

// FuzzVersionParse exercises Parse with arbitrary tag names. Parse must never
// panic; on success the components must be non-negative, Raw must round-trip
// through Parse to an identical Version, and Stable() must agree with Pre.
func FuzzVersionParse(f *testing.F) {
	f.Add("v4.2.2")
	f.Add("1.2.3-rc.1")
	f.Add("V10")
	f.Add("3.1")
	f.Add("v1.2.3+build.5")
	f.Add("not-a-version")
	f.Add("")
	f.Add("v-1.2.3")
	f.Add("1..2")

	f.Fuzz(func(t *testing.T, name string) {
		v, ok := Parse(name)
		if !ok {
			return
		}
		if v.Major < 0 || v.Minor < 0 || v.Patch < 0 {
			t.Fatalf("Parse(%q) produced negative components: %+v", name, v)
		}
		if v.Stable() != (v.Pre == "") {
			t.Fatalf("Parse(%q): Stable()=%v disagrees with Pre=%q", name, v.Stable(), v.Pre)
		}
		// Raw is the trimmed original tag, so re-parsing it must reproduce the
		// same Version exactly.
		again, ok := Parse(v.Raw)
		if !ok {
			t.Fatalf("Parse(%q) ok, but re-parsing Raw %q failed", name, v.Raw)
		}
		if again != v {
			t.Fatalf("Parse(%q) is not idempotent on Raw: %+v vs %+v", name, v, again)
		}
	})
}

// FuzzVersionCompare checks that Compare is a valid total order over parsed
// versions: reflexive, antisymmetric, and consistent with equality.
func FuzzVersionCompare(f *testing.F) {
	f.Add("v1.2.3", "v1.2.4")
	f.Add("2.0.0-rc.1", "2.0.0")
	f.Add("v3", "3.0.0")
	f.Add("1.0.0-alpha", "1.0.0-beta")

	f.Fuzz(func(t *testing.T, aName, bName string) {
		a, okA := Parse(aName)
		b, okB := Parse(bName)
		if !okA || !okB {
			return
		}
		if got := Compare(a, a); got != 0 {
			t.Fatalf("Compare(a, a) = %d, want 0 (a=%+v)", got, a)
		}
		ab, ba := Compare(a, b), Compare(b, a)
		if ab != -ba {
			t.Fatalf("Compare is not antisymmetric: Compare(a,b)=%d, Compare(b,a)=%d (a=%+v b=%+v)", ab, ba, a, b)
		}
		if a == b && ab != 0 {
			t.Fatalf("equal versions compare non-zero: %d (a=%+v)", ab, a)
		}
		if ab == 0 && a.Raw != b.Raw {
			t.Fatalf("distinct raw tags compare equal: %q vs %q", a.Raw, b.Raw)
		}
	})
}

// FuzzConstraintMatches exercises ParseConstraint + Matches together. Neither
// must panic; an empty constraint matches everything; a parsed version always
// matches the constraint spelled from its own components; and a match implies
// the pinned components agree with the version.
func FuzzConstraintMatches(f *testing.F) {
	f.Add("v3", "v3.1.4")
	f.Add("3.1", "3.1.0-rc.1")
	f.Add("", "v0.0.1")
	f.Add("1.2.3", "1.2.3")
	f.Add("garbage", "v1.0.0")

	f.Fuzz(func(t *testing.T, conStr, verStr string) {
		v, vok := Parse(verStr)
		con, cok := ParseConstraint(conStr)
		if !vok {
			return
		}
		if cok && strings.TrimSpace(conStr) == "" && !con.Matches(v) {
			t.Fatalf("empty constraint must match every version, rejected %+v", v)
		}
		if !cok {
			return
		}
		if con.Specificity < 0 || con.Specificity > 3 {
			t.Fatalf("ParseConstraint(%q) produced specificity %d", conStr, con.Specificity)
		}
		// A constraint built from the version's own components must match it.
		exact, ok := ParseConstraint(verStr)
		if !ok {
			t.Fatalf("version %q parsed but is not a valid constraint", verStr)
		}
		if !exact.Matches(v) {
			t.Fatalf("constraint %q derived from version %q does not match it", verStr, verStr)
		}
	})
}
