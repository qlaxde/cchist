package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// sessionSummary is the denormalised view used by the status commands.
// Holding it here avoids re-deriving per-command.
type sessionSummary struct {
	SessionID string
	Source    string
	Project   string
	Slug      string
	FirstTS   string
	LastTS    string
	Turns     int
	FirstUser string
}

func buildSessionSummaries(cache *Cache) map[string]*sessionSummary {
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
					Source:    t.Source,
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
	return out
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
	cache, _, err := refreshCache(cachePath(), refreshOptions{
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
	cache, _, err := refreshCache(cachePath(), refreshOptions{
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
	if err := fs.Parse(argv); err != nil {
		return err
	}

	cache, _, err := refreshCache(cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
		Force:        c.Reindex,
		Verbose:      c.Verbose,
	})
	if err != nil {
		return err
	}
	meta := loadMetadata()
	summaries := buildSessionSummaries(cache)

	since, err := parseSince(c.Since)
	if err != nil {
		return err
	}
	cwdFilter := resolveCwdScope(&c)

	hideCurrent := currentSessionID()
	rows := make([]*sessionSummary, 0, len(summaries))
	for _, s := range summaries {
		if !c.IncludeDeprecated && meta.isDeprecated(s.SessionID) {
			continue
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
		if hideCurrent != "" && s.SessionID == hideCurrent {
			continue
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

	for _, g := range groups {
		printThreadRow(g.Canonical, "", len(g.Forks)+1, 0)
		for i, fork := range g.Forks {
			prefix := "├─"
			if i == len(g.Forks)-1 {
				prefix = "└─"
			}
			printThreadRow(fork, prefix, len(g.Forks)+1, i+1)
		}
	}
	return nil
}

// printThreadRow renders one line of `cchist threads` output. branchPrefix
// draws the ├─ / └─ glyph when the row is a fork member; empty otherwise.
// familyTotal > 1 adds a "fork i/N" hint so users know the session has
// siblings without running `cchist forks`.
func printThreadRow(s *sessionSummary, branchPrefix string, familyTotal, memberIdx int) {
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
		color(sourceBadge(s.Source), colorDim),
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
	if resume := resumeCommand(s.Source, s.SessionID); resume != "" {
		fmt.Printf("%s%s\n\n", previewIndent, color(resume, colorDim))
	} else {
		fmt.Println()
	}
}

// --- resume ----------------------------------------------------------------

// cmdResume prints the resume one-liner for the most recent open thread in
// cwd (or globally with -a). Collapses the "cchist threads → copy resume
// line" two-step into a single call.
func cmdResume(argv []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	var c commonFlags
	bindCommon(fs, &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	pick, err := pickMostRecent(&c, pickOpts{excludeCurrent: true})
	if err != nil {
		return err
	}
	cmd := resumeCommand(pick.Source, pick.SessionID)
	if cmd == "" {
		return fmt.Errorf("no known resume command for source %q", pick.Source)
	}
	fmt.Println(cmd)
	if !c.Quiet {
		fmt.Fprintf(os.Stderr, "# %s  %s  %s\n",
			color(pick.SessionID[:min(8, len(pick.SessionID))], colorCyan),
			shortTS(pick.LastTS),
			color(shortProject(pick.Project), colorGreen),
		)
	}
	return nil
}

// --- prev ------------------------------------------------------------------

// cmdPrev shows the most recent session in cwd (or -a) that isn't the live
// one running cchist. With a query, greps that session for matching turns;
// otherwise shows the whole session. Faster than search+show when the agent
// just wants "what was I doing last time".
func cmdPrev(argv []string) error {
	fs := flag.NewFlagSet("prev", flag.ContinueOnError)
	var c commonFlags
	// bindCommon already wires -n/--limit to c.Limit; we reuse it as the
	// turn-limit when passing through to show.
	bindCommon(fs, &c)
	role := fs.String("role", "both", "user | assistant | both")
	withThinking := fs.Bool("with-thinking", false, "include assistant thinking blocks")
	withTools := fs.Bool("with-tools", false, "include tool_use and tool_result blocks")
	allContent := fs.Bool("all-blocks", false, "shorthand for --with-thinking --with-tools")
	full := fs.Bool("full", false, "untruncated tool inputs/results")
	// prev defaults to the LAST 10 turns — agents asking "what was I doing"
	// want the recap, not the cold start. Pass --no-tail or --limit 0 to opt
	// out (limit 0 = render everything).
	tail := fs.Bool("tail", true, "with --limit, take last N turns (default true for prev)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	pick, err := pickMostRecent(&c, pickOpts{excludeCurrent: true})
	if err != nil {
		return err
	}
	if !c.Quiet {
		fmt.Fprintf(os.Stderr, "# prev: %s  %s  %s\n",
			color(pick.SessionID[:min(8, len(pick.SessionID))], colorCyan),
			shortTS(pick.LastTS),
			color(shortProject(pick.Project), colorGreen),
		)
	}
	// If a free-text query is present, route through search scoped to this
	// session — same ranking, same operators agents already know.
	if q := strings.TrimSpace(strings.Join(fs.Args(), " ")); q != "" {
		showArgs := []string{pick.SessionID[:min(8, len(pick.SessionID))]}
		// Just hand off to cmdShow with the prefix; query-within-prev is left
		// to the caller via `cchist <q> -p <project-of-prev>`. Keeps prev
		// honest about its one job.
		_ = showArgs
		// Honour query positional by piggy-backing on search to keep the API
		// promise — query positional => grep within picked session.
		searchArgs := []string{q, "--all", "-n", "20"}
		// Project-scope the search so we hit ONLY the picked session's project.
		searchArgs = append(searchArgs, "-p", pick.Project)
		return cmdSearch(searchArgs)
	}
	// No query: render the session like cchist show would. Flags MUST precede
	// the session-id positional or cmdShow treats them as the turn-index arg.
	if *allContent {
		*withThinking = true
		*withTools = true
	}
	showArgs := []string{}
	if *role != "both" {
		showArgs = append(showArgs, "--role", *role)
	}
	if *withThinking {
		showArgs = append(showArgs, "--with-thinking")
	}
	if *withTools {
		showArgs = append(showArgs, "--with-tools")
	}
	if *full {
		showArgs = append(showArgs, "--full")
	}
	if c.Limit > 0 {
		showArgs = append(showArgs, "--limit", fmt.Sprint(c.Limit))
		if *tail {
			showArgs = append(showArgs, "--tail")
		}
	}
	if c.Format != "" {
		showArgs = append(showArgs, "--format", c.Format)
	}
	if c.JSON {
		showArgs = append(showArgs, "--json")
	}
	showArgs = append(showArgs, pick.SessionID[:min(8, len(pick.SessionID))])
	return cmdShow(showArgs)
}

// pickOpts gates the candidate set for pickMostRecent.
type pickOpts struct {
	excludeCurrent bool
}

// pickMostRecent returns the most recently active session matching c's scope
// rules (cwd by default, --all overrides). Used by resume/prev to avoid each
// re-deriving the same selection logic.
func pickMostRecent(c *commonFlags, opts pickOpts) (*sessionSummary, error) {
	cache, _, err := refreshCache(cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
		Force:        c.Reindex,
		Verbose:      c.Verbose,
	})
	if err != nil {
		return nil, err
	}
	meta := loadMetadata()
	summaries := buildSessionSummaries(cache)
	cwdFilter := resolveCwdScope(c)
	hideCurrent := ""
	if opts.excludeCurrent {
		hideCurrent = currentSessionID()
	}
	var pick *sessionSummary
	for _, s := range summaries {
		if c.Project != "" && !strings.Contains(strings.ToLower(s.Project), strings.ToLower(c.Project)) {
			continue
		}
		if cwdFilter != "" && !cwdMatches(s.Project, cwdFilter) {
			continue
		}
		if meta.isDeprecated(s.SessionID) {
			continue
		}
		if hideCurrent != "" && s.SessionID == hideCurrent {
			continue
		}
		if pick == nil || s.LastTS > pick.LastTS {
			pick = s
		}
	}
	if pick == nil {
		where := "cwd"
		if cwdFilter == "" {
			where = "any project"
		}
		return nil, fmt.Errorf("no matching session in %s — try --all", where)
	}
	return pick, nil
}

// resumeCommand returns the CLI one-liner that reopens the given session for
// the named source. Empty string means "no known resume command" — we still
// print the session but skip the paste-ready line.
func resumeCommand(source, sessionID string) string {
	switch source {
	case "claude", "":
		return "claude --resume " + sessionID
	case "codex":
		return "codex resume " + sessionID
	}
	return ""
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
