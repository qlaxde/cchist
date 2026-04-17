package main

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Turn is one logical Q&A unit: a user prompt plus every assistant / tool
// response that followed it until the next real user prompt. Everything we
// need for ranking and display lives on this struct so the cache is fully
// self-contained.
type Turn struct {
	File          string
	SessionID     string
	Project       string // cwd reported by Claude Code
	TurnIdx       int
	Timestamp     string
	Slug          string
	UserText      string
	AssistantText string
	Text          string // tokenised blob (user + assistant + tool names)
	// RootUserUUID is the uuid of the earliest user message in the session.
	// Forks created via Claude Code's fork action copy the prefix verbatim,
	// so sessions sharing a RootUserUUID are members of the same fork family.
	RootUserUUID string
}

// --- JSONL records ---------------------------------------------------------
// Only the fields we care about are deserialised; the rest is discarded by
// json.Decoder.

type rawRecord struct {
	Type      string      `json:"type"`
	SessionID string      `json:"sessionId"`
	Cwd       string      `json:"cwd"`
	Slug      string      `json:"slug"`
	Timestamp string      `json:"timestamp"`
	UUID      string      `json:"uuid"`
	Message   *rawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`     // tool_use
	Input   json.RawMessage `json:"input"`    // tool_use
	Content json.RawMessage `json:"content"`  // tool_result (string or array)
}

var skipTypes = map[string]struct{}{
	"file-history-snapshot": {},
	"permission-mode":       {},
	"progress":              {},
	"attachment":            {},
	"summary":               {},
}

// parseSession reads a single Claude Code JSONL transcript and groups messages
// into Turns. A "real" user message starts a new turn; tool_result user
// messages are treated as continuations of the current assistant response.
func parseSession(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Claude transcripts routinely contain very long lines (multi-MB tool
	// outputs). Give the scanner headroom.
	scanner.Buffer(make([]byte, 1<<16), 1<<24)

	var turns []Turn
	var (
		sessionID     string
		cwd           string
		slug          string
		rootUserUUID  string
		inTurn        bool
		curTimestamp  string
		curUserText   strings.Builder
		curAsst       strings.Builder
		turnIdx       int
	)

	flush := func() {
		if !inTurn {
			return
		}
		user := strings.TrimSpace(curUserText.String())
		asst := strings.TrimSpace(curAsst.String())
		toolNames := extractToolNames(asst)
		blob := strings.TrimSpace(strings.Join(nonEmpty(user, asst, toolNames), "\n"))
		if blob != "" {
			turns = append(turns, Turn{
				File:          path,
				SessionID:     sessionID,
				Project:       cwd,
				TurnIdx:       turnIdx,
				Timestamp:     curTimestamp,
				Slug:          slug,
				UserText:      user,
				AssistantText: asst,
				Text:          blob,
				RootUserUUID:  rootUserUUID,
			})
			turnIdx++
		}
		inTurn = false
		curUserText.Reset()
		curAsst.Reset()
		curTimestamp = ""
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		// Harvest session-wide metadata from any record that carries it.
		if sessionID == "" && rec.SessionID != "" {
			sessionID = rec.SessionID
		}
		if cwd == "" && rec.Cwd != "" {
			cwd = rec.Cwd
		}
		if slug == "" && rec.Slug != "" {
			slug = rec.Slug
		}

		if _, skip := skipTypes[rec.Type]; skip {
			continue
		}

		switch rec.Type {
		case "user":
			if rec.Message == nil {
				continue
			}
			text, toolResultOnly := extractUserText(rec.Message.Content)
			if toolResultOnly {
				if text != "" {
					curAsst.WriteString(text)
					curAsst.WriteByte('\n')
				}
				continue
			}
			// Real user message: start a new turn. The first such uuid per
			// session becomes the fork-family key — Claude Code's fork action
			// copies the prefix verbatim so sibling sessions share this value.
			flush()
			inTurn = true
			curTimestamp = rec.Timestamp
			curUserText.WriteString(text)
			if rootUserUUID == "" && rec.UUID != "" {
				rootUserUUID = rec.UUID
			}
		case "assistant":
			if rec.Message == nil {
				continue
			}
			text := extractAssistantText(rec.Message.Content)
			if text != "" {
				curAsst.WriteString(text)
				curAsst.WriteByte('\n')
			}
		}
	}
	flush()
	return turns, scanner.Err()
}

// extractUserText returns (text, isToolResultOnly). isToolResultOnly is true
// when the message is purely a tool_result follow-up with no user-authored
// text; such messages must not open a new turn.
func extractUserText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	// The simple case: user prompt is a plain string.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true
		}
		return s, false
	}
	// Otherwise it's an array of content blocks.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", true
	}
	var parts []string
	hasReal := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			hasReal = true
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_result":
			if t := flattenToolResultContent(b.Content); t != "" {
				parts = append(parts, "[tool_result] "+truncate(t, 200))
			}
		default:
			hasReal = true
		}
	}
	return strings.Join(parts, "\n"), !hasReal
}

// extractAssistantText collapses an assistant message into a searchable blob.
// Tool uses render as "[tool:Name summary]" so tool names become searchable
// terms, matching the behaviour of the Python reference.
func extractAssistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "thinking":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			summary := summariseToolInput(b.Name, b.Input)
			if summary == "" {
				parts = append(parts, "[tool:"+b.Name+"]")
			} else {
				parts = append(parts, "[tool:"+b.Name+" "+summary+"]")
			}
		}
	}
	return strings.Join(parts, "\n")
}

// summariseToolInput picks the most useful field from a tool's input object
// so that e.g. file paths and bash commands become searchable text instead of
// being lost inside a JSON blob.
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	prefKeys := map[string][]string{
		"Read":     {"file_path"},
		"Edit":     {"file_path"},
		"Write":    {"file_path"},
		"Bash":     {"command"},
		"Grep":     {"pattern"},
		"Glob":     {"pattern"},
		"WebFetch": {"url"},
	}
	for _, key := range prefKeys[name] {
		if v, ok := m[key].(string); ok && v != "" {
			return truncate(v, 80)
		}
	}
	// Fallback: first non-empty string field, alphabetically ordered for
	// determinism across runs.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return truncate(v, 80)
		}
	}
	return ""
}

func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var toolRE = regexp.MustCompile(`\[tool:([A-Za-z0-9_-]+)`)

func extractToolNames(asst string) string {
	if asst == "" {
		return ""
	}
	matches := toolRE.FindAllStringSubmatch(asst, -1)
	if len(matches) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		seen[m[1]] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func nonEmpty(s ...string) []string {
	out := s[:0]
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

