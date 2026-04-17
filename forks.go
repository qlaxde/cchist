package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Fork families are keyed by the uuid of the first user message in a session.
// Every member of the family has copied that uuid verbatim as a result of
// Claude Code's fork action; detection therefore needs nothing more than an
// equality check. Sessions without a RootUserUUID (legacy cache entries, or
// transcripts where no user message was parsed) are their own singleton
// families keyed on their sessionID.

// buildFamilies returns rootUUID -> sorted list of session IDs that share
// that uuid. Only families with more than one member are interesting to the
// UI; singletons are omitted.
func buildFamilies(summaries map[string]*sessionSummary, rootByID map[string]string) map[string][]string {
	out := make(map[string][]string)
	for id, root := range rootByID {
		if root == "" {
			continue
		}
		if _, ok := summaries[id]; !ok {
			continue
		}
		out[root] = append(out[root], id)
	}
	for k, v := range out {
		if len(v) < 2 {
			delete(out, k)
			continue
		}
		sort.Strings(v)
	}
	return out
}

// collectRootUUIDs walks the cache once and maps each sessionID to the
// RootUserUUID of its MAIN session file. Subagent JSONLs carry the parent's
// sessionId but have their own root — conflating them would sever fork
// families and attach spurious siblings. Main-session files live directly
// under <project-hash>/ while subagents live under <project-hash>/subagents/,
// which lets us disambiguate on path alone.
func collectRootUUIDs(cache *Cache) map[string]string {
	out := make(map[string]string)
	for fpath, turns := range cache.TurnsByFile {
		if strings.Contains(fpath, "/subagents/") {
			continue
		}
		for _, t := range turns {
			if t.SessionID == "" || t.RootUserUUID == "" {
				continue
			}
			if _, ok := out[t.SessionID]; ok {
				continue
			}
			out[t.SessionID] = t.RootUserUUID
			break
		}
	}
	return out
}

// familyMembersOf returns every session that shares the fork family of the
// given session ID. Always includes the session itself. Members are sorted
// by most-recent activity descending so the caller can pick a "canonical"
// representative trivially.
func familyMembersOf(sessionID string, summaries map[string]*sessionSummary, rootByID map[string]string) []*sessionSummary {
	root := rootByID[sessionID]
	members := []*sessionSummary{}
	if root == "" {
		// No known family; just return the session.
		if s, ok := summaries[sessionID]; ok {
			members = append(members, s)
		}
		return members
	}
	for id, s := range summaries {
		if rootByID[id] == root {
			members = append(members, s)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].LastTS > members[j].LastTS })
	return members
}

// --- cchist forks -----------------------------------------------------------

func cmdForks(argv []string) error {
	fs := flag.NewFlagSet("forks", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	cache, _, err := refreshCache(historyDir(), cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
	})
	if err != nil {
		return err
	}
	meta := loadMetadata()
	summaries := buildSessionSummaries(cache, meta)
	rootByID := collectRootUUIDs(cache)

	// Explicit id → show only that family.
	if fs.NArg() >= 1 {
		ids := keysOf(summaries)
		full, err := resolveSessionPrefix(fs.Arg(0), ids)
		if err != nil {
			return err
		}
		members := familyMembersOf(full, summaries, rootByID)
		return renderFamily(rootByID[full], members, *asJSON)
	}

	// No id → list every family with >1 member, newest activity first.
	families := buildFamilies(summaries, rootByID)
	if len(families) == 0 {
		fmt.Fprintln(os.Stderr, "no fork families found")
		return nil
	}

	// Sort families by the most-recent activity of any member.
	rootKeys := make([]string, 0, len(families))
	familyLastTS := make(map[string]string, len(families))
	for root, ids := range families {
		var latest string
		for _, id := range ids {
			if s, ok := summaries[id]; ok && s.LastTS > latest {
				latest = s.LastTS
			}
		}
		familyLastTS[root] = latest
		rootKeys = append(rootKeys, root)
	}
	sort.Slice(rootKeys, func(i, j int) bool { return familyLastTS[rootKeys[i]] > familyLastTS[rootKeys[j]] })

	if *asJSON {
		out := make(map[string][]string, len(families))
		for _, k := range rootKeys {
			out[k] = families[k]
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	for _, root := range rootKeys {
		members := make([]*sessionSummary, 0, len(families[root]))
		for _, id := range families[root] {
			if s, ok := summaries[id]; ok {
				members = append(members, s)
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i].LastTS > members[j].LastTS })
		if err := renderFamily(root, members, false); err != nil {
			return err
		}
	}
	return nil
}

// renderFamily pretty-prints one fork family as a tree: the canonical (most
// recent) session on top, forks indented below with a └─ branch glyph.
func renderFamily(rootUUID string, members []*sessionSummary, asJSON bool) error {
	if len(members) == 0 {
		return nil
	}
	if asJSON {
		type row struct {
			RootUUID string   `json:"root_uuid"`
			Members  []string `json:"members"`
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.SessionID)
		}
		return json.NewEncoder(os.Stdout).Encode(row{RootUUID: rootUUID, Members: ids})
	}
	fmt.Printf("%s  %d sessions share root %s\n",
		color("●", colorYellow), len(members),
		color(shortUUID(rootUUID), colorDim))
	for i, m := range members {
		branch := "├─"
		if i == len(members)-1 {
			branch = "└─"
		}
		status := color("○", colorDim)
		if m.Status == "completed" {
			status = color("✓", colorGreen)
		}
		preview := collapseSpaces(m.FirstUser)
		if len(preview) > 70 {
			preview = preview[:70] + "…"
		}
		fmt.Printf("  %s %s  %s  %s  %4dt  %s  %s\n",
			branch, status,
			color(m.SessionID[:min(8, len(m.SessionID))], colorCyan),
			shortTS(m.LastTS), m.Turns,
			color(shortProject(m.Project), colorGreen),
			color(m.Slug, colorDim),
		)
		fmt.Printf("       %s\n", color("claude --resume "+m.SessionID, colorDim))
	}
	fmt.Println()
	return nil
}

func shortUUID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}

// familyDedupFilter returns a predicate that keeps at most one session per
// fork family — whichever appears first in the iteration order. Used during
// search so duplicate fork hits don't dominate the result list.
func familyDedupFilter(rootByID map[string]string) func(sessionID string) bool {
	seen := make(map[string]struct{})
	return func(sid string) bool {
		root := rootByID[sid]
		if root == "" {
			return true
		}
		if _, dup := seen[root]; dup {
			return false
		}
		seen[root] = struct{}{}
		return true
	}
}

// groupThreadsByFamily partitions the open-threads list into (canonical,
// forks[]) groups. Canonical is the family member with the most recent
// activity. Sessions without a family (or alone in theirs) render as
// canonical with an empty forks slice.
type threadGroup struct {
	Canonical *sessionSummary
	Forks     []*sessionSummary
}

func groupThreadsByFamily(rows []*sessionSummary, rootByID map[string]string) []threadGroup {
	// Bucket rows by their rootUUID; sessions without a root become their
	// own solo bucket keyed on the sessionID.
	buckets := make(map[string][]*sessionSummary)
	order := []string{}
	for _, r := range rows {
		key := rootByID[r.SessionID]
		if key == "" {
			key = r.SessionID
		}
		if _, ok := buckets[key]; !ok {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], r)
	}
	out := make([]threadGroup, 0, len(buckets))
	for _, k := range order {
		members := buckets[k]
		sort.Slice(members, func(i, j int) bool { return members[i].LastTS > members[j].LastTS })
		out = append(out, threadGroup{
			Canonical: members[0],
			Forks:     members[1:],
		})
	}
	// Most-recent-canonical first, matches the non-grouped threads output.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Canonical.LastTS > out[j].Canonical.LastTS
	})
	return out
}

// formatForkBadge returns a short indicator like "fork 2/3" meaning "this is
// the 2nd most-recent member out of 3". Used in headers so users know a
// session belongs to a family without running `cchist forks`.
func formatForkBadge(memberIdx, total int) string {
	if total <= 1 {
		return ""
	}
	return fmt.Sprintf("fork %d/%d", memberIdx+1, total)
}

// String variants used to keep the single-family-render path tidy without
// pulling strings.Join into the hot path.
var _ = strings.TrimSpace
