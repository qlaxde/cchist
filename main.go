package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Global options shared across subcommands. Kept as a package-level struct so
// each subcommand's flag set can bind into the same values without plumbing.
type commonFlags struct {
	Limit             int
	Project           string
	All               bool // override the default cwd-scoped lookup
	Since             string
	JSON              bool   // deprecated alias for --format json
	Format            string // "" | "text" | "json" | "toon"
	Fields            string // comma-separated field names, or "all"; structured outputs only
	Quiet             bool
	Verbose           bool
	Reindex           bool
	IncludeDeprecated bool
	IncludeCompleted  bool
	ShowForks         bool
}

// currentSessionID returns the Claude Code session id of the conversation
// invoking cchist, if any. Claude Code exports this on every shell command it
// runs, so we use it to hide the in-progress conversation from search / list /
// threads results — agents looking up history almost never want their own
// turns echoed back.
func currentSessionID() string {
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

func bindCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.IntVar(&c.Limit, "n", 10, "max results")
	fs.IntVar(&c.Limit, "limit", 10, "max results")
	fs.StringVar(&c.Project, "p", "", "filter by project substring (matches cwd)")
	fs.StringVar(&c.Project, "project", "", "filter by project substring (matches cwd)")
	fs.BoolVar(&c.All, "all", false, "search across all projects (default: current dir only)")
	fs.BoolVar(&c.All, "a", false, "search across all projects (shorthand for --all)")
	fs.BoolVar(&c.All, "global", false, "search across all projects (alias for --all)")
	fs.StringVar(&c.Since, "since", "", "recency filter (ISO date or e.g. 7d, 12h, 2w)")
	fs.BoolVar(&c.JSON, "json", false, "alias for --format json")
	fs.StringVar(&c.Format, "format", "", "output format: text|json|toon (default toon)")
	fs.StringVar(&c.Fields, "fields", "", "fields to emit (or 'all'); structured outputs only")
	fs.BoolVar(&c.Quiet, "quiet", false, "suppress stderr hints (still prints results)")
	fs.BoolVar(&c.Quiet, "q", false, "shorthand for --quiet")
	fs.BoolVar(&c.Verbose, "v", false, "log indexing progress to stderr")
	fs.BoolVar(&c.Verbose, "verbose", false, "log indexing progress to stderr")
	fs.BoolVar(&c.Reindex, "reindex", false, "force full reindex before running")
	fs.BoolVar(&c.IncludeDeprecated, "include-deprecated", false, "include sessions marked deprecated")
	fs.BoolVar(&c.IncludeCompleted, "include-completed", true, "include completed sessions (default true)")
	fs.BoolVar(&c.ShowForks, "show-forks", false, "don't dedup fork siblings (show every match)")
}

// resolveFormat returns the canonical output format and validates it. The
// legacy --json flag maps to "json" so old agent invocations keep working.
//
// Default is "toon" — cchist is consumed by AI agents, so the token-efficient
// format wins by default. Humans running the tool interactively can pass
// --format text to get the colorised pretty-print.
func (c *commonFlags) resolveFormat() (string, error) {
	f := strings.ToLower(strings.TrimSpace(c.Format))
	if f == "" {
		if c.JSON {
			return "json", nil
		}
		return "toon", nil
	}
	switch f {
	case "text", "json", "toon":
		return f, nil
	}
	return "", fmt.Errorf("--format must be one of: text, json, toon (got %q)", c.Format)
}

// resolveCwdScope returns the directory to filter on, or "" when the caller
// should not apply any cwd restriction. The rules are:
//   - --all or -a → no cwd filter (empty string)
//   - --project <substr> → no cwd filter (substring is the filter)
//   - otherwise → current working directory
//
// Centralising this keeps the three subcommands (search, list, threads)
// behaving identically without each re-deriving the rule.
func resolveCwdScope(c *commonFlags) string {
	if c.All || c.Project != "" {
		return ""
	}
	cwd, _ := os.Getwd()
	return cwd
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cchist:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	// If the first arg isn't a known subcommand, treat the whole thing as a
	// search query. Makes `cchist foo bar baz` work without ceremony.
	known := map[string]bool{
		"search": true, "list": true, "show": true, "reindex": true,
		"hook": true, "archive": true,
		"deprecate": true, "undeprecate": true, "deprecated": true,
		"purge":   true,
		"threads": true, "forks": true,
		"resume": true, "prev": true,
		"-h": true, "--help": true, "help": true,
	}
	if len(argv) == 0 {
		return usage(os.Stdout)
	}
	if !known[argv[0]] {
		// Bare scope flags (cchist --all / -a / --global) used to error out of
		// cmdSearch with "empty query". Agents kept tripping on this — the
		// intent is obviously "list everything across projects", so route to
		// list when no positional follows.
		if onlyScopeFlags(argv) {
			argv = append([]string{"list"}, argv...)
		} else {
			argv = append([]string{"search"}, argv...)
		}
	}

	cmd := argv[0]
	// `hook` is invoked by Claude Code with the payload on stdin — never
	// rewrite its argv, it takes no positional flags from us.
	rest := argv[1:]
	if cmd != "hook" {
		rest = hoistFlags(rest)
	}
	switch cmd {
	case "search":
		return cmdSearch(rest)
	case "list":
		return cmdList(rest)
	case "show":
		return cmdShow(rest)
	case "reindex":
		return cmdReindex(rest)
	case "hook":
		return cmdHook(rest)
	case "archive":
		return cmdArchive(rest)
	case "deprecate":
		return cmdDeprecate(rest)
	case "undeprecate":
		return cmdUndeprecate(rest)
	case "deprecated":
		return cmdDeprecated(rest)
	case "purge":
		return cmdPurge(rest)
	case "threads":
		return cmdThreads(rest)
	case "forks":
		return cmdForks(rest)
	case "resume":
		return cmdResume(rest)
	case "prev":
		return cmdPrev(rest)
	case "help", "-h", "--help":
		return usage(os.Stdout)
	}
	return fmt.Errorf("unknown command: %s", cmd)
}

func usage(w io.Writer) error {
	_, err := fmt.Fprint(w, `cchist — search agent transcripts (Claude Code, Codex). Tuned for AI agents.

Quick start (scope defaults to current directory; auto-widens to all if empty):
  cchist <query...>              BM25 search (free text + operators)
  cchist <query...> --top        search and inline the top hit's full transcript
  cchist prev [query...]         most-recent session in cwd (excl. live), grep optional
  cchist resume                  print the resume command for the newest open thread
  cchist show <id-prefix> [turn] print a session by id prefix
  cchist list                    sessions newest first
  cchist threads                 open threads (with resume commands), forks grouped

Query operators (mix with free text):
  kind:text|thinking|tool_use|tool_result
  role:user|assistant
  tool:<Name>                    only turns that called this tool

Common flags (work on search / list / threads / prev):
  -a, --all, --global            search every project (default: cwd only)
  -p, --project S                filter by project substring
  -n, --limit N                  max results (default 10)
  --since SPEC                   ISO date or 7d / 12h / 2w
  --format text|json|toon        default toon (text for humans)
  --fields F1,F2,…               structured outputs: pick fields, or 'all'
  -q, --quiet                    suppress stderr hints (search/list/threads)
  --reindex                      force full reindex before running

Show flags (cchist show ... | cchist prev | cchist <q> --top):
  --role user|assistant|both     limit to one side (default both)
  --with-thinking                include thinking blocks
  --with-tools                   include tool_use / tool_result blocks
  --all                          shorthand for --with-thinking --with-tools (show only)
  --blocks                       emit typed block array (each has idx)
  --block N                      single block by idx, untruncated (--full implicit)
  --full                         untruncated tool inputs/results
  -n, --limit N                  render at most N turns (replaces piping through head)
  --tail                         with --limit, take the last N turns instead of first

Less common:
  cchist forks [id-prefix]       list fork families
  cchist archive                 mirror every agent's live transcripts + plans
  cchist hook                    Claude Code hook entry point (stdin payload)
  cchist deprecate <id-prefix>   hide from search (keeps archive copy)
  cchist undeprecate <id-prefix>
  cchist deprecated              list deprecated ids
  cchist purge <id-prefix>       DELETE from archive (irreversible)
  cchist reindex                 force full rebuild of the cache
  --include-deprecated           include soft-hidden sessions
  --show-forks                   don't dedup fork siblings in search results
  --json                         alias for --format json
  -v, --verbose                  log indexing progress

Env:
  CLAUDE_HISTORY_DIR  defaults to ~/.claude/projects
  CODEX_HOME          defaults to ~/.codex
  CCHIST_CACHE        defaults to ~/.cache/cchist
  CCHIST_ARCHIVE      defaults to ~/.local/share/cchist
`)
	return err
}

// --- paths -----------------------------------------------------------------

func cacheDir() string {
	if v := os.Getenv("CCHIST_CACHE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "cchist")
}

func cachePath() string {
	return filepath.Join(cacheDir(), "corpus.gob")
}

func indexPath() string {
	return filepath.Join(cacheDir(), "index.gob")
}

// onlyScopeFlags returns true when argv consists solely of -a / --all /
// --global (bare scope toggles, no positional). Used to route `cchist --all`
// to `cchist list --all` instead of dying with "empty query".
func onlyScopeFlags(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for _, a := range argv {
		switch a {
		case "-a", "--all", "--global":
		default:
			return false
		}
	}
	return true
}

// hoistFlags rewrites argv so that any token starting with '-' is moved to
// the front (preserving relative order and value-pairs). Lets users type
// flags after positional args — e.g. `cchist "foo bar" -n 3` — which the
// stdlib flag package otherwise rejects.
func hoistFlags(argv []string) []string {
	flags := make([]string, 0, len(argv))
	positional := make([]string, 0, len(argv))
	boolFlags := map[string]bool{
		"--cwd": true, "--json": true, "-v": true, "--verbose": true,
		"--reindex":       true,
		"--with-thinking": true, "--with-tools": true,
		"--blocks":        true,
		"--full":          true, "--quiet": true, "-q": true,
		"-a": true, "--all": true, "--global": true,
		"--include-deprecated": true, "--show-forks": true,
	}
	i := 0
	for i < len(argv) {
		tok := argv[i]
		if strings.HasPrefix(tok, "-") && tok != "-" && tok != "--" {
			flags = append(flags, tok)
			// If it's a value-taking flag and the next token is a value (not
			// another flag), take that too.
			hasEquals := strings.Contains(tok, "=")
			if !hasEquals && !boolFlags[tok] && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				flags = append(flags, argv[i+1])
				i += 2
				continue
			}
			i++
			continue
		}
		positional = append(positional, tok)
		i++
	}
	return append(flags, positional...)
}

// --- search ----------------------------------------------------------------

func cmdSearch(argv []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var c commonFlags
	var context int
	var top bool
	bindCommon(fs, &c)
	fs.IntVar(&context, "context", 300, "snippet width in chars")
	fs.BoolVar(&top, "top", false, "inline the top hit's full transcript (one tool call instead of search+show)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	format, err := c.resolveFormat()
	if err != nil {
		return err
	}
	rawQuery := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if rawQuery == "" {
		// `cchist --all` (and similar bare-scope invocations) used to die with
		// "empty query". The intent is clearly "list everything in scope" —
		// agents kept tripping on the error, then re-reading --help. Route to
		// list which uses the same scope/since/format/fields flags.
		if c.All || c.Project != "" || c.Since != "" || c.Format != "" || c.Fields != "" || c.JSON {
			return cmdList(argv)
		}
		return fmt.Errorf("empty query")
	}
	// Pull kind:/role:/tool: operators out before we tokenise. Free text is
	// what BM25 ranks; the operators gate the result set after ranking.
	qf := parseQueryOperators(rawQuery)

	cache, changed, err := refreshCache(cachePath(), refreshOptions{
		Force:        c.Reindex,
		RescanWindow: defaultRescanWindow,
		Verbose:      c.Verbose,
	})
	if err != nil {
		return err
	}
	turns := cache.allTurns()
	if len(turns) == 0 {
		return fmt.Errorf("no transcripts indexed — no installed agents produced any sessions yet")
	}

	rankText := qf.FreeText
	if rankText == "" {
		// Pure structural query (e.g. `tool:Bash`). Rank by recency rather than
		// relevance — there's no text to score against.
		rankText = ""
	}

	var hits []scoredDoc
	if rankText != "" {
		idx := loadOrBuildIndex(turns, indexPath(), changed || c.Reindex)
		qtok := tokenize(rankText)
		if len(qtok) == 0 {
			return fmt.Errorf("query contained only stopwords")
		}
		// Over-fetch so post-filters don't starve the result set.
		k := c.Limit * 10
		if k < 50 {
			k = 50
		}
		if k > len(turns) {
			k = len(turns)
		}
		hits = idx.search(qtok, k)
	} else if qf.hasFilter() {
		// Recency-ranked walk over all turns; applyTurnFilters does the gating.
		hits = make([]scoredDoc, len(turns))
		for i := range turns {
			hits[i] = scoredDoc{DocID: uint32(i), Score: 0}
		}
	} else {
		return fmt.Errorf("query had no searchable text and no operators")
	}

	since, err := parseSince(c.Since)
	if err != nil {
		return err
	}
	cwdFilter := resolveCwdScope(&c)

	qterms := splitWords(rankText)
	meta := loadMetadata()
	rootByID := collectRootUUIDs(cache)
	hideCurrent := currentSessionID()

	gather := func(scope string) []scoredTurn {
		keepFamily := familyDedupFilter(rootByID)
		out := make([]scoredTurn, 0, c.Limit)
		for _, h := range hits {
			t := turns[h.DocID]
			if !matchFilters(t, c.Project, scope, since) {
				continue
			}
			if !c.IncludeDeprecated && meta.isDeprecated(t.SessionID) {
				continue
			}
			if hideCurrent != "" && t.SessionID == hideCurrent {
				continue
			}
			if !c.ShowForks && !keepFamily(t.SessionID) {
				continue
			}
			if !turnMatchesQuery(t, qf) {
				continue
			}
			out = append(out, scoredTurn{Score: h.Score, Turn: t})
			if len(out) >= c.Limit {
				break
			}
		}
		return out
	}

	results := gather(cwdFilter)
	widened := false
	if len(results) == 0 && cwdFilter != "" && c.Project == "" {
		// Cwd-scope came up empty. Agents almost always want this re-run with
		// the wider scope rather than giving up — the historical pattern is
		// "cchist foo → 0 hits → retry with -a". Do it for them.
		results = gather("")
		widened = len(results) > 0
	}

	if top && len(results) > 0 {
		if widened && !c.Quiet {
			fmt.Fprintln(os.Stderr, color("(--top: widened scope to --all; cwd had 0 hits)", colorDim))
		}
		return emitTopHit(cache, results[0].Turn.SessionID, format)
	}

	if format == "json" || format == "toon" {
		if widened && !c.Quiet {
			fmt.Fprintln(os.Stderr, color("(widened scope to --all; cwd had 0 hits)", colorDim))
		}
		fields, err := resolveFields(c.Fields, searchDefaultFields, searchAllFields)
		if err != nil {
			return err
		}
		if err := emitStructured(format, buildSearchPayload(results, qterms, context, fields)); err != nil {
			return err
		}
		if len(results) > 0 && !c.Quiet {
			fmt.Fprintf(os.Stderr, "# next: cchist show %s   (or pass --top to inline)\n",
				results[0].Turn.SessionID[:min(8, len(results[0].Turn.SessionID))])
		}
		return nil
	}
	if len(results) == 0 {
		if !c.Quiet {
			fmt.Fprintln(os.Stderr, emptyHint(cwdFilter, "matches"))
		}
		return nil
	}
	if widened && !c.Quiet {
		fmt.Fprintln(os.Stderr, color("(widened scope to --all; cwd had 0 hits)", colorDim))
	}
	for _, r := range results {
		printResult(r, qterms, context)
	}
	if !c.Quiet {
		fmt.Fprintf(os.Stderr, "# next: cchist show %s   (or pass --top to inline)\n",
			results[0].Turn.SessionID[:min(8, len(results[0].Turn.SessionID))])
	}
	return nil
}

// emitTopHit renders one session's full transcript in the given format. Used
// by `cchist <query> --top` to collapse search-then-show into a single call.
func emitTopHit(cache *Cache, sessionID, format string) error {
	var file string
	for fpath, turns := range cache.TurnsByFile {
		for _, t := range turns {
			if t.SessionID == sessionID {
				file = fpath
				break
			}
		}
		if file != "" {
			break
		}
	}
	if file == "" {
		return fmt.Errorf("top hit %s not found in cache", sessionID)
	}
	turns := cache.TurnsByFile[file]
	allowedKinds := map[string]bool{BlockText: true}
	if format == "json" || format == "toon" {
		return emitStructured(format, buildShowPayload(turns, true, true, true, allowedKinds, -1))
	}
	for _, t := range turns {
		fmt.Println(color(fmt.Sprintf("── #%d  %s  session %s ──", t.TurnIdx, shortTS(t.Timestamp), t.SessionID), colorCyan))
		fmt.Println(color("user:", colorBold))
		if t.UserText == "" {
			fmt.Println("(empty)")
		} else {
			fmt.Println(t.UserText)
		}
		if asst := renderBlocks(t.Blocks, allowedKinds); asst != "" {
			fmt.Println(color("assistant:", colorBold))
			fmt.Println(asst)
		}
		fmt.Println()
	}
	return nil
}

// emptyHint explains "why was this empty" when the default cwd-scoped view
// returns no rows. If the user ran without --all, there may actually be hits
// elsewhere — telling them so prevents the "wait, that can't be right"
// confusion of the new default.
func emptyHint(cwdFilter, noun string) string {
	if cwdFilter == "" {
		return "no " + noun
	}
	return fmt.Sprintf("no %s in %s — try --all to search everywhere", noun, shortProject(cwdFilter))
}

type scoredTurn struct {
	Score float64
	Turn  Turn
}

// loadOrBuildIndex reuses a persisted BM25 if the corpus hasn't changed and
// the saved shape still matches the live corpus. Doc count is a cheap
// canary — if it differs, the on-disk index is stale and we rebuild.
func loadOrBuildIndex(turns []Turn, path string, stale bool) *BM25 {
	if !stale {
		if idx := loadIndex(path); idx != nil && len(idx.docLens) == len(turns) {
			return idx
		}
	}
	docs := make([][]string, len(turns))
	for i, t := range turns {
		docs[i] = tokenize(t.Text)
	}
	idx := buildBM25(docs)
	// Ignore save errors: worst case we rebuild next time.
	_ = saveIndex(path, idx)
	return idx
}

// --- list ------------------------------------------------------------------

// sessionRow is the per-session aggregate emitted by cmdList. Hoisted to file
// scope so the field-selection helpers (sessionRowToMap, listDefaultFields,
// listAllFields) can refer to it.
type sessionRow struct {
	SessionID string
	Source    string
	Project   string
	Slug      string
	FirstTS   string
	LastTS    string
	Turns     int
	FirstUser string
	File      string
}

// listAllFields and listDefaultFields are the ordered lists of selectable
// fields for `cchist list`. The default omits low-value fields agents rarely
// need — slug is often empty, file is just the on-disk JSONL path duplicated
// from session_id, first_ts is dwarfed by last_ts in usefulness.
var (
	listAllFields     = []string{"session_id", "source", "project", "slug", "first_ts", "last_ts", "turns", "first_user", "file"}
	listDefaultFields = []string{"session_id", "source", "project", "last_ts", "turns", "first_user"}
)

func sessionRowToMap(r *sessionRow, fields []string) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		switch f {
		case "session_id":
			m["session_id"] = r.SessionID
		case "source":
			m["source"] = r.Source
		case "project":
			m["project"] = r.Project
		case "slug":
			m["slug"] = r.Slug
		case "first_ts":
			m["first_ts"] = r.FirstTS
		case "last_ts":
			m["last_ts"] = r.LastTS
		case "turns":
			m["turns"] = r.Turns
		case "first_user":
			m["first_user"] = r.FirstUser
		case "file":
			m["file"] = r.File
		}
	}
	return m
}

func cmdList(argv []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var c commonFlags
	bindCommon(fs, &c)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	format, err := c.resolveFormat()
	if err != nil {
		return err
	}

	cache, _, err := refreshCache(cachePath(), refreshOptions{
		Force:        c.Reindex,
		RescanWindow: defaultRescanWindow,
		Verbose:      c.Verbose,
	})
	if err != nil {
		return err
	}

	byID := make(map[string]*sessionRow)
	for _, turns := range cache.TurnsByFile {
		for _, t := range turns {
			if t.SessionID == "" {
				continue
			}
			row, ok := byID[t.SessionID]
			if !ok {
				row = &sessionRow{
					SessionID: t.SessionID,
					Source:    t.Source,
					Project:   t.Project,
					Slug:      t.Slug,
					FirstTS:   t.Timestamp,
					LastTS:    t.Timestamp,
					FirstUser: t.UserText,
					File:      t.File,
				}
				byID[t.SessionID] = row
			}
			row.Turns++
			if t.Timestamp > row.LastTS {
				row.LastTS = t.Timestamp
			}
			if row.FirstTS == "" || (t.Timestamp != "" && t.Timestamp < row.FirstTS) {
				row.FirstTS = t.Timestamp
				row.FirstUser = t.UserText
			}
		}
	}

	since, err := parseSince(c.Since)
	if err != nil {
		return err
	}
	cwdFilter := resolveCwdScope(&c)

	meta := loadMetadata()
	hideCurrent := currentSessionID()
	rows := make([]*sessionRow, 0, len(byID))
	for _, r := range byID {
		if c.Project != "" && !strings.Contains(strings.ToLower(r.Project), strings.ToLower(c.Project)) {
			continue
		}
		if cwdFilter != "" && !cwdMatches(r.Project, cwdFilter) {
			continue
		}
		if !since.IsZero() {
			dt, ok := parseTS(r.LastTS)
			if !ok || dt.Before(since) {
				continue
			}
		}
		if !c.IncludeDeprecated && meta.isDeprecated(r.SessionID) {
			continue
		}
		if hideCurrent != "" && r.SessionID == hideCurrent {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastTS > rows[j].LastTS })
	if len(rows) > c.Limit {
		rows = rows[:c.Limit]
	}

	if format == "json" || format == "toon" {
		fields, err := resolveFields(c.Fields, listDefaultFields, listAllFields)
		if err != nil {
			return err
		}
		out := make([]map[string]any, len(rows))
		for i, r := range rows {
			out[i] = sessionRowToMap(r, fields)
		}
		return emitStructured(format, out)
	}
	if len(rows) == 0 {
		if !c.Quiet {
			fmt.Fprintln(os.Stderr, emptyHint(cwdFilter, "sessions"))
		}
		return nil
	}
	for _, r := range rows {
		header := strings.Join(filterEmpty([]string{
			color(sourceBadge(r.Source), colorDim),
			color(r.SessionID[:min(8, len(r.SessionID))], colorCyan),
			shortTS(r.LastTS),
			color(fmt.Sprintf("%4dt", r.Turns), colorDim),
			color(shortProject(r.Project), colorGreen),
			color(r.Slug, colorDim),
		}), "  ")
		fmt.Println(header)
		if r.FirstUser != "" {
			preview := collapseSpaces(r.FirstUser)
			if len(preview) > 100 {
				preview = preview[:100] + "…"
			}
			fmt.Printf("  %s\n", preview)
		}
		fmt.Println()
	}
	return nil
}

// --- show ------------------------------------------------------------------

func cmdShow(argv []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log indexing progress")
	role := fs.String("role", "both", "user | assistant | both")
	// Content-inclusion is opt-in: default is chat only (kind=text) joined
	// into assistant_text. Agents opt into thinking/tools/blocks as needed.
	withThinking := fs.Bool("with-thinking", false, "include assistant thinking blocks")
	withTools := fs.Bool("with-tools", false, "include tool_use and tool_result blocks")
	allContent := fs.Bool("all", false, "shorthand for --with-thinking --with-tools")
	asBlocks := fs.Bool("blocks", false, "emit typed block array instead of joined assistant_text")
	blockIdx := fs.Int("block", -1, "single block by idx, untruncated (--full implicit)")
	full := fs.Bool("full", false, "untruncated tool inputs/results")
	limit := fs.Int("limit", 0, "render at most N turns (0 = all). Pair with --tail to take the last N.")
	fs.IntVar(limit, "n", 0, "shorthand for --limit")
	tail := fs.Bool("tail", false, "with --limit N, render the LAST N turns (default: first N)")
	asJSON := fs.Bool("json", false, "alias for --format json")
	formatFlag := fs.String("format", "", "output format: text|json|toon (default toon)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	format := strings.ToLower(strings.TrimSpace(*formatFlag))
	if format == "" && *asJSON {
		format = "json"
	}
	if format == "" {
		format = "toon"
	}
	if format != "text" && format != "json" && format != "toon" {
		return fmt.Errorf("--format must be one of: text, json, toon (got %q)", *formatFlag)
	}
	if *allContent {
		*withThinking = true
		*withTools = true
	}
	if *role != "both" && *role != "user" && *role != "assistant" {
		return fmt.Errorf("--role must be one of: user, assistant, both")
	}
	showUser := *role == "both" || *role == "user"
	showAsst := *role == "both" || *role == "assistant"
	allowedKinds := map[string]bool{
		BlockText:       true,
		BlockThinking:   *withThinking,
		BlockToolUse:    *withTools,
		BlockToolResult: *withTools,
	}
	// --block N implies typed shape, all kinds allowed, and --full: by the
	// time you address a specific block by index you've seen its clipped
	// form already, so single it out to recover the untruncated content.
	joinAsst := !*asBlocks && *blockIdx < 0
	if *blockIdx >= 0 {
		allowedKinds = map[string]bool{
			BlockText: true, BlockThinking: true,
			BlockToolUse: true, BlockToolResult: true,
		}
		*full = true
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("show requires a session id (prefix match)")
	}
	needle := strings.ToLower(fs.Arg(0))
	var turnFilter = -1
	if fs.NArg() >= 2 {
		n, err := strconv.Atoi(fs.Arg(1))
		if err != nil {
			return fmt.Errorf("invalid turn index %q", fs.Arg(1))
		}
		turnFilter = n
	}

	cache, _, err := refreshCache(cachePath(), refreshOptions{
		RescanWindow: defaultRescanWindow,
		Verbose:      *verbose,
	})
	if err != nil {
		return err
	}

	// Subagents inherit the parent's sessionId, so a prefix match can resolve
	// to either. Sort and prefer non-/subagents/ paths for determinism.
	var (
		mainMatch     string
		subagentMatch string
	)
	paths := make([]string, 0, len(cache.TurnsByFile))
	for p := range cache.TurnsByFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, fpath := range paths {
		hit := false
		for _, t := range cache.TurnsByFile[fpath] {
			if strings.HasPrefix(strings.ToLower(t.SessionID), needle) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if strings.Contains(fpath, "/subagents/") {
			if subagentMatch == "" {
				subagentMatch = fpath
			}
			continue
		}
		mainMatch = fpath
		break
	}
	matchFile := mainMatch
	if matchFile == "" {
		matchFile = subagentMatch
	}
	if matchFile == "" {
		return fmt.Errorf("no session matches %q", fs.Arg(0))
	}

	turns := cache.TurnsByFile[matchFile]
	// --full re-parses with truncation lifted; cache holds the clipped form.
	if *full {
		fresh, err := reparseFull(matchFile)
		if err != nil {
			return fmt.Errorf("--full re-parse failed: %w", err)
		}
		turns = fresh
	}
	if turnFilter >= 0 {
		filtered := turns[:0]
		for _, t := range turns {
			if t.TurnIdx == turnFilter {
				filtered = append(filtered, t)
			}
		}
		turns = filtered
		if len(turns) == 0 {
			return fmt.Errorf("no turn %d in session", turnFilter)
		}
	}
	truncated := 0
	if *limit > 0 && *limit < len(turns) {
		truncated = len(turns) - *limit
		if *tail {
			turns = turns[len(turns)-*limit:]
		} else {
			turns = turns[:*limit]
		}
	}

	if format == "json" || format == "toon" {
		if err := emitStructured(format, buildShowPayload(turns, showUser, showAsst, joinAsst, allowedKinds, *blockIdx)); err != nil {
			return err
		}
		if truncated > 0 {
			fmt.Fprintf(os.Stderr, "# truncated to %d turns; %d more available — drop --limit or pass --tail\n", *limit, truncated)
		}
		return nil
	}
	for _, t := range turns {
		fmt.Println(color(fmt.Sprintf("── #%d  %s  session %s ──", t.TurnIdx, shortTS(t.Timestamp), t.SessionID), colorCyan))
		if showUser {
			fmt.Println(color("user:", colorBold))
			if t.UserText == "" {
				fmt.Println("(empty)")
			} else {
				fmt.Println(t.UserText)
			}
		}
		if showAsst {
			if asst := renderBlocks(t.Blocks, allowedKinds); asst != "" {
				fmt.Println(color("assistant:", colorBold))
				fmt.Println(asst)
			}
		}
		fmt.Println()
	}
	if truncated > 0 {
		fmt.Fprintf(os.Stderr, "# truncated to %d turns; %d more available — drop --limit or pass --tail\n", *limit, truncated)
	}
	return nil
}

// buildShowPayload produces the structured wire shape for `show --json|--toon`.
// Envelope holds session-wide constants once; turns are uniform map rows so
// TOON renders one tabular header for all of them.
func buildShowPayload(turns []Turn, showUser, showAsst, joinAsst bool, allowed map[string]bool, blockIdx int) any {
	if len(turns) == 0 {
		return map[string]any{"turns": []map[string]any{}}
	}
	head := turns[0]
	rows := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		row := map[string]any{
			"turn":      t.TurnIdx,
			"timestamp": t.Timestamp,
		}
		// User text is irrelevant when addressing a single assistant block.
		if showUser && blockIdx < 0 {
			row["user_text"] = t.UserText
		}
		if showAsst {
			if blockIdx >= 0 {
				if blockIdx >= len(t.Blocks) {
					// Skip turns that don't have that index — caller may have
					// filtered by --turn already, in which case we want a
					// clear empty result rather than guessing.
					continue
				}
				b := t.Blocks[blockIdx]
				row["block"] = map[string]any{
					"idx":  blockIdx,
					"kind": b.Kind,
					"name": b.Name,
					"text": b.Text,
				}
			} else if joinAsst {
				row["assistant_text"] = renderBlocks(t.Blocks, allowed)
			} else {
				// Indexed blocks so the agent can address one with --block N.
				blocks := make([]map[string]any, 0, len(t.Blocks))
				for i, b := range t.Blocks {
					if !allowed[b.Kind] {
						continue
					}
					blocks = append(blocks, map[string]any{
						"idx":  i,
						"kind": b.Kind,
						"name": b.Name,
						"text": b.Text,
					})
				}
				row["blocks"] = blocks
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"session_id": head.SessionID,
		"source":     head.Source,
		"project":    head.Project,
		"turns":      rows,
	}
}

// reparseFull re-runs the source parser with truncation disabled so `show
// --full` can return real tool inputs and tool results. The cache always holds
// the truncated form for compactness; `--full` is the escape hatch.
func reparseFull(path string) ([]Turn, error) {
	for _, s := range sources {
		if s.Match(path) {
			prevIn, prevRes := parseToolInputCap, parseToolResultCap
			parseToolInputCap = 0
			parseToolResultCap = 0
			defer func() {
				parseToolInputCap = prevIn
				parseToolResultCap = prevRes
			}()
			return s.Parse(path)
		}
	}
	return nil, fmt.Errorf("no source claims %s", path)
}

// renderBlocks joins block renders, dropping any whose Kind is disallowed.
func renderBlocks(blocks []Block, allowed map[string]bool) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if !allowed[b.Kind] {
			continue
		}
		if r := b.Render(); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n")
}

// --- reindex ---------------------------------------------------------------

func cmdReindex(argv []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	t0 := time.Now()
	cache, _, err := refreshCache(cachePath(), refreshOptions{
		Force:   true,
		Verbose: true,
	})
	if err != nil {
		return err
	}
	turns := 0
	for _, ts := range cache.TurnsByFile {
		turns += len(ts)
	}
	fmt.Fprintf(os.Stderr, "indexed %d turns across %d files in %.2fs\n",
		turns, len(cache.TurnsByFile), time.Since(t0).Seconds())
	return nil
}

// --- filtering -------------------------------------------------------------

var sinceRE = regexp.MustCompile(`^(\d+)\s*([hdwm])$`)

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if m := sinceRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "h":
			return time.Now().Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
		case "w":
			return time.Now().Add(-time.Duration(n) * 7 * 24 * time.Hour), nil
		case "m":
			return time.Now().Add(-time.Duration(n) * 30 * 24 * time.Hour), nil
		}
	}
	dt, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return dt, nil
	}
	dt, err = time.Parse("2006-01-02", s)
	if err == nil {
		return dt, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since: %s", s)
}

func parseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if dt, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return dt, true
	}
	if dt, err := time.Parse(time.RFC3339, s); err == nil {
		return dt, true
	}
	return time.Time{}, false
}

func matchFilters(t Turn, project, cwd string, since time.Time) bool {
	if cwd != "" && !cwdMatches(t.Project, cwd) {
		return false
	}
	if project != "" && !strings.Contains(strings.ToLower(t.Project), strings.ToLower(project)) {
		return false
	}
	if !since.IsZero() {
		dt, ok := parseTS(t.Timestamp)
		if !ok || dt.Before(since) {
			return false
		}
	}
	return true
}

// cwdMatches implements project-aware cwd scoping. A session belongs to the
// "current project" when either:
//
//   - the user's cwd sits inside the session's recorded project root (I'm in
//     /repo/apps/admin; the session ran at /repo — include it), OR
//   - the session's recorded project sits inside the user's cwd (I'm at /repo
//     and the session was deep inside it — include it too).
//
// We compare with trailing slashes appended so /foo doesn't leak into /foobar.
// The git-root / worktree resolution that the history-viewer does at decode
// time isn't needed here because Claude Code's transcripts already record the
// real cwd, so this pure prefix check covers the cases that matter.
func cwdMatches(sessionProject, cwd string) bool {
	if sessionProject == "" || cwd == "" {
		return false
	}
	if sessionProject == cwd {
		return true
	}
	sp := strings.TrimSuffix(sessionProject, "/") + "/"
	cp := strings.TrimSuffix(cwd, "/") + "/"
	return strings.HasPrefix(cp, sp) || strings.HasPrefix(sp, cp)
}

// --- rendering -------------------------------------------------------------

const (
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorCyan   = "\033[36m"
	colorYellow = "\033[33m"
	colorGreen  = "\033[32m"
	colorReset  = "\033[0m"
)

var useColor = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}()

func color(s, code string) string {
	if !useColor || s == "" {
		return s
	}
	return code + s + colorReset
}

func shortTS(s string) string {
	dt, ok := parseTS(s)
	if !ok {
		if len(s) >= 16 {
			return s[:16]
		}
		return s
	}
	return dt.Local().Format("2006-01-02 15:04")
}

func shortProject(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

var spaceRE = regexp.MustCompile(`\s+`)

func collapseSpaces(s string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

// snippet returns up to `width` characters of text centred on the first query
// term that matches. Falls back to a plain prefix if no term is found.
func snippet(text string, terms []string, width int) string {
	if text == "" {
		return ""
	}
	collapsed := collapseSpaces(text)
	lower := strings.ToLower(collapsed)
	idx := -1
	for _, term := range terms {
		i := strings.Index(lower, strings.ToLower(term))
		if i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		if len(collapsed) <= width {
			return collapsed
		}
		return collapsed[:width] + "…"
	}
	start := idx - width/4
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(collapsed) {
		end = len(collapsed)
	}
	var prefix, suffix string
	if start > 0 {
		prefix = "…"
	}
	if end < len(collapsed) {
		suffix = "…"
	}
	return prefix + collapsed[start:end] + suffix
}

func printResult(r scoredTurn, terms []string, width int) {
	header := strings.Join(filterEmpty([]string{
		color(fmt.Sprintf("%6.2f", r.Score), colorYellow),
		color(sourceBadge(r.Turn.Source), colorDim),
		color(r.Turn.SessionID[:min(8, len(r.Turn.SessionID))], colorCyan),
		color(fmt.Sprintf("#%d", r.Turn.TurnIdx), colorDim),
		shortTS(r.Turn.Timestamp),
		color(shortProject(r.Turn.Project), colorGreen),
		color(r.Turn.Slug, colorDim),
	}), "  ")
	fmt.Println(header)
	fmt.Printf("  %s\n\n", snippet(r.Turn.Text, terms, width))
}

// sourceBadge formats a Source.ID() for inclusion in a result header. The
// empty badge is allowed so legacy cached turns (Source="") don't print a
// stray "[]".
func sourceBadge(id string) string {
	if id == "" {
		return ""
	}
	return "[" + id + "]"
}

// searchAllFields and searchDefaultFields drive --fields filtering for cmdSearch.
// Default drops slug (often empty) and file (the JSONL path; session_id is the
// canonical identifier and `show --full` will re-derive the path on demand).
var (
	searchAllFields     = []string{"score", "source", "session_id", "turn", "timestamp", "project", "slug", "snippet", "file"}
	searchDefaultFields = []string{"score", "source", "session_id", "turn", "timestamp", "project", "snippet"}
)

func buildSearchPayload(results []scoredTurn, terms []string, width int, fields []string) []map[string]any {
	out := make([]map[string]any, len(results))
	for i, r := range results {
		m := make(map[string]any, len(fields))
		for _, f := range fields {
			switch f {
			case "score":
				m["score"] = round4(r.Score)
			case "source":
				m["source"] = r.Turn.Source
			case "session_id":
				m["session_id"] = r.Turn.SessionID
			case "turn":
				m["turn"] = r.Turn.TurnIdx
			case "timestamp":
				m["timestamp"] = r.Turn.Timestamp
			case "project":
				m["project"] = r.Turn.Project
			case "slug":
				m["slug"] = r.Turn.Slug
			case "snippet":
				m["snippet"] = snippet(r.Turn.Text, terms, width)
			case "file":
				m["file"] = r.Turn.File
			}
		}
		out[i] = m
	}
	return out
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

func filterEmpty(xs []string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func splitWords(s string) []string {
	fields := strings.Fields(s)
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
