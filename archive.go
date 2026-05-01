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

// migrateLegacyArchive relocates pre-multisource archives into the new
// per-source layout. Before v3 the archive root was
// <archive>/conversations/<proj-hash>/<session>.jsonl; after v3 Claude's
// bucket is <archive>/conversations/claude/<proj-hash>/<session>.jsonl.
// Anything already under a source's ID (claude/, codex/, unknown/) is left
// alone, so this is safe to call on every invocation.
func migrateLegacyArchive() error {
	root := conversationsDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	reserved := map[string]struct{}{
		"claude":  {},
		"codex":   {},
		"unknown": {},
	}
	claudeDir := filepath.Join(root, "claude")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, skip := reserved[e.Name()]; skip {
			continue
		}
		src := filepath.Join(root, e.Name())
		dst := filepath.Join(claudeDir, e.Name())
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return err
		}
		// If the destination already exists (partial prior migration), fall
		// back to moving individual files rather than rejecting the rename.
		if _, err := os.Stat(dst); err == nil {
			if err := mergeDirInto(src, dst); err != nil {
				return err
			}
			_ = os.Remove(src)
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// mergeDirInto moves every file from src into dst, preserving relative paths.
// Used by migrateLegacyArchive when a claude/<proj-hash> dir already exists
// and we need to merge rather than rename over it.
func mergeDirInto(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Honour the "never shrink the archive" rule: skip if the dst file
		// already equals or exceeds the src size.
		if di, err := os.Stat(target); err == nil {
			if si, err := os.Stat(path); err == nil && di.Size() >= si.Size() {
				return os.Remove(path)
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Rename(path, target)
	})
}

// archivedConversationPath asks every registered Source where the given
// live transcript should be mirrored. Sources that don't recognise the
// path return "" so we fall through to the next one. The final fallback
// buckets unknown paths under conversations/unknown/ so nothing is lost.
func archivedConversationPath(livePath string) string {
	for _, s := range sources {
		if dst := s.ArchiveDst(livePath); dst != "" {
			return dst
		}
	}
	parent := filepath.Base(filepath.Dir(livePath))
	name := filepath.Base(livePath)
	return filepath.Join(conversationsDir(), "unknown", parent, name)
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

// mirrorAll walks every source's live trees and copies anything new/larger
// into the archive. Claude's Plans directory is mirrored too — it shares
// Claude's 30-day cleanup risk. Fast because archiveFile short-circuits when
// the dest is already at least as large as the source.
func mirrorAll(verbose bool) (int, int, error) {
	sessions := 0
	plans := 0

	for _, src := range sources {
		// Skip the first root (archive) — it's the destination, not a live
		// source to mirror from. Everything after that is an agent-owned
		// live tree the agent may rewrite or prune.
		roots := src.Roots()
		if len(roots) < 2 {
			continue
		}
		for _, root := range roots[1:] {
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !src.Match(p) {
					return nil
				}
				dst := src.ArchiveDst(p)
				if dst == "" {
					return nil
				}
				if err := archiveFile(p, dst); err != nil {
					if verbose {
						fmt.Fprintf(os.Stderr, "  ! [%s] %s: %v\n", src.ID(), p, err)
					}
					return nil
				}
				sessions++
				return nil
			})
			if err != nil && !os.IsNotExist(err) {
				return sessions, plans, err
			}
		}
	}

	home, _ := os.UserHomeDir()
	livePlans := filepath.Join(home, ".claude", "plans")
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
