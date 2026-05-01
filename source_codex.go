package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type codexSource struct{}

func (*codexSource) ID() string          { return "codex" }
func (*codexSource) DisplayName() string { return "Codex CLI" }

// Match accepts rollout-*.jsonl files. Codex writes session_index.jsonl and
// other non-transcript JSONLs in the same tree, so the filename prefix is
// required in addition to the extension.
func (*codexSource) Match(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.EqualFold(filepath.Ext(base), ".jsonl")
}

// Roots returns the archive mirror and the two live trees Codex uses:
// sessions (active) and archived_sessions (old rotations the CLI keeps).
// Honors $CODEX_HOME.
func (*codexSource) Roots() []string {
	base := codexBase()
	return []string{
		filepath.Join(conversationsDir(), "codex"),
		filepath.Join(base, "sessions"),
		filepath.Join(base, "archived_sessions"),
	}
}

func codexBase() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// Parse reads a Codex rollout JSONL into turns. File path stamps Turn.File
// so callers can map doc IDs back to source files; the parser itself works
// on io.Reader.
func (*codexSource) Parse(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	turns, err := parseCodex(f)
	if err != nil {
		return nil, err
	}
	for i := range turns {
		turns[i].File = path
	}
	return turns, nil
}

// ArchiveDst mirrors Codex's date-bucketed tree
// (YYYY/MM/DD/rollout-...jsonl) under <archive>/conversations/codex/.
// Returns "" for paths outside either live root so the registry can fall
// through to other sources.
func (*codexSource) ArchiveDst(livePath string) string {
	base := codexBase()
	for _, sub := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(base, sub)
		rel, err := filepath.Rel(root, livePath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join(conversationsDir(), "codex", sub, rel)
		}
	}
	return ""
}

// parseCodex reads a Codex CLI rollout JSONL (one JSON object per line) and
// groups its response_items into Turns, mirroring the shape parseSession
// produces for Claude transcripts so downstream code stays uniform.
//
// Codex records we care about:
//
//   - type:"session_meta"    payload:{id, cwd, …}           → session-wide metadata
//   - type:"response_item"   payload:{type:"message", role, content:[…]}
//     role:"user"            → starts a new turn
//     role:"assistant"       → continues the current turn
//   - type:"response_item"   payload:{type:"function_call"|"local_shell_call"|…}
//     → rendered as `[tool:Name …]` into the assistant text
//   - type:"event_msg", type:"turn_context"
//     → ignored (they duplicate info already captured)
//
// The first user message Codex injects is an `<environment_context>` wrapper
// that leaks shell/cwd metadata into search terms — we skip it wholesale.
func parseCodex(r io.Reader) ([]Turn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<24)

	var (
		turns        []Turn
		sessionID    string
		cwd          string
		inTurn       bool
		curTimestamp string
		curUserText  strings.Builder
		curBlocks    []Block
		turnIdx      int
	)

	flush := func() {
		if !inTurn {
			return
		}
		user := strings.TrimSpace(curUserText.String())
		blob := buildSearchBlob(user, curBlocks)
		if blob != "" {
			turns = append(turns, Turn{
				Source:    "codex",
				SessionID: sessionID,
				Project:   cwd,
				TurnIdx:   turnIdx,
				Timestamp: curTimestamp,
				UserText:  user,
				Blocks:    curBlocks,
				Text:      blob,
			})
			turnIdx++
		}
		inTurn = false
		curUserText.Reset()
		curBlocks = nil
		curTimestamp = ""
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec codexRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		switch rec.Type {
		case "session_meta":
			if rec.Payload != nil {
				if sessionID == "" {
					sessionID = rec.Payload.ID
				}
				if cwd == "" {
					cwd = rec.Payload.Cwd
				}
			}
		case "response_item":
			if rec.Payload == nil {
				continue
			}
			switch rec.Payload.Type {
			case "message":
				text := extractCodexMessageText(rec.Payload.Content)
				if text == "" {
					continue
				}
				switch rec.Payload.Role {
				case "user":
					if isEnvironmentContext(text) {
						continue
					}
					flush()
					inTurn = true
					curTimestamp = rec.Timestamp
					curUserText.WriteString(text)
				case "assistant":
					curBlocks = append(curBlocks, Block{Kind: BlockText, Text: text})
				}
			case "function_call", "local_shell_call", "custom_tool_call", "web_search_call":
				name, summary := renderCodexToolCall(rec.Payload)
				if name == "" {
					continue
				}
				curBlocks = append(curBlocks, Block{Kind: BlockToolUse, Name: name, Text: summary})
			}
		}
	}
	flush()
	return turns, sc.Err()
}

type codexRecord struct {
	Timestamp string        `json:"timestamp"`
	Type      string        `json:"type"`
	Payload   *codexPayload `json:"payload"`
}

type codexPayload struct {
	ID        string              `json:"id"`
	Cwd       string              `json:"cwd"`
	Type      string              `json:"type"`
	Role      string              `json:"role"`
	Content   []codexContentBlock `json:"content"`
	Name      string              `json:"name"`
	Arguments string              `json:"arguments"`
	Action    *codexShellAction   `json:"action"`
}

type codexShellAction struct {
	Command []string `json:"command"`
}

type codexContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// extractCodexMessageText concatenates all text-bearing blocks. Codex uses
// "input_text" for user input and "output_text" for assistant output; we
// treat both identically for search.
func extractCodexMessageText(blocks []codexContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Text == "" {
			continue
		}
		switch b.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// renderCodexToolCall returns the normalised tool name and a short argument
// summary suitable for indexing. Codex's native names map to Claude's set
// (shell → Bash) so search terms are consistent across sources.
func renderCodexToolCall(p *codexPayload) (name, summary string) {
	native := p.Name
	if p.Type == "local_shell_call" {
		native = "shell"
	}
	switch native {
	case "shell", "exec_command", "write_stdin":
		name = "Bash"
	case "":
		name = p.Type
	default:
		name = native
	}

	switch p.Type {
	case "function_call":
		if p.Arguments == "" {
			return name, ""
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(p.Arguments), &args); err != nil {
			return name, truncate(p.Arguments, parseToolInputCap)
		}
		if name == "Bash" {
			if cmd := shellCommandSummary(args); cmd != "" {
				return name, truncate(cmd, parseToolInputCap)
			}
		}
		return name, codexFirstStringField(args)
	case "local_shell_call":
		if p.Action == nil {
			return name, ""
		}
		cmd := unwrapShellCommand(p.Action.Command)
		return name, truncate(cmd, parseToolInputCap)
	}
	return name, ""
}

// shellCommandSummary reads a shell function_call's arguments and returns the
// user-facing command. Codex wraps commands as `bash -lc <cmd>`; we strip the
// wrapper so the inner command is what's indexed.
func shellCommandSummary(args map[string]any) string {
	cmd, ok := args["command"].([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(cmd))
	for _, p := range cmd {
		if s, ok := p.(string); ok {
			parts = append(parts, s)
		}
	}
	return unwrapShellCommand(parts)
}

func unwrapShellCommand(parts []string) string {
	if len(parts) == 3 && parts[0] == "bash" && parts[1] == "-lc" {
		return parts[2]
	}
	return strings.Join(parts, " ")
}

// codexFirstStringField returns the first non-empty string value from a
// tool-call arguments object, alphabetically ordered for determinism. Matches
// the fallback path in summariseToolInput so both sources behave the same when
// no preferred key exists.
func codexFirstStringField(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return truncate(v, parseToolInputCap)
		}
	}
	return ""
}

// isEnvironmentContext reports whether a user message is synthetic Codex
// preamble rather than a real user turn. Codex injects two such messages at
// session start:
//
//   - `<environment_context>…</environment_context>` — shell/cwd metadata
//   - `# AGENTS.md instructions for <path>` — the project's AGENTS.md file
//
// Both would dominate BM25 scoring (AGENTS.md is long) and would misrepresent
// the first "real" user turn in previews. Skipping them keeps the index and
// the list/threads previews honest.
func isEnvironmentContext(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "<environment_context>") && strings.HasSuffix(t, "</environment_context>") {
		return true
	}
	if strings.HasPrefix(t, "# AGENTS.md instructions for ") {
		return true
	}
	return false
}
