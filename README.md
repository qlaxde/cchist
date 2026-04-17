# cchist

Fast CLI for searching, preserving, and managing [Claude Code](https://claude.com/claude-code) session transcripts. Indexes `~/.claude/projects/**/*.jsonl` with BM25, mirrors conversations + plans to a durable archive so they survive compaction and Claude's 30-day cleanup, and surfaces "loose threads" so you can close REPLs without losing track of work.

## Background

This project started from Eric Tramel's blog post **[Searchable Agent Memory](https://eric-tramel.github.io/blog/2026-02-07-searchable-agent-memory/)**, which sketches an MCP server that makes Claude Code transcripts searchable via BM25. cchist takes the same core indexing idea and reshapes it:

- **CLI, not MCP** — invoked from your shell, not another agent.
- **Go, not Python** — native startup, no interpreter overhead. Warm queries finish in ~250 ms on a 9 000-turn corpus.
- **Persistent archive** — hooks snapshot transcripts before compaction and on session end, so the full history survives events that ordinarily lose it.
- **Workflow features** — fork-family detection, completion status, running-process reaping. Designed to fix the "16 GB of dirty Claude processes I'm afraid to close" problem.

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

```bash
cchist "sip gemini realtime"        # default: BM25 search
cchist --cwd "type error"            # limit to current project
cchist --since 7d "migration"        # recent hits only
cchist --show-forks "…"              # don't dedup fork siblings (see below)
```

### Browse

```bash
cchist list                           # sessions newest first
cchist show <session-prefix>          # print a full session
cchist show <session-prefix> 12       # print just turn #12
```

### Loose threads

Each Claude REPL you leave open leaks ~200 MB/hour. The reason you leave them open: they represent unfinished work you're afraid to lose. `cchist threads` surfaces them so you can close safely.

```bash
cchist threads                        # all open sessions, newest first
cchist threads --cwd                  # scoped to current project
cchist done                           # mark the most recent session complete
cchist done --family <id>             # also complete every fork of that session
```

Output shows `●` for sessions still in memory, `○` for dormant. Each row prints its `claude --resume <id>` command so resuming is one paste.

### Forks

When you fork a conversation in Claude Code, both siblings share their prefix but diverge afterwards. cchist groups them automatically:

```bash
cchist forks                          # list every fork family
cchist forks <id>                     # one family's members
```

In `threads` output, forks render as a tree with `├─` / `└─` connectors and `fork N/M` badges. In search, duplicates are deduped by default (override with `--show-forks`).

### Running processes

```bash
cchist running                        # list running claude processes with RSS + status
cchist reap                           # SIGTERM → 5s → SIGKILL every completed-and-still-running session
cchist reap --dry-run                 # preview without killing
```

### Soft-hide / hard-delete

```bash
cchist deprecate <id>                 # hide from search, keep archive copy
cchist undeprecate <id>
cchist purge <id>                     # DELETE from archive (irreversible)
```

## Preservation via hooks

Add these to `~/.claude/settings.json` to snapshot transcripts on lifecycle events:

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
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "cchist hook 2>/dev/null || true",
            "timeout": 5
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
- **`SessionStart`** — writes a PID → session marker so `cchist done` and `cchist reap` know which process is which.

After editing `settings.json`, reload once via `/hooks` in an existing Claude session, or just start new sessions — they pick the hooks up automatically.

Seed the archive once with `cchist archive` so pre-existing transcripts get mirrored before the hooks do their work on future sessions.

## Data layout

```
~/.claude/projects/<proj-hash>/<session>.jsonl    # live (Claude writes here, auto-deleted after 30 days)
~/.local/share/cchist/
├── conversations/<proj-hash>/<session>.jsonl     # archive (cchist writes here, kept forever)
├── plans/<slug>.md                                # mirror of ~/.claude/plans — same 30-day risk
├── metadata.json                                  # completion / deprecated flags
└── current/<pid>.json                             # SessionStart markers
~/.cache/cchist/
├── corpus.gob                                     # parsed turns + mtime map (schema v2)
└── index.gob                                      # BM25 postings (rebuilt when corpus changes)
```

### Env overrides

| Variable             | Default                 |
| -------------------- | ----------------------- |
| `CLAUDE_HISTORY_DIR` | `~/.claude/projects`    |
| `CCHIST_CACHE`       | `~/.cache/cchist`       |
| `CCHIST_ARCHIVE`     | `~/.local/share/cchist` |

## How it works

### Indexing

- JSONL parse groups messages into _turns_ (one user prompt + every following assistant / tool response until the next real user prompt). `tool_result` user messages are treated as continuations, not new turns.
- Turn text concatenates user prompt, assistant text, and tool names (rendered as `[tool:Read]`, `[tool:Bash]`, …) so tool usage becomes searchable.
- An inverted index with standard BM25 scoring (k1=1.5, b=0.75) is held in memory and persisted as gob. Top-K via a size-bounded min-heap.

### Incremental refresh

Each invocation stats every JSONL file and re-parses only those whose mtime changed. A 30-second cooldown skips the stat walk entirely when back-to-back queries are within that window — the common case.

### Fork detection

Every turn carries a `RootUserUUID` — the uuid of its session's first user message. Claude Code's fork action copies that uuid verbatim into the new session, so two sessions sharing a `RootUserUUID` are provably fork siblings. Subagent JSONLs live under `<proj>/subagents/` and are excluded from fork-family resolution (they carry the parent's sessionId but have their own root uuid).

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Acknowledgements

- Eric Tramel's [Searchable Agent Memory](https://eric-tramel.github.io/blog/2026-02-07-searchable-agent-memory/) for the original BM25-over-transcripts idea.
- [jhlee0409/claude-code-history-viewer](https://github.com/jhlee0409/claude-code-history-viewer) for the Plans indexing, fork awareness, and running-process feature directions.
