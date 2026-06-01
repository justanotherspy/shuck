package semver

import "testing"

func TestCompareBranches(t *testing.T) {
	mustParse := func(s string) Version {
		v, ok := Parse(s)
		if !ok {
			t.Fatalf("Parse(%q) failed", s)
		}
		return v
	}
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal stable", "v1.2.3", "v1.2.3", 0},
		{"minor less", "v1.1.0", "v1.2.0", -1},
		{"patch greater", "v1.2.4", "v1.2.3", 1},
		{"prerelease less than stable", "v5.0.0-rc.1", "v5.0.0", -1},
		{"prerelease suffix lexical less", "v5.0.0-alpha", "v5.0.0-beta", -1},
		{"prerelease suffix lexical greater", "v5.0.0-rc.2", "v5.0.0-rc.1", 1},
		{"equal prerelease tie-break on raw", "v5.0.0-rc.1", "V5.0.0-rc.1", 1}, // "v..." > "V..." lexically on raw
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sign(Compare(mustParse(tc.a), mustParse(tc.b))); got != tc.want {
				t.Errorf("Compare(%q,%q) sign = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareEqualRawIsZero(t *testing.T) {
	v, _ := Parse("v2.0.0-rc.1")
	if got := Compare(v, v); got != 0 {
		t.Errorf("Compare(v, v) = %d, want 0", got)
	}
}

func TestSign(t *testing.T) {
	cases := map[int]int{-5: -1, -1: -1, 0: 0, 1: 1, 9: 1}
	for in, want := range cases {
		if got := sign(in); got != want {
			t.Errorf("sign(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseConstraintErrors(t *testing.T) {
	// "v" trims to "", then Split("", ".") yields [""] which fails to parse, so it
	// is invalid (only a literal empty/whitespace string is the match-all case).
	for _, s := range []string{"1.2.3.4", "abc", "v-1", "1.x", "v"} {
		if _, ok := ParseConstraint(s); ok {
			t.Errorf("ParseConstraint(%q) ok=true, want false", s)
		}
	}
	for _, s := range []string{"", "   "} {
		if _, ok := ParseConstraint(s); !ok {
			t.Errorf("ParseConstraint(%q) ok=false, want true (empty matches all)", s)
		}
	}
}

func TestParseConstraintStripsBuildAndPre(t *testing.T) {
	c, ok := ParseConstraint("v3.1+build.4")
	if !ok || c.Specificity != 2 {
		t.Fatalf("ParseConstraint(v3.1+build.4) = %+v, ok=%v", c, ok)
	}
	c2, ok := ParseConstraint("3.1.0-rc.1")
	if !ok || c2.Specificity != 3 {
		t.Fatalf("ParseConstraint(3.1.0-rc.1) = %+v, ok=%v", c2, ok)
	}
}
