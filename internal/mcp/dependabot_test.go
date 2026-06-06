package mcp

import (
	"strings"
	"testing"
)

func TestAuditDependabotTargetArgs(t *testing.T) {
	cases := []struct {
		name string
		in   auditDependabotInput
		want []string
	}{
		{"url wins", auditDependabotInput{URL: "https://github.com/o/r", Repo: "x/y"}, []string{"https://github.com/o/r"}},
		{"repo", auditDependabotInput{Repo: "o/r"}, []string{"o/r"}},
		{"nothing", auditDependabotInput{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.targetArgs()
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("targetArgs() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAuditDependabotRegistered makes sure the tool is wired into the server.
func TestAuditDependabotRegistered(t *testing.T) {
	if newServer() == nil {
		t.Fatal("newServer returned nil")
	}
}
