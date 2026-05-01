package main

import (
	"os"
	"path/filepath"
	"strings"
)

// claudeSource wraps Claude Code's transcript format: JSONL files under
// ~/.claude/projects/<proj-hash>/<session>.jsonl plus the archived mirror.
type claudeSource struct{}

func (*claudeSource) ID() string          { return "claude" }
func (*claudeSource) DisplayName() string { return "Claude Code" }

// Match accepts any .jsonl file. Claude's live directory tree contains only
// transcripts, so extension alone is sufficient; the discovery walker scopes
// the search to Claude roots before calling this.
func (*claudeSource) Match(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".jsonl")
}

// Roots returns the archive (first, shadows live) and live directory for
// Claude Code transcripts. Honors $CLAUDE_HISTORY_DIR for the live tree.
func (*claudeSource) Roots() []string {
	return []string{
		filepath.Join(conversationsDir(), "claude"),
		claudeLiveDir(),
	}
}

func claudeLiveDir() string {
	if v := os.Getenv("CLAUDE_HISTORY_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// Parse reads a Claude Code JSONL transcript into turns. Delegates to the
// existing parseSession path-based parser; every turn is stamped with
// Source="claude" so downstream consumers can tag hits.
func (*claudeSource) Parse(path string) ([]Turn, error) {
	turns, err := parseSession(path)
	if err != nil {
		return nil, err
	}
	for i := range turns {
		turns[i].Source = "claude"
	}
	return turns, nil
}

// ArchiveDst mirrors the live tree layout (<proj-hash>/<session>.jsonl)
// under <archive>/conversations/claude/. Paths outside the live root are
// archived by (parent-dir, filename) to keep collisions unlikely.
func (*claudeSource) ArchiveDst(livePath string) string {
	root := claudeLiveDir()
	rel, err := filepath.Rel(root, livePath)
	if err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(conversationsDir(), "claude", rel)
	}
	parent := filepath.Base(filepath.Dir(livePath))
	name := filepath.Base(livePath)
	return filepath.Join(conversationsDir(), "claude", parent, name)
}
