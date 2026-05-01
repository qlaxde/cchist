package main

import "testing"

func TestSourceRegistry(t *testing.T) {
	tests := map[string]struct {
		id, display string
	}{
		"claude": {"claude", "Claude Code"},
		"codex":  {"codex", "Codex CLI"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := sourceByID(tt.id)
			if s == nil {
				t.Fatalf("sourceByID(%q) = nil", tt.id)
			}
			if got := s.ID(); got != tt.id {
				t.Errorf("ID = %q, want %q", got, tt.id)
			}
			if got := s.DisplayName(); got != tt.display {
				t.Errorf("DisplayName = %q, want %q", got, tt.display)
			}
		})
	}
	if got := sourceByID("unknown-agent"); got != nil {
		t.Errorf("sourceByID(unknown-agent) = %v, want nil", got)
	}
}

func TestSourceMatch(t *testing.T) {
	tests := map[string]struct {
		id   string
		path string
		want bool
	}{
		"claude matches .jsonl under projects":         {"claude", "/home/me/.claude/projects/x/session.jsonl", true},
		"claude rejects non-jsonl":                     {"claude", "/home/me/.claude/projects/x/session.txt", false},
		"codex matches rollout-*.jsonl":                {"codex", "/home/me/.codex/sessions/2025/11/rollout-abc.jsonl", true},
		"codex rejects non-rollout jsonl":              {"codex", "/home/me/.codex/sessions/2025/11/index.jsonl", false},
		"codex rejects non-jsonl even with prefix":     {"codex", "/home/me/.codex/sessions/2025/11/rollout-abc.log", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := sourceByID(tt.id)
			if s == nil {
				t.Fatalf("sourceByID(%q) = nil", tt.id)
			}
			if got := s.Match(tt.path); got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
