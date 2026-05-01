package main

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const codexMetaLine = `{"timestamp":"2025-11-07T14:25:23.752Z","type":"session_meta","payload":{"id":"019a5eb5-2f12-7c13-9400-fa1830934949","cwd":"/Users/me/project"}}`

func TestParseCodex(t *testing.T) {
	envContext := `{"timestamp":"2025-11-07T14:25:33.753Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/Users/me/project</cwd>\n</environment_context>"}]}}`
	userFix := `{"timestamp":"2025-11-07T14:25:44.978Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the migration"}]}}`
	asstReply := `{"timestamp":"2025-11-07T14:25:46.100Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running the migration now."}]}}`
	shellCall := `{"timestamp":"2025-11-07T14:25:54.327Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"git status -sb\"],\"workdir\":\"/Users/me/project\"}","call_id":"call_X"}}`

	tests := map[string]struct {
		lines []string
		want  []Turn
	}{
		"user-only turn strips environment_context": {
			lines: []string{codexMetaLine, envContext, userFix},
			want: []Turn{{
				Source:    "codex",
				SessionID: "019a5eb5-2f12-7c13-9400-fa1830934949",
				Project:   "/Users/me/project",
				Timestamp: "2025-11-07T14:25:44.978Z",
				UserText:  "fix the migration",
				Text:      "fix the migration",
			}},
		},
		"user + assistant": {
			lines: []string{codexMetaLine, userFix, asstReply},
			want: []Turn{{
				Source:    "codex",
				SessionID: "019a5eb5-2f12-7c13-9400-fa1830934949",
				Project:   "/Users/me/project",
				Timestamp: "2025-11-07T14:25:44.978Z",
				UserText:  "fix the migration",
				Blocks:    []Block{{Kind: BlockText, Text: "Running the migration now."}},
				Text:      "fix the migration\nRunning the migration now.",
			}},
		},
		"shell function_call renders as [tool:Bash …]": {
			lines: []string{codexMetaLine, userFix, shellCall},
			want: []Turn{{
				Source:    "codex",
				SessionID: "019a5eb5-2f12-7c13-9400-fa1830934949",
				Project:   "/Users/me/project",
				Timestamp: "2025-11-07T14:25:44.978Z",
				UserText:  "fix the migration",
				Blocks:    []Block{{Kind: BlockToolUse, Name: "Bash", Text: "git status -sb"}},
				Text:      "fix the migration\n[tool:Bash git status -sb]\nBash",
			}},
		},
	}

	ignore := cmpopts.IgnoreFields(Turn{}, "File", "TurnIdx")
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseCodex(strings.NewReader(strings.Join(tt.lines, "\n")))
			if err != nil {
				t.Fatalf("parseCodex: %v", err)
			}
			if diff := cmp.Diff(tt.want, got, ignore); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
