package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
// block Claude from compacting / ending.
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
	case "SessionEnd":
		return handleSessionEnd(&in)
	case "PreCompact":
		return handlePreCompact(&in)
	default:
		if in.TranscriptPath != "" {
			_ = archiveSessionByPath(in.TranscriptPath)
		}
		return nil
	}
}

// handleSessionEnd archives the final transcript state on clean exit.
func handleSessionEnd(in *hookInput) error {
	if in.TranscriptPath != "" {
		_ = archiveSessionByPath(in.TranscriptPath)
	}
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
