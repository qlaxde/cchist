package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sessionSummary is the denormalised view used by the status commands.
// Holding it here avoids re-deriving per-command.
type sessionSummary struct {
	SessionID string
	Project   string
	Slug      string
	FirstTS   string
	LastTS    string
	Turns     int
	FirstUser string
	Status    string // "completed" or ""
	Completed string // completedAt timestamp
}

func buildSessionSummaries(cache *Cache, meta *Metadata) map[string]*sessionSummary {
	out := make(map[string]*sessionSummary)
	for _, turns := range cache.TurnsByFile {
		for _, t := range turns {
			if t.SessionID == "" {
				continue
			}
			s, ok := out[t.SessionID]
			if !ok {
				s = &sessionSummary{
					SessionID: t.SessionID,
					Project:   t.Project,
					Slug:      t.Slug,
					FirstTS:   t.Timestamp,
					LastTS:    t.Timestamp,
					FirstUser: t.UserText,
				}
				out[t.SessionID] = s
			}
			s.Turns++
			if t.Timestamp > s.LastTS {
				s.LastTS = t.Timestamp
			}
			if s.FirstTS == "" || (t.Timestamp != "" && t.Timestamp < s.FirstTS) {
				s.FirstTS = t.Timestamp
				s.FirstUser = t.UserText
			}
		}
	}
	// Layer metadata on top so one source of truth lives in the summary.
	for id, s := range out {
		if m, ok := meta.Sessions[id]; ok {
			s.Status = m.Status
			s.Completed = m.CompletedAt
		}
	}
	return out
}

// --- complete / uncomplete -------------------------------------------------

func cmdComplete(argv []string) error {
	return markCompletion(argv, true)
}

func cmdUncomplete(argv []string) error {
	return markCompletion(argv, false)
}

func markCompletion(argv []string, completed bool) error {
	fs := flag.NewFlagSet("complete", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: cchist %s <session-id-prefix>",
			cond(completed, "complete", "uncomplete"))
	}
	prefix := fs.Arg(0)

	cache, _, err := refreshCache(historyDir(), cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
	})
	if err != nil {
		return err
	}
	ids := collectSessionIDs(cache)
	full, err := resolveSessionPrefix(prefix, ids)
	if err != nil {
		return err
	}
	meta := loadMetadata()
	s := meta.session(full)
	if completed {
		s.Status = "completed"
		s.CompletedAt = nowUTC()
	} else {
		s.Status = ""
		s.CompletedAt = ""
	}
	if err := saveMetadata(meta); err != nil {
		return err
	}
	word := cond(completed, "completed", "reopened")
	fmt.Printf("%s %s\n", word, full)
	return nil
}

// --- done (shortcut) -------------------------------------------------------

func cmdDone(argv []string) error {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	global := fs.Bool("global", false, "ignore cwd filter; take globally most recent session")
	family := fs.Bool("family", false, "also mark every fork of the target session complete")
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

	// Explicit id-prefix arg wins over inference.
	if fs.NArg() >= 1 {
		prefix := fs.Arg(0)
		ids := keysOf(summaries)
		full, err := resolveSessionPrefix(prefix, ids)
		if err != nil {
			return err
		}
		return applyCompletionMaybeFamily(meta, summaries[full], summaries, rootByID, *family)
	}

	// No arg: pick the most recently active session, optionally scoped to cwd.
	cwd, _ := os.Getwd()
	var pick *sessionSummary
	for _, s := range summaries {
		if !*global && !cwdMatches(s.Project, cwd) {
			continue
		}
		if s.Status == "completed" {
			continue
		}
		if pick == nil || s.LastTS > pick.LastTS {
			pick = s
		}
	}
	if pick == nil {
		if !*global {
			return fmt.Errorf("no open session in %s — use `cchist done --global` or pass an id", cwd)
		}
		return fmt.Errorf("no open sessions found")
	}
	return applyCompletionMaybeFamily(meta, pick, summaries, rootByID, *family)
}

func applyCompletionMaybeFamily(meta *Metadata, s *sessionSummary, summaries map[string]*sessionSummary, rootByID map[string]string, family bool) error {
	targets := []*sessionSummary{s}
	if family {
		targets = familyMembersOf(s.SessionID, summaries, rootByID)
	}
	ts := nowUTC()
	for _, t := range targets {
		sm := meta.session(t.SessionID)
		sm.Status = "completed"
		sm.CompletedAt = ts
	}
	if err := saveMetadata(meta); err != nil {
		return err
	}
	for _, t := range targets {
		fmt.Printf("%s  %s  %s\n",
			color("✓ completed", colorGreen),
			color(t.SessionID[:min(8, len(t.SessionID))], colorCyan),
			shortProject(t.Project),
		)
		if t.FirstUser != "" {
			preview := collapseSpaces(t.FirstUser)
			if len(preview) > 80 {
				preview = preview[:80] + "…"
			}
			fmt.Printf("  %s\n", preview)
		}
	}
	if family && len(targets) > 1 {
		fmt.Printf("\nclosed %d fork(s) of the same family\n", len(targets))
	}
	return nil
}

// --- deprecate / undeprecate / deprecated ---------------------------------

func cmdDeprecate(argv []string) error {
	return setDeprecated(argv, true)
}

func cmdUndeprecate(argv []string) error {
	return setDeprecated(argv, false)
}

func setDeprecated(argv []string, on bool) error {
	fs := flag.NewFlagSet("deprecate", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: cchist %s <session-id-prefix>",
			cond(on, "deprecate", "undeprecate"))
	}
	cache, _, err := refreshCache(historyDir(), cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
	})
	if err != nil {
		return err
	}
	full, err := resolveSessionPrefix(fs.Arg(0), collectSessionIDs(cache))
	if err != nil {
		return err
	}
	meta := loadMetadata()
	meta.session(full).Deprecated = on
	if err := saveMetadata(meta); err != nil {
		return err
	}
	word := cond(on, "deprecated", "undeprecated")
	fmt.Printf("%s %s\n", word, full)
	return nil
}

func cmdDeprecated(argv []string) error {
	fs := flag.NewFlagSet("deprecated", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	meta := loadMetadata()
	var ids []string
	for id, m := range meta.Sessions {
		if m.Deprecated {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(ids)
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "no deprecated sessions")
		return nil
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}

// --- purge -----------------------------------------------------------------

func cmdPurge(argv []string) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: cchist purge <session-id-prefix>")
	}
	cache, _, err := refreshCache(historyDir(), cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
	})
	if err != nil {
		return err
	}
	full, err := resolveSessionPrefix(fs.Arg(0), collectSessionIDs(cache))
	if err != nil {
		return err
	}
	// Remove: archive copies (conversations + plan), metadata entry, live jsonl.
	removed := 0
	for fpath, turns := range cache.TurnsByFile {
		for _, t := range turns {
			if t.SessionID == full {
				if err := os.Remove(fpath); err == nil {
					removed++
				}
				break
			}
		}
	}
	meta := loadMetadata()
	delete(meta.Sessions, full)
	_ = saveMetadata(meta)
	fmt.Printf("purged %s (%d files removed)\n", full, removed)
	fmt.Fprintln(os.Stderr, "note: run `cchist reindex` to drop purged entries from the search index")
	return nil
}

// --- threads ---------------------------------------------------------------

func cmdThreads(argv []string) error {
	fs := flag.NewFlagSet("threads", flag.ContinueOnError)
	var c commonFlags
	bindCommon(fs, &c)
	// --closed surfaces completed/deprecated threads too. Kept out of
	// commonFlags because it only makes sense here — search and list don't
	// hide completed sessions by default.
	closed := fs.Bool("closed", false, "include completed and deprecated threads")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	cache, _, err := refreshCache(historyDir(), cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
		Force:        c.Reindex,
		Verbose:      c.Verbose,
	})
	if err != nil {
		return err
	}
	meta := loadMetadata()
	summaries := buildSessionSummaries(cache, meta)

	since, err := parseSince(c.Since)
	if err != nil {
		return err
	}
	cwdFilter := resolveCwdScope(&c)

	rows := make([]*sessionSummary, 0, len(summaries))
	for _, s := range summaries {
		if !*closed {
			if s.Status == "completed" {
				continue
			}
			if meta.isDeprecated(s.SessionID) {
				continue
			}
		}
		if c.Project != "" && !strings.Contains(strings.ToLower(s.Project), strings.ToLower(c.Project)) {
			continue
		}
		if cwdFilter != "" && !cwdMatches(s.Project, cwdFilter) {
			continue
		}
		if !since.IsZero() {
			dt, ok := parseTS(s.LastTS)
			if !ok || dt.Before(since) {
				continue
			}
		}
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastTS > rows[j].LastTS })

	// Group fork families so duplicate conversations collapse into a tree.
	// Limit is applied to the number of GROUPS, not individual sessions, so
	// a heavily-forked family doesn't push unrelated work off the screen.
	rootByID := collectRootUUIDs(cache)
	groups := groupThreadsByFamily(rows, rootByID)
	if c.Limit > 0 && len(groups) > c.Limit {
		groups = groups[:c.Limit]
	}

	if c.JSON {
		// In JSON mode, emit the full flat list — machines handle dups fine
		// and the family structure isn't lossy if consumers need it.
		return json.NewEncoder(os.Stdout).Encode(rows)
	}

	if len(groups) == 0 {
		fmt.Fprintln(os.Stderr, emptyHint(cwdFilter, "open threads"))
		return nil
	}

	runningByID := runningSessionIDs()
	for _, g := range groups {
		printThreadRow(g.Canonical, runningByID, "", len(g.Forks)+1, 0)
		for i, fork := range g.Forks {
			prefix := "├─"
			if i == len(g.Forks)-1 {
				prefix = "└─"
			}
			printThreadRow(fork, runningByID, prefix, len(g.Forks)+1, i+1)
		}
	}
	return nil
}

// printThreadRow renders one line of `cchist threads` output. branchPrefix
// draws the ├─ / └─ glyph when the row is a fork member; empty otherwise.
// familyTotal > 1 adds a "fork i/N" hint so users know the session has
// siblings without running `cchist forks`.
func printThreadRow(s *sessionSummary, runningByID map[string]int, branchPrefix string, familyTotal, memberIdx int) {
	statusBadge := ""
	if s.Status == "completed" {
		statusBadge = color("✓", colorGreen)
	} else if _, live := runningByID[s.SessionID]; live {
		statusBadge = color("●", colorYellow)
	} else {
		statusBadge = color("○", colorDim)
	}
	forkBadge := ""
	if familyTotal > 1 {
		forkBadge = color(formatForkBadge(memberIdx, familyTotal), colorYellow)
	}
	indent := ""
	if branchPrefix != "" {
		indent = "  "
	}
	header := strings.Join(filterEmpty([]string{
		indent + branchPrefix,
		statusBadge,
		color(s.SessionID[:min(8, len(s.SessionID))], colorCyan),
		shortTS(s.LastTS),
		color(fmt.Sprintf("%4dt", s.Turns), colorDim),
		color(shortProject(s.Project), colorGreen),
		color(s.Slug, colorDim),
		forkBadge,
	}), "  ")
	fmt.Println(header)
	previewIndent := "  "
	if branchPrefix != "" {
		previewIndent = "       "
	}
	if s.FirstUser != "" {
		preview := collapseSpaces(s.FirstUser)
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		fmt.Printf("%s%s\n", previewIndent, preview)
	}
	fmt.Printf("%s%s\n\n", previewIndent, color("claude --resume "+s.SessionID, colorDim))
}

func runningSessionIDs() map[string]int {
	procs, err := listClaudeProcesses()
	if err != nil {
		return map[string]int{}
	}
	out := make(map[string]int, len(procs))
	// Also fold in breadcrumbs from the SessionStart hook — more reliable
	// than argv parsing.
	for _, m := range loadCurrentMarkers() {
		if m.SessionID != "" && m.PID > 0 {
			out[m.SessionID] = m.PID
		}
	}
	for _, p := range procs {
		if p.SessionID != "" {
			out[p.SessionID] = p.PID
		}
	}
	return out
}

// --- running ---------------------------------------------------------------

func cmdRunning(argv []string) error {
	fs := flag.NewFlagSet("running", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	procs, err := listClaudeProcesses()
	if err != nil {
		return err
	}
	markers := loadCurrentMarkers()
	pidToMarker := make(map[int]currentMarker, len(markers))
	for _, m := range markers {
		pidToMarker[m.PID] = m
	}

	meta := loadMetadata()

	type row struct {
		PID       int    `json:"pid"`
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
		Status    string `json:"status"`
		Uptime    string `json:"uptime"`
		RSSMB     uint64 `json:"rss_mb"`
	}
	var rows []row
	var totalRSS uint64
	for _, p := range procs {
		r := row{
			PID:    p.PID,
			Uptime: p.Etime,
			RSSMB:  p.RSSBytes / (1024 * 1024),
		}
		if m, ok := pidToMarker[p.PID]; ok {
			r.SessionID = m.SessionID
			r.Project = m.Cwd
		} else if p.SessionID != "" {
			r.SessionID = p.SessionID
		}
		if r.SessionID != "" && meta.isCompleted(r.SessionID) {
			r.Status = "completed"
		} else {
			r.Status = "open"
		}
		totalRSS += p.RSSBytes
		rows = append(rows, r)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].RSSMB > rows[j].RSSMB })

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no claude processes running")
		return nil
	}
	fmt.Printf("%s  %s  %s  %s  %s  %s\n",
		pad("PID", 7),
		pad("SESSION", 9),
		pad("STATUS", 10),
		pad("UPTIME", 10),
		pad("RSS", 8),
		"PROJECT",
	)
	for _, r := range rows {
		sid := r.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		status := r.Status
		if status == "completed" {
			status = color(status, colorGreen)
		}
		fmt.Printf("%s  %s  %s  %s  %s  %s\n",
			pad(fmt.Sprintf("%d", r.PID), 7),
			color(pad(sid, 9), colorCyan),
			pad(status, 10),
			pad(r.Uptime, 10),
			pad(fmt.Sprintf("%dM", r.RSSMB), 8),
			shortProject(r.Project),
		)
	}
	fmt.Printf("\n%d process%s · %d MB total\n",
		len(rows), cond(len(rows) == 1, "", "es"), totalRSS/(1024*1024))
	return nil
}

// --- reap ------------------------------------------------------------------

func cmdReap(argv []string) error {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show what would be killed without doing it")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	procs, err := listClaudeProcesses()
	if err != nil {
		return err
	}
	meta := loadMetadata()
	markers := loadCurrentMarkers()
	pidToMarker := make(map[int]currentMarker, len(markers))
	for _, m := range markers {
		pidToMarker[m.PID] = m
	}

	targets := make([]claudeProcess, 0)
	for _, p := range procs {
		sid := p.SessionID
		if m, ok := pidToMarker[p.PID]; ok && sid == "" {
			sid = m.SessionID
		}
		if sid == "" {
			continue // can't confirm completion without an id — never kill unknowns
		}
		if !meta.isCompleted(sid) {
			continue
		}
		targets = append(targets, p)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to reap — no running-and-completed sessions")
		return nil
	}
	if *dry {
		for _, t := range targets {
			fmt.Printf("would kill pid=%d rss=%dM\n", t.PID, t.RSSBytes/(1024*1024))
		}
		return nil
	}
	killed := 0
	var freed uint64
	for _, t := range targets {
		if terminate(t.PID, 5*time.Second) {
			killed++
			freed += t.RSSBytes
			fmt.Printf("%s pid=%d  %s  %dM reclaimed\n",
				color("✓", colorGreen), t.PID,
				color(t.SessionID[:min(8, len(t.SessionID))], colorCyan),
				t.RSSBytes/(1024*1024))
		} else {
			fmt.Printf("%s pid=%d did not exit\n", color("!", colorYellow), t.PID)
		}
	}
	fmt.Printf("\nreaped %d, %d MB reclaimed\n", killed, freed/(1024*1024))
	return nil
}

// --- helpers ---------------------------------------------------------------

func collectSessionIDs(cache *Cache) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, turns := range cache.TurnsByFile {
		for _, t := range turns {
			if t.SessionID == "" {
				continue
			}
			if _, ok := seen[t.SessionID]; ok {
				continue
			}
			seen[t.SessionID] = struct{}{}
			ids = append(ids, t.SessionID)
		}
	}
	return ids
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func cond[T any](b bool, yes, no T) T {
	if b {
		return yes
	}
	return no
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// archivedConversationPathForHome is used by the purge command when scanning
// the archive directory; exported here so the test shim doesn't need access
// to unexported state.
func archivedConversationsRoot() string { return filepath.Join(archiveDir(), "conversations") }
