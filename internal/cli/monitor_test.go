package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/monitor"
)

// monitorHome points shuck's whole on-disk state at a temp directory, so a
// test never touches the developer's real monitor.
func monitorHome(t *testing.T) {
	t.Helper()
	t.Setenv("SHUCK_HOME", t.TempDir())
}

// noDaemonClient returns a client that will never find or start a daemon,
// which is the state every one of these tests runs against: the CLI's job here
// is to report that clearly, not to conjure one.
func noDaemonClient(t *testing.T) {
	t.Helper()
	original := newMonitorClient
	newMonitorClient = func() (*monitor.Client, error) {
		c, err := monitor.NewClient()
		if err != nil {
			return nil, err
		}
		c.AutoStart = false
		return c, nil
	}
	t.Cleanup(func() { newMonitorClient = original })
}

func runCLI(args ...string) (code int, stdoutText, stderrText string) {
	var stdout, stderr bytes.Buffer
	code = Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMonitorUsage(t *testing.T) {
	code, _, stderr := runCLI("monitor", "nonsense")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for an unknown subcommand", code)
	}
	for _, want := range []string{"unknown subcommand", "shuck monitor watch", "shuck monitor events"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, stderr)
		}
	}
}

func TestMonitorAliasIsRegistered(t *testing.T) {
	// `shuck m` must reach the monitor, not be mistaken for a target.
	_, _, stderr := runCLI("m", "nonsense")
	if !strings.Contains(stderr, "shuck monitor") {
		t.Errorf("`shuck m` did not dispatch to the monitor:\n%s", stderr)
	}
}

func TestMonitorStatusWithNoDaemon(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	// --no-start is the honest read: report that nothing is running rather
	// than starting one just to say it is empty.
	code, stdout, _ := runCLI("monitor", "status", "--no-start")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q, want it to say the monitor is not running", stdout)
	}
}

func TestMonitorReadCommandsDoNotStartADaemon(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	for _, args := range [][]string{
		{"monitor", "events"},
		{"monitor", "poke"},
		{"monitor", "unwatch"},
	} {
		code, _, stderr := runCLI(args...)
		if code != 2 {
			t.Errorf("%v exited %d, want 2 with no daemon", args, code)
		}
		if !strings.Contains(stderr, "no shuck monitor is running") {
			t.Errorf("%v stderr = %q, want it to name the problem", args, stderr)
		}
	}
}

func TestMonitorStopWithNoDaemonIsNotAnError(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	// Stopping something already stopped is what the caller wanted.
	code, stdout, _ := runCLI("monitor", "stop")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not running") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestMonitorLogWithNoLog(t *testing.T) {
	monitorHome(t)
	code, stdout, _ := runCLI("monitor", "log")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no log yet") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestMonitorWatchRejectsABadTarget(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)

	code, _, stderr := runCLI("monitor", "watch", "not-a-target")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stderr == "" {
		t.Error("a bad target should be explained")
	}
}

func TestMonitorWatchNeedsAToken(t *testing.T) {
	monitorHome(t)
	noDaemonClient(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	// The daemon a client starts inherits this environment, so a missing token
	// is reported here — in front of the person who can fix it — rather than
	// silently inside a background process.
	code, _, stderr := runCLI("monitor", "watch", "o/r#1")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "no GitHub token") {
		t.Errorf("stderr = %q, want the missing token named", stderr)
	}
}

func TestMonitorHookNeedsAnEvent(t *testing.T) {
	code, _, stderr := runCLI("monitor", "hook")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "which hook") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestMonitorHookExitsZeroWithNoDaemon(t *testing.T) {
	monitorHome(t)
	// A hook must never be the reason a session stalls.
	code, stdout, _ := runCLI("monitor", "hook", "user-prompt-submit")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want silence", stdout)
	}
}

func TestMonitorRunRejectsBadFlags(t *testing.T) {
	monitorHome(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	code, _, stderr := runCLI("monitor", "run")
	if code != 2 {
		t.Errorf("exit = %d, want 2 with no token", code)
	}
	if !strings.Contains(stderr, "no GitHub token") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRenderStatus(t *testing.T) {
	var b bytes.Buffer
	renderStatus(&b, &monitor.Status{
		PID: 42, Version: "v1.2.3", Uptime: 90 * time.Second,
		RateRemaining: 4200, RateLimit: 5000,
		Watches: []monitor.Watch{
			{ID: "tree:/w", Kind: monitor.WatchTree, Path: "/w", Owner: "o", Repo: "r", Number: 7, Branch: "feature"},
		},
		Targets: []monitor.TargetStatus{
			{Target: "o/r#7", Verdict: "failed", NextPoll: time.Now().Add(30 * time.Second)},
		},
		Events: 12, Pending: 3,
	})

	got := b.String()
	for _, want := range []string{"v1.2.3", "pid 42", "4200/5000", "/w", "o/r#7", "CI FAILING", "next check in", "12 event", "3 waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("status is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStatusEmpty(t *testing.T) {
	var b bytes.Buffer
	renderStatus(&b, &monitor.Status{Version: "dev"})
	got := b.String()
	if !strings.Contains(got, "shuck monitor watch") {
		t.Errorf("an empty monitor should say how to point it at something:\n%s", got)
	}
}

func TestTargetLine(t *testing.T) {
	tests := []struct {
		name   string
		target monitor.TargetStatus
		want   []string
	}{
		{"green", monitor.TargetStatus{Verdict: "passed"}, []string{"CI green"}},
		{"red", monitor.TargetStatus{Verdict: "failed"}, []string{"CI FAILING"}},
		{"running", monitor.TargetStatus{}, []string{"running or not started"}},
		{"merged", monitor.TargetStatus{Verdict: "passed", Lifecycle: "merged"}, []string{"merged"}},
		{"due now", monitor.TargetStatus{NextPoll: time.Now().Add(-time.Minute)}, []string{"checking now"}},
		{"failing", monitor.TargetStatus{LastError: "401"}, []string{"error: 401"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetLine(tt.target)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("targetLine = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestEmitEvents(t *testing.T) {
	events := []monitor.Event{
		{ID: 1, Kind: monitor.KindCIFailed, Target: "o/r#7", Title: "test failed", Body: "boom"},
	}

	t.Run("text", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := emitEvents(&out, &errBuf, events, false, true); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(out.String(), "test failed") || !strings.Contains(out.String(), "boom") {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("json is one object per line", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := emitEvents(&out, &errBuf, events, true, true); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
			t.Errorf("one event should be one line, got %d newlines", lines+1)
		}
		if !strings.Contains(out.String(), `"kind":"ci.failed"`) {
			t.Errorf("stdout = %q", out.String())
		}
	})

	t.Run("nothing new", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		emitEvents(&out, &errBuf, nil, false, true)
		if !strings.Contains(out.String(), "nothing new") {
			t.Errorf("stdout = %q", out.String())
		}

		out.Reset()
		emitEvents(&out, &errBuf, nil, false, false)
		if out.String() != "" {
			t.Errorf("while following, an empty batch should print nothing, got %q", out.String())
		}
	})
}
