package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAppID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"valid", "12345", 12345, false},
		{"empty is required", "", 0, true},
		{"not a number", "abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAppID(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseAppID(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseAppID(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadPrivateKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyFile, []byte("file-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		env      string
		file     string
		want     string
		wantErr  bool
		errsWith string
	}{
		{"env value only", "env-key", "", "env-key", false, ""},
		{"file only", "", keyFile, "file-key", false, ""},
		{"env value wins over file", "env-key", keyFile, "env-key", false, ""},
		{"neither is an error", "", "", "", true, "required"},
		{"unreadable file", "", filepath.Join(t.TempDir(), "missing.pem"), "", true, "read SHUCK_GITHUB_APP_PRIVATE_KEY_FILE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHUCK_GITHUB_APP_PRIVATE_KEY", tt.env)
			t.Setenv("SHUCK_GITHUB_APP_PRIVATE_KEY_FILE", tt.file)
			got, err := loadPrivateKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("loadPrivateKey() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errsWith) {
					t.Errorf("err = %v, want it to mention %q", err, tt.errsWith)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("loadPrivateKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRunConfigFailures drives run() through env parse/validation failures.
// Every case must fail before any AWS client or loop is touched — the dummy
// private key is never parsed on these paths.
func TestRunConfigFailures(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		errsWith string
	}{
		{
			"missing app id",
			map[string]string{"SHUCK_GITHUB_APP_ID": ""},
			"SHUCK_GITHUB_APP_ID is required",
		},
		{
			"missing deliver url and secret",
			map[string]string{"SHUCK_DELIVER_URL": "", "SHUCK_DELIVER_SECRET": ""},
			"SHUCK_DELIVER_URL and SHUCK_DELIVER_SECRET are required",
		},
		{
			"unparseable summary limit",
			map[string]string{"SHUCK_SUMMARY_LIMIT": "lots"},
			"parse SHUCK_SUMMARY_LIMIT",
		},
		{
			"unparseable review context lines",
			map[string]string{"SHUCK_REVIEW_CONTEXT_LINES": "ten"},
			"parse SHUCK_REVIEW_CONTEXT_LINES",
		},
		{
			"negative review context lines",
			map[string]string{"SHUCK_REVIEW_CONTEXT_LINES": "-1"},
			"SHUCK_REVIEW_CONTEXT_LINES must be >= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A viable base config, overridden per case. The key is a dummy:
			// these paths must all return before it is parsed.
			base := map[string]string{
				"SHUCK_GITHUB_APP_ID":          "7",
				"SHUCK_GITHUB_APP_PRIVATE_KEY": "dummy",
				"SHUCK_DELIVER_URL":            "http://gateway/internal/deliver",
				"SHUCK_DELIVER_SECRET":         "s3cret",
				"SHUCK_SUMMARY_LIMIT":          "",
				"SHUCK_REVIEW_CONTEXT_LINES":   "",
			}
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			err := run(context.Background(), slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("run() succeeded, want a config error")
			}
			if !strings.Contains(err.Error(), tt.errsWith) {
				t.Errorf("err = %v, want it to mention %q", err, tt.errsWith)
			}
		})
	}
}
