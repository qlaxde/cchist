# cchist

Fast CLI for searching, preserving, and managing agent transcripts across every agent you run — today [Claude Code](https://claude.com/claude-code) and [Codex CLI](https://github.com/openai/codex). Indexes `~/.claude/projects/**/*.jsonl` plus `~/.codex/sessions/**/rollout-*.jsonl` with BM25, mirrors conversations + plans to a durable archive so they survive compaction and 30-day cleanups, and surfaces "loose threads" so you can close REPLs without losing track of work. Every result is tagged with its source (`[claude]` / `[codex]`).

**Built for AI agents.** The default output is [TOON](https://toonformat.dev/) (Token-Oriented Object Notation) and `show` returns chat only by default — typically <2% the size of `claude --resume`'s context load while preserving enough signal to pick up old work. Pass `--format text` for the colorised human pretty-print.

## Let an agent install it

Paste the prompt below into a fresh Claude Code session (or any agent CLI that can read/write your filesystem and edit JSON) to install cchist end-to-end — binary, archive seed, and lifecycle hooks — without running the steps by hand:

```
Install cchist from https://github.com/qlaxde/cchist end-to-end. Do every step; stop and ask only if something fails.

1. Detect my OS and CPU arch with `uname -s` and `uname -m`. Map to one of: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Refuse to proceed on any other platform.

2. Download the matching binary from
   https://github.com/qlaxde/cchist/releases/latest/download/cchist-<os>-<arch>
   Install it at ~/.local/bin/cchist (mkdir -p the directory), chmod +x. If ~/.local/bin is not on PATH, tell me the exact shell-rc line to add.

3. Verify with `cchist help` — it should print the usage banner.

4. Run `cchist archive` once to snapshot my existing ~/.claude/projects transcripts and ~/.claude/plans into the durable archive. Report how many sessions and plans were archived.

5. Install two lifecycle hooks in ~/.claude/settings.json: PreCompact and SessionEnd. Each runs the command
      /Users/<me>/.local/bin/cchist hook 2>/dev/null || true
   with timeouts of 10s each (use the absolute path — hooks don't inherit my shell PATH).

   CRITICAL: READ the existing settings.json first and MERGE into the hooks object. Do NOT replace it. I probably have other hooks (formatters, MCP compressors, statusline, etc.) — preserve every one. Validate the final file with `jq -e .` before saving; abort if invalid.

6. Tell me the hooks won't take effect in my already-running Claude sessions — I need to open `/hooks` once (which reloads settings) or start a new session. Offer to also add a one-liner verifier hook command I can paste into `/hooks` to confirm `cchist hook` fires.

7. Report: binary path, binary size, archive totals, exactly which hook entries you added, and link me to the README for workflow commands: https://github.com/qlaxde/cchist/blob/main/README.md
```

The prompt is deliberately explicit — every step references a specific artefact, and the settings.json step spells out the merge/validate dance so the agent doesn't clobber existing hooks.

## Background

This project started from Eric Tramel's blog post **[Searchable Agent Memory](https://eric-tramel.github.io/blog/2026-02-07-searchable-agent-memory/)**, which sketches an MCP server that makes Claude Code transcripts searchable via BM25. cchist takes the same core indexing idea and reshapes it:

- **CLI, not MCP** — invoked from your shell, not another agent.
- **Go, not Python** — native startup, no interpreter overhead. Warm queries finish in ~250 ms on a 9 000-turn corpus.
- **Persistent archive** — hooks snapshot transcripts before compaction and on session end, so the full history survives events that ordinarily lose it.
- **Workflow features** — fork-family detection and loose-thread surfacing so you can close REPLs without losing track of unfinished work.

## Install

### From source

```bash
go install github.com/qlaxde/cchist@latest
```

### Pre-built binaries

Grab the right binary for your platform from the [releases page](https://github.com/qlaxde/cchist/releases):

```bash
# Apple Silicon
curl -L https://github.com/qlaxde/cchist/releases/latest/download/cchist-darwin-arm64 -o cchist
chmod +x cchist && mv cchist ~/.local/bin/

# Intel Mac
curl -L https://github.com/qlaxde/cchist/releases/latest/download/cchist-darwin-amd64 -o cchist

# Linux x86_64
curl -L https://github.com/qlaxde/cchist/releases/latest/download/cchist-linux-amd64 -o cchist
```

## Usage

### Search

**Default scope is the current project, across every installed agent.** `search`, `list`, `threads`, and `prev` filter to the directory you're in (matched by prefix so a subdir like `apps/admin` still resolves to its repo root) and search Claude + Codex transcripts together. Use `--all` / `-a` / `--global` to broaden to every project. Each row is tagged `[claude]` or `[codex]` so you can see at a glance where a hit came from.

```bash
cchist "sip gemini realtime"      # default: current project, both agents
cchist -a "sip gemini realtime"   # every project, every agent (--global is an alias)
cchist "sip gemini realtime" --top # search + inline the top hit's transcript in one call
cchist -p marketplace "…"          # filter by substring of the cwd path
cchist --since 7d "migration"      # recent hits only
cchist --show-forks "…"            # don't dedup fork siblings (see below)
cchist --all                       # bare --all with no query lists every session
```

When a cwd-scoped search returns zero hits, cchist automatically retries with `--all` and prints a `(widened scope to --all; cwd had 0 hits)` note on stderr. Non-empty result sets emit a `# next: cchist show <prefix>` hint pointing at the natural follow-up call. Pass `-q` / `--quiet` to suppress both.

#### Query operators

Mix structural operators with free-text terms. Operators gate the result set after BM25 ranking; pure-operator queries walk the corpus by recency.

```bash
cchist "useEffect" role:user            # term must appear in user prompts
cchist tool:Bash kind:tool_use git      # turns that ran git via Bash
cchist role:assistant kind:thinking deadlock   # in assistant thinking blocks
```

| operator | values |
| --- | --- |
| `kind:` | `text`, `thinking`, `tool_use`, `tool_result` |
| `role:` | `user`, `assistant` |
| `tool:` | tool name (e.g. `Bash`, `Read`, `Edit`) |

### Browse

```bash
cchist list                           # sessions in current project, newest first
cchist list -a                        # across all projects
cchist show <session-prefix>          # default: chat only, joined into assistant_text per turn
cchist show <session-prefix> 12       # just turn #12
cchist prev                           # most recent session in cwd (excludes the live one)
cchist prev "migration"               # search within that previous session
cchist resume                         # print `claude --resume <id>` for the newest open thread
```

`prev` without a query renders the last 10 turns of the most recent session (uses `--tail` by default; pass `--limit 0` for the whole transcript). With a query it routes to a scoped search inside that session. `resume` picks the most recent open thread in cwd, excluding the live session, and prints the resume command on stdout with metadata on stderr — one paste to jump back in.

#### Show flags

```bash
cchist show <id> --with-thinking      # include thinking blocks
cchist show <id> --with-tools         # include tool_use / tool_result
cchist show <id> --all                # both of the above
cchist show <id> --blocks             # typed block array (each carries idx)
cchist show <id> --block 3            # one block by idx, untruncated (--full implicit)
cchist show <id> --full               # untruncated tool inputs/results everywhere
cchist show <id> --role user          # one side only
cchist show <id> -n 20                # render at most 20 turns (replaces piping to head)
cchist show <id> -n 20 --tail         # the last 20 instead of the first
```

The cache stores tool inputs clipped to 80 chars and tool results clipped to 200 — small enough to stay light, lossless via `--full`. The single-block path (`--block N --full` is implicit) is the cheap recovery: ~1 KB for one full tool_result vs ~100 KB+ for re-parsing the whole session. When `--limit` clips turns, cchist prints a truncation hint on stderr so you know more is available.

### Output formats

```bash
cchist list --format toon             # default — agent-friendly, ~13–17% smaller than compact JSON
cchist list --format json             # standard JSON
cchist list --format text             # human pretty-print (was the old default)
cchist list --json                    # alias for --format json
```

Structured outputs (`json`, `toon`) on `list` and `search` trim low-signal fields (`slug`, `file`, `first_ts`) by default. Pass `--fields all` to restore everything, or `--fields a,b,c` to pick exactly. Unknown names error so typos don't silently drop data.

`show --format toon` hoists session-wide constants (`session_id`, `source`, `project`) to an envelope so they're emitted once, then turns follow as a uniform tabular array.

### Loose threads

Each Claude REPL you leave open leaks ~200 MB/hour. The reason you leave them open: they represent unfinished work you're afraid to lose. `cchist threads` surfaces them so you can close safely.

```bash
cchist threads                        # open threads in current project
cchist threads -a                     # across all projects
cchist threads --include-deprecated   # also show soft-hidden sessions
```

Each row prints its `claude --resume <id>` command so resuming is one paste. Fork siblings render under a single canonical row with `├─` / `└─` connectors so duplicate conversations collapse into a tree.

### Forks

When you fork a conversation in Claude Code, both siblings share their prefix but diverge afterwards. cchist groups them automatically:

```bash
cchist forks                          # list every fork family
cchist forks <id>                     # one family's members
```

In `threads` output, forks render as a tree with `├─` / `└─` connectors and `fork N/M` badges. In search, duplicates are deduped by default (override with `--show-forks`).

### Soft-hide / hard-delete

```bash
cchist deprecate <id>                 # hide from search, keep archive copy
cchist undeprecate <id>
cchist purge <id>                     # DELETE from archive (irreversible)
```

## Preservation via hooks (Claude Code)

Claude Code fires lifecycle events; cchist listens and snapshots the transcript before Claude can truncate it. Codex CLI has no equivalent hook surface today — its rollouts are durable on disk, so `cchist archive` (or any ordinary `cchist` invocation, which refreshes the index and mirrors live files) is sufficient.

Add these to `~/.claude/settings.json` to snapshot Claude transcripts on lifecycle events:

```json
{
  "hooks": {
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "cchist hook 2>/dev/null || true",
            "timeout": 10
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "cchist hook 2>/dev/null || true",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Claude Code reads stdin JSON payloads for each event; `cchist hook` dispatches based on `hook_event_name`:

- **`PreCompact`** — snapshots the transcript before Claude rewrites it. This is the one that fixes "I lost my conversation to compaction".
- **`SessionEnd`** — final snapshot on clean exit.

After editing `settings.json`, reload once via `/hooks` in an existing Claude session, or just start new sessions — they pick the hooks up automatically.

Seed the archive once with `cchist archive` so pre-existing transcripts get mirrored before the hooks do their work on future sessions.

## Data layout

```
~/.claude/projects/<proj-hash>/<session>.jsonl                  # Claude live (auto-deleted after 30 days)
~/.codex/sessions/YYYY/MM/DD/rollout-…-<session>.jsonl         # Codex live
~/.local/share/cchist/
├── conversations/
│   ├── claude/<proj-hash>/<session>.jsonl                      # Claude archive (cchist writes, kept forever)
│   └── codex/sessions/YYYY/MM/DD/rollout-…-<session>.jsonl     # Codex archive
├── plans/<slug>.md                                              # mirror of ~/.claude/plans — same 30-day risk
└── metadata.json                                                # deprecated flags + notes
~/.cache/cchist/
├── corpus.gob                                                   # parsed turns + typed Blocks + mtime map (schema v4)
└── index.gob                                                    # BM25 postings (rebuilt when corpus changes)
```

### Env overrides

| Variable             | Default                 |
| -------------------- | ----------------------- |
| `CLAUDE_HISTORY_DIR` | `~/.claude/projects`    |
| `CODEX_HOME`         | `~/.codex`              |
| `CCHIST_CACHE`       | `~/.cache/cchist`       |
| `CCHIST_ARCHIVE`     | `~/.local/share/cchist` |

## How it works

### Indexing

- JSONL parse groups messages into _turns_ (one user prompt + every following assistant / tool response until the next real user prompt). `tool_result` user messages are treated as continuations, not new turns.
- Each turn carries a `UserText` plus a typed `Blocks` slice — every block is one of `text`, `thinking`, `tool_use`, `tool_result`. Block-level structure is what lets `show --block N`, `kind:` query operators, and the chat-only default work without re-parsing the source.
- A search blob (user text + rendered blocks + tool names) feeds an inverted index with standard BM25 scoring (k1=1.5, b=0.75) held in memory and persisted as gob. Top-K via a size-bounded min-heap.

### Incremental refresh

Each invocation stats every JSONL file and re-parses only those whose mtime changed. A 30-second cooldown skips the stat walk entirely when back-to-back queries are within that window — the common case.

### Fork detection

Every turn carries a `RootUserUUID` — the uuid of its session's first user message. Claude Code's fork action copies that uuid verbatim into the new session, so two sessions sharing a `RootUserUUID` are provably fork siblings. Subagent JSONLs live under `<proj>/subagents/` and are excluded from fork-family resolution (they carry the parent's sessionId but have their own root uuid).

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Acknowledgements

- Eric Tramel's [Searchable Agent Memory](https://eric-tramel.github.io/blog/2026-02-07-searchable-agent-memory/) for the original BM25-over-transcripts idea.
- [jhlee0409/claude-code-history-viewer](https://github.com/jhlee0409/claude-code-history-viewer) for the Plans indexing, fork awareness, and running-process feature directions.
