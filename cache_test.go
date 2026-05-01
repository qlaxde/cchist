package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDiscoverAll_TagsBySource(t *testing.T) {
	home := t.TempDir()
	archive := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCHIST_ARCHIVE", archive)
	t.Setenv("CLAUDE_HISTORY_DIR", "") // fall back to $HOME/.claude/projects
	t.Setenv("CODEX_HOME", "")         // fall back to $HOME/.codex

	// Seed a Claude transcript and a Codex rollout under the fake $HOME.
	claudeFile := filepath.Join(home, ".claude", "projects", "proj-abc", "session-1.jsonl")
	codexFile := filepath.Join(home, ".codex", "sessions", "2025", "11", "07", "rollout-2025-11-07T14-25-23-xyz.jsonl")
	// A non-rollout file Codex also drops in its sessions tree — must be ignored.
	codexIgnore := filepath.Join(home, ".codex", "sessions", "index.jsonl")

	for _, p := range []string{claudeFile, codexFile, codexIgnore} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := discoverAll(sources)
	if err != nil {
		t.Fatalf("discoverAll: %v", err)
	}

	type pair struct{ source, path string }
	pairs := make([]pair, 0, len(got))
	for _, fi := range got {
		pairs = append(pairs, pair{fi.Source, fi.Path})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].source != pairs[j].source {
			return pairs[i].source < pairs[j].source
		}
		return pairs[i].path < pairs[j].path
	})

	want := []pair{
		{"claude", claudeFile},
		{"codex", codexFile},
	}
	if len(pairs) != len(want) {
		t.Fatalf("discoverAll returned %d entries, want %d: %+v", len(pairs), len(want), pairs)
	}
	for i, w := range want {
		if pairs[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, pairs[i], w)
		}
	}
}
