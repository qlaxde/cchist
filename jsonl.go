package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Block is one piece of structured assistant-side content within a Turn. Tool
// results from follow-up user messages are stitched in here too, since they
// continue the assistant's work. Keeping blocks typed lets `cchist show`
// filter at display time (drop thinking, drop tool calls, etc.) without
// having to re-parse the source JSONL.
type Block struct {
	Kind string // BlockText | BlockThinking | BlockToolUse | BlockToolResult
	Name string // tool name on BlockToolUse; empty otherwise
	Text string // body: text/thinking content, tool input summary, or truncated tool result
}

const (
	BlockText       = "text"
	BlockThinking   = "thinking"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Render returns the inline string form of a block as it appears both in the
// search blob and in `show` output.
func (b Block) Render() string {
	switch b.Kind {
	case BlockText, BlockThinking:
		return b.Text
	case BlockToolUse:
		if b.Text == "" {
			return "[tool:" + b.Name + "]"
		}
		return "[tool:" + b.Name + " " + b.Text + "]"
	case BlockToolResult:
		if b.Text == "" {
			return "[tool_result]"
		}
		return "[tool_result] " + b.Text
	}
	return b.Text
}

// Turn is one logical Q&A unit: a user prompt plus every assistant / tool
// response that followed it until the next real user prompt. Everything we
// need for ranking and display lives on this struct so the cache is fully
// self-contained.
type Turn struct {
	// Source names the agent that produced this transcript ("claude", "codex", …).
	// Empty on legacy caches; treated as "claude" by consumers for backwards
	// compatibility until the cache is rebuilt.
	Source    string
	File      string
	SessionID string
	Project   string // cwd reported by the agent
	TurnIdx   int
	Timestamp string
	Slug      string
	UserText  string
	// Blocks holds the assistant side broken down by kind so `show` can filter
	// (e.g. text-only chat without thinking or tool noise). Tool_result blocks
	// from follow-up user messages live here too — they're conceptually part of
	// the assistant turn that triggered them.
	Blocks []Block
	Text   string // tokenised blob (user + block renders + tool names)
	// RootUserUUID is the uuid of the earliest user message in the session.
	// Forks created via Claude Code's fork action copy the prefix verbatim,
	// so sessions sharing a RootUserUUID are members of the same fork family.
	// Codex has no fork action, so Codex turns always leave this empty.
	RootUserUUID string
}

// AssistantText flattens every block into one string, mirroring the shape old
// callers expected before Blocks existed. Use Blocks directly when you need to
// filter by kind.
func (t Turn) AssistantText() string {
	if len(t.Blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.Blocks))
	for _, b := range t.Blocks {
		if r := b.Render(); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n")
}

// buildSearchBlob composes the indexable text for a turn from its user prompt,
// rendered blocks, and the unique tool names invoked. Tool names are appended
// separately so they survive tokenisation as standalone search terms — without
// this, "Bash" would only appear inside `[tool:Bash …]` and tokenisers that
// split on punctuation would drop it.
func buildSearchBlob(user string, blocks []Block) string {
	var parts []string
	if user != "" {
		parts = append(parts, user)
	}
	seenTools := map[string]struct{}{}
	for _, b := range blocks {
		if r := b.Render(); r != "" {
			parts = append(parts, r)
		}
		if b.Kind == BlockToolUse && b.Name != "" {
			seenTools[b.Name] = struct{}{}
		}
	}
	if len(seenTools) > 0 {
		names := make([]string, 0, len(seenTools))
		for n := range seenTools {
			names = append(names, n)
		}
		sort.Strings(names)
		parts = append(parts, strings.Join(names, " "))
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
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
		sessionID    string
		cwd          string
		slug         string
		rootUserUUID string
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
				File:         path,
				SessionID:    sessionID,
				Project:      cwd,
				TurnIdx:      turnIdx,
				Timestamp:    curTimestamp,
				Slug:         slug,
				UserText:     user,
				Blocks:       curBlocks,
				Text:         blob,
				RootUserUUID: rootUserUUID,
			})
			turnIdx++
		}
		inTurn = false
		curUserText.Reset()
		curBlocks = nil
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
			text, toolResults, isToolResultOnly := extractUserContent(rec.Message.Content)
			if isToolResultOnly {
				curBlocks = append(curBlocks, toolResults...)
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
			curBlocks = append(curBlocks, extractAssistantBlocks(rec.Message.Content)...)
		}
	}
	flush()
	return turns, scanner.Err()
}

// extractUserContent splits a user message into (authored text, tool_result
// blocks, isToolResultOnly). isToolResultOnly is true when the message is
// purely a tool_result follow-up with no user-authored text; such messages
// must not open a new turn — their results are appended to the in-flight
// assistant turn instead.
func extractUserContent(raw json.RawMessage) (string, []Block, bool) {
	if len(raw) == 0 {
		return "", nil, true
	}
	// The simple case: user prompt is a plain string.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil, true
		}
		return s, nil, false
	}
	// Otherwise it's an array of content blocks.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, true
	}
	var (
		parts       []string
		toolResults []Block
		hasReal     bool
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			hasReal = true
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_result":
			if t := flattenToolResultContent(b.Content); t != "" {
				toolResults = append(toolResults, Block{
					Kind: BlockToolResult,
					Text: truncate(t, parseToolResultCap),
				})
			}
		default:
			hasReal = true
		}
	}
	return strings.Join(parts, "\n"), toolResults, !hasReal
}

// extractAssistantBlocks turns one assistant message's content into typed
// Blocks. A bare-string content is treated as a single text block.
func extractAssistantBlocks(raw json.RawMessage) []Block {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		if s == "" {
			return nil
		}
		return []Block{{Kind: BlockText, Text: s}}
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var out []Block
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, Block{Kind: BlockText, Text: b.Text})
			}
		case "thinking":
			if b.Text != "" {
				out = append(out, Block{Kind: BlockThinking, Text: b.Text})
			}
		case "tool_use":
			out = append(out, Block{
				Kind: BlockToolUse,
				Name: b.Name,
				Text: summariseToolInput(b.Name, b.Input),
			})
		}
	}
	return out
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
			return truncate(v, parseToolInputCap)
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
			return truncate(v, parseToolInputCap)
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

// parseToolInputCap and parseToolResultCap clip large tool payloads at parse
// time so the cache stays compact (tool results in particular can be megabytes
// of file content). `cchist show --full` lifts both to 0 (= no clip) for one
// re-parse so agents can recover the real content.
var (
	parseToolInputCap  = 80
	parseToolResultCap = 200
)

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

