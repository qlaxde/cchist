package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyArchive(t *testing.T) {
	archive := t.TempDir()
	t.Setenv("CCHIST_ARCHIVE", archive)

	conv := filepath.Join(archive, "conversations")
	legacy := filepath.Join(conv, "-Users-me-Workspace-foo", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Also seed an already-migrated file under claude/ — migration must leave
	// it alone and not re-bucket into claude/claude/...
	already := filepath.Join(conv, "claude", "-Users-me-Workspace-bar", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(already), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(already, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyArchive(); err != nil {
		t.Fatalf("migrateLegacyArchive: %v", err)
	}

	want := filepath.Join(conv, "claude", "-Users-me-Workspace-foo", "session.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected migrated file at %s: %v", want, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("expected legacy path %s to be removed, got err=%v", legacy, err)
	}
	if _, err := os.Stat(already); err != nil {
		t.Errorf("already-migrated file at %s was disturbed: %v", already, err)
	}

	// Idempotent: running a second time must not error or double-move.
	if err := migrateLegacyArchive(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file gone after second run: %v", err)
	}
}
