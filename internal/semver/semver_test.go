package semver

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in                  string
		ok                  bool
		major, minor, patch int
		pre                 string
	}{
		{"v4.2.2", true, 4, 2, 2, ""},
		{"4.2.2", true, 4, 2, 2, ""},
		{"v3", true, 3, 0, 0, ""},
		{"3.1", true, 3, 1, 0, ""},
		{"v5.0.0-rc.1", true, 5, 0, 0, "rc.1"},
		{"v4.2.2+build.7", true, 4, 2, 2, ""},
		{"latest", false, 0, 0, 0, ""},
		{"nightly", false, 0, 0, 0, ""},
		{"v1.2.3.4", false, 0, 0, 0, ""},
		{"", false, 0, 0, 0, ""},
	}
	for _, tc := range tests {
		got, ok := Parse(tc.in)
		if ok != tc.ok {
			t.Errorf("Parse(%q) ok=%v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Major != tc.major || got.Minor != tc.minor || got.Patch != tc.patch || got.Pre != tc.pre {
			t.Errorf("Parse(%q) = %+v, want %d.%d.%d pre=%q", tc.in, got, tc.major, tc.minor, tc.patch, tc.pre)
		}
	}
}

func TestCompare(t *testing.T) {
	mustParse := func(s string) Version {
		v, ok := Parse(s)
		if !ok {
			t.Fatalf("Parse(%q) failed", s)
		}
		return v
	}
	tests := []struct {
		a, b string
		want int // sign
	}{
		{"v4.2.2", "v4.2.1", 1},
		{"v4.2.2", "v4.3.0", -1},
		{"v5.0.0", "v5.0.0-rc.1", 1}, // stable > prerelease
		{"v5.0.0-rc.2", "v5.0.0-rc.1", 1},
		{"v10.0.0", "v9.9.9", 1}, // numeric, not lexical
	}
	for _, tc := range tests {
		got := Compare(mustParse(tc.a), mustParse(tc.b))
		if sign(got) != tc.want {
			t.Errorf("Compare(%q,%q) sign = %d, want %d", tc.a, tc.b, sign(got), tc.want)
		}
	}
}

func TestConstraintMatches(t *testing.T) {
	v := func(s string) Version {
		p, _ := Parse(s)
		return p
	}
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		{"", "v4.2.2", true},
		{"v4", "v4.2.2", true},
		{"v4", "v3.2.2", false},
		{"4.2", "v4.2.9", true},
		{"4.2", "v4.3.0", false},
		{"4.2.2", "v4.2.2", true},
		{"4.2.2", "v4.2.3", false},
	}
	for _, tc := range tests {
		c, ok := ParseConstraint(tc.constraint)
		if !ok {
			t.Fatalf("ParseConstraint(%q) failed", tc.constraint)
		}
		if got := c.Matches(v(tc.version)); got != tc.want {
			t.Errorf("%q.Matches(%q) = %v, want %v", tc.constraint, tc.version, got, tc.want)
		}
	}
}

func TestParseConstraintInvalid(t *testing.T) {
	for _, s := range []string{"1.2.3.4", "abc"} {
		if _, ok := ParseConstraint(s); ok {
			t.Errorf("ParseConstraint(%q) ok=true, want false", s)
		}
	}
}
