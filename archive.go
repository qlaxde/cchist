package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// archiveFile mirrors a single file from src to dst, writing atomically so a
// concurrent reader (or the next cchist invocation) never sees a torn file.
// If dst already exists and is at least as large as src, the copy is skipped
// — we only ever want to grow the archive copy, never truncate it (the whole
// point is to survive post-compact truncation of the live JSONL).
func archiveFile(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if di, err := os.Stat(dst); err == nil {
		if di.Size() >= si.Size() {
			// Archive already has the full transcript (possibly a pre-compact
			// snapshot). Touch it if mtime is older — but never shrink.
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Preserve the source mtime so cache invalidation logic elsewhere stays
	// consistent between live and archived copies.
	_ = os.Chtimes(tmp, si.ModTime(), si.ModTime())
	return os.Rename(tmp, dst)
}

// archivedConversationPath returns where a given live JSONL should be mirrored
// to. We replicate Claude's own directory tree relative to ~/.claude/projects
// so that nested paths (e.g. <proj>/subagents/<file>.jsonl) survive intact —
// otherwise dedup between live and archive breaks and the index double-counts.
func archivedConversationPath(livePath string) string {
	home, _ := os.UserHomeDir()
	projectsRoot := filepath.Join(home, ".claude", "projects")
	if rel, err := filepath.Rel(projectsRoot, livePath); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(conversationsDir(), rel)
	}
	// Fallback for paths outside the canonical projects root — keep the
	// immediate parent so we still get a sensible, collision-resistant path.
	parent := filepath.Base(filepath.Dir(livePath))
	name := filepath.Base(livePath)
	return filepath.Join(conversationsDir(), parent, name)
}

// archiveSessionByPath snapshots one transcript into the archive and also
// copies any plan referenced via the transcript's `slug` field.
func archiveSessionByPath(livePath string) error {
	dst := archivedConversationPath(livePath)
	if err := archiveFile(livePath, dst); err != nil {
		return err
	}
	if slug := extractSlugFromJSONL(livePath); slug != "" {
		if err := archivePlan(slug); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// extractSlugFromJSONL reads only enough lines to find a slug, so this is
// cheap even on very large transcripts. The slug appears on the first
// user message as written by Claude Code.
func extractSlugFromJSONL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<24)
	for i := 0; sc.Scan() && i < 40; i++ {
		var rec struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Slug != "" {
			return rec.Slug
		}
	}
	return ""
}

// archivePlan copies the plan markdown file for a slug and all of its
// subagent variants (<slug>-agent-<hash>.md). Subagent plans carry research
// findings that aren't in the session transcript, so losing them loses real
// information.
func archivePlan(slug string) error {
	home, _ := os.UserHomeDir()
	plansSrc := filepath.Join(home, ".claude", "plans")
	entries, err := os.ReadDir(plansSrc)
	if err != nil {
		return err
	}
	prefix := slug
	matched := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		// Match either "<slug>.md" or "<slug>-agent-*.md".
		base := strings.TrimSuffix(name, ".md")
		if base != prefix && !strings.HasPrefix(base, prefix+"-agent-") {
			continue
		}
		if err := archiveFile(
			filepath.Join(plansSrc, name),
			filepath.Join(plansArchiveDir(), name),
		); err != nil {
			return err
		}
		matched++
	}
	if matched == 0 {
		return os.ErrNotExist
	}
	return nil
}

// mirrorAll walks every live JSONL and plan and copies anything new/larger
// into the archive. Fast because archiveFile short-circuits when the dest
// is already at least as large as the source (the common case).
func mirrorAll(verbose bool) (int, int, error) {
	home, _ := os.UserHomeDir()
	liveProjects := filepath.Join(home, ".claude", "projects")
	livePlans := filepath.Join(home, ".claude", "plans")

	sessions := 0
	plans := 0

	err := filepath.WalkDir(liveProjects, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		dst := archivedConversationPath(p)
		if err := archiveFile(p, dst); err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", p, err)
			}
			return nil
		}
		sessions++
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return sessions, plans, err
	}

	entries, err := os.ReadDir(livePlans)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			src := filepath.Join(livePlans, e.Name())
			dst := filepath.Join(plansArchiveDir(), e.Name())
			if err := archiveFile(src, dst); err != nil {
				if verbose {
					fmt.Fprintf(os.Stderr, "  ! plan %s: %v\n", e.Name(), err)
				}
				continue
			}
			plans++
		}
	}
	return sessions, plans, nil
}
