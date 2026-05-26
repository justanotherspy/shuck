package cache

import (
	"testing"
	"time"

	"github.com/justanotherspy/shuck/internal/model"
)

func TestActionTagsRoundTrip(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())

	want := []model.ActionTag{
		{Name: "v4.2.2", SHA: "abc"},
		{Name: "v4", SHA: "abc"},
	}
	before := time.Now()
	if err := SaveActionTags("actions", "checkout", want); err != nil {
		t.Fatalf("SaveActionTags: %v", err)
	}

	got, fetchedAt, ok, err := LoadActionTags("actions", "checkout")
	if err != nil {
		t.Fatalf("LoadActionTags: %v", err)
	}
	if !ok {
		t.Fatal("LoadActionTags ok=false for a saved entry")
	}
	if len(got) != 2 || got[0].Name != "v4.2.2" || got[0].SHA != "abc" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if fetchedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("fetchedAt %v not stamped near now", fetchedAt)
	}
}

func TestLoadActionTagsMissing(t *testing.T) {
	t.Setenv("SHUCK_HOME", t.TempDir())
	_, _, ok, err := LoadActionTags("nope", "missing")
	if err != nil {
		t.Fatalf("LoadActionTags: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a missing entry")
	}
}
