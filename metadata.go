package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Metadata persists user-managed session/plan state that doesn't belong
// inside the transcripts themselves: completion status, soft-hides, notes.
// A single JSON file keeps the model easy to inspect and edit by hand.
type Metadata struct {
	Sessions map[string]*SessionMeta `json:"sessions"`
	Plans    map[string]*PlanMeta    `json:"plans"`
}

type SessionMeta struct {
	Status      string `json:"status,omitempty"` // "completed" or empty (= open)
	CompletedAt string `json:"completedAt,omitempty"`
	Deprecated  bool   `json:"deprecated,omitempty"`
	Note        string `json:"note,omitempty"`
}

type PlanMeta struct {
	Deprecated bool   `json:"deprecated,omitempty"`
	Note       string `json:"note,omitempty"`
}

func archiveDir() string {
	if v := os.Getenv("CCHIST_ARCHIVE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "cchist")
}

func metadataPath() string    { return filepath.Join(archiveDir(), "metadata.json") }
func currentDir() string       { return filepath.Join(archiveDir(), "current") }
func conversationsDir() string { return filepath.Join(archiveDir(), "conversations") }
func plansArchiveDir() string  { return filepath.Join(archiveDir(), "plans") }

// loadMetadata returns a writable Metadata struct, creating an empty one if
// the file is missing or corrupt. We never bubble read errors to callers
// because a missing metadata file is a valid starting state.
func loadMetadata() *Metadata {
	m := &Metadata{
		Sessions: make(map[string]*SessionMeta),
		Plans:    make(map[string]*PlanMeta),
	}
	data, err := os.ReadFile(metadataPath())
	if err != nil {
		return m
	}
	if err := json.Unmarshal(data, m); err != nil {
		return &Metadata{
			Sessions: make(map[string]*SessionMeta),
			Plans:    make(map[string]*PlanMeta),
		}
	}
	if m.Sessions == nil {
		m.Sessions = make(map[string]*SessionMeta)
	}
	if m.Plans == nil {
		m.Plans = make(map[string]*PlanMeta)
	}
	return m
}

func saveMetadata(m *Metadata) error {
	if err := os.MkdirAll(archiveDir(), 0o755); err != nil {
		return err
	}
	tmp := metadataPath() + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metadataPath())
}

// session accessor helpers — create-on-read semantics keep callers terse.

func (m *Metadata) session(id string) *SessionMeta {
	s, ok := m.Sessions[id]
	if !ok {
		s = &SessionMeta{}
		m.Sessions[id] = s
	}
	return s
}

func (m *Metadata) plan(slug string) *PlanMeta {
	p, ok := m.Plans[slug]
	if !ok {
		p = &PlanMeta{}
		m.Plans[slug] = p
	}
	return p
}

// isCompleted / isDeprecated are read-only accessors that treat "no entry"
// as the default (open, not deprecated). Used in search/list filters.
func (m *Metadata) isCompleted(id string) bool {
	s, ok := m.Sessions[id]
	return ok && s.Status == "completed"
}

func (m *Metadata) isDeprecated(id string) bool {
	s, ok := m.Sessions[id]
	return ok && s.Deprecated
}

func (m *Metadata) isPlanDeprecated(slug string) bool {
	p, ok := m.Plans[slug]
	return ok && p.Deprecated
}

// resolveSessionPrefix returns the single session ID that starts with the
// given prefix. Ambiguous prefixes return an error listing the matches so
// the user can disambiguate without re-running with --list.
func resolveSessionPrefix(prefix string, knownIDs []string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("session id required")
	}
	var matches []string
	seen := make(map[string]struct{})
	for _, id := range knownIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if id == prefix {
			return id, nil
		}
		if len(prefix) <= len(id) && id[:len(prefix)] == prefix {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matches %q", prefix)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("ambiguous prefix %q, matches: %v", prefix, matches)
	}
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
