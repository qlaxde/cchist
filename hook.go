package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// hookInput is the subset of Claude Code's hook payload we care about. We
// deserialise lazily so unknown fields in future hook versions don't break us.
type hookInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// cmdHook is the entry point for every Claude Code lifecycle event we care
// about. Reads the payload from stdin, dispatches by event name. It is
// deliberately tolerant: we would rather swallow an error silently than
// block Claude from compacting / starting / ending.
func cmdHook(argv []string) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}
	var in hookInput
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &in)
	}

	switch in.HookEventName {
	case "SessionStart":
		return handleSessionStart(&in)
	case "SessionEnd":
		return handleSessionEnd(&in)
	case "PreCompact":
		return handlePreCompact(&in)
	default:
		// Unknown event or missing event — try to archive whatever transcript
		// was supplied as a safety net, then exit.
		if in.TranscriptPath != "" {
			_ = archiveSessionByPath(in.TranscriptPath)
		}
		return nil
	}
}

// handleSessionStart drops a breadcrumb so `cchist done` can find the current
// session without needing a new prompt-in-REPL roundtrip. Keyed on PPID —
// that's the Claude CLI process, not the shell wrapper around the hook.
func handleSessionStart(in *hookInput) error {
	if in.SessionID == "" {
		return nil
	}
	if err := os.MkdirAll(currentDir(), 0o755); err != nil {
		return nil
	}
	ppid := os.Getppid()
	path := filepath.Join(currentDir(), strconv.Itoa(ppid)+".json")
	body, _ := json.MarshalIndent(map[string]any{
		"sessionId":      in.SessionID,
		"transcriptPath": in.TranscriptPath,
		"cwd":            in.Cwd,
		"pid":            ppid,
		"startedAt":      nowUTC(),
	}, "", "  ")
	return os.WriteFile(path, body, 0o644)
}

// handleSessionEnd archives the final transcript state and clears the
// breadcrumb. Running `cchist done` after this still works because it also
// falls back to "most recently ended session".
func handleSessionEnd(in *hookInput) error {
	if in.TranscriptPath != "" {
		_ = archiveSessionByPath(in.TranscriptPath)
	}
	// Remove any current-marker that references this session id.
	cleanupCurrentMarkers(in.SessionID)
	return nil
}

// handlePreCompact is the one that actually matters for the "I lost my
// conversation" problem — snapshot BEFORE Claude rewrites the transcript.
func handlePreCompact(in *hookInput) error {
	if in.TranscriptPath != "" {
		return archiveSessionByPath(in.TranscriptPath)
	}
	return nil
}

// cleanupCurrentMarkers deletes any breadcrumb file whose payload names the
// given session id. This keeps `cchist running` honest after clean exits.
func cleanupCurrentMarkers(sessionID string) {
	entries, err := os.ReadDir(currentDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(currentDir(), e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var body struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			continue
		}
		if sessionID != "" && body.SessionID == sessionID {
			os.Remove(p)
		}
	}
}

// loadCurrentMarkers returns every breadcrumb currently on disk. The caller
// can cross-reference pids with ps output to distinguish live sessions from
// crashed ones whose marker never got cleaned up.
type currentMarker struct {
	Path           string
	PID            int
	SessionID      string
	TranscriptPath string
	Cwd            string
	StartedAt      string
}

func loadCurrentMarkers() []currentMarker {
	entries, err := os.ReadDir(currentDir())
	if err != nil {
		return nil
	}
	var out []currentMarker
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(currentDir(), e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var body struct {
			PID            int    `json:"pid"`
			SessionID      string `json:"sessionId"`
			TranscriptPath string `json:"transcriptPath"`
			Cwd            string `json:"cwd"`
			StartedAt      string `json:"startedAt"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			continue
		}
		out = append(out, currentMarker{
			Path: p, PID: body.PID, SessionID: body.SessionID,
			TranscriptPath: body.TranscriptPath, Cwd: body.Cwd,
			StartedAt: body.StartedAt,
		})
	}
	return out
}

// cmdArchive is the manual "mirror everything now" command. Mostly useful as
// a one-off seed; the hooks + refreshCache keep the archive fresh after that.
func cmdArchive(argv []string) error {
	sessions, plans, err := mirrorAll(true)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "archived %d sessions, %d plans\n", sessions, plans)
	return nil
}
