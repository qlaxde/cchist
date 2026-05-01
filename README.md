# cchist

Fast CLI for searching, preserving, and managing agent transcripts across every agent you run — today [Claude Code](https://claude.com/claude-code) and [Codex CLI](https://github.com/openai/codex). Indexes `~/.claude/projects/**/*.jsonl` plus `~/.codex/sessions/**/rollout-*.jsonl` with BM25, mirrors conversations + plans to a durable archive so they survive compaction and 30-day cleanups, and surfaces "loose threads" so you can close REPLs without losing track of work. Every result is tagged with its source (`[claude]` / `[codex]`).

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

### Let an agent install it

Paste the prompt below into a fresh Claude Code session (or any agent CLI that can read/write your filesystem and edit JSON) to install cchist end-to-end — binary, archive seed, and lifecycle hooks — without running the steps by hand:

```
Install cchist from https://github.com/qlaxde/cchist end-to-end. Do every step; stop and ask only if something fails.

1. Detect my OS and CPU arch with `uname -s` and `uname -m`. Map to one of: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Refuse to proceed on any other platform.

2. Download the matching binary from
   https://github.com/qlaxde/cchist/releases/latest/download/cchist-<os>-<arch>
   Install it at ~/.local/bin/cchist (mkdir -p the directory), chmod +x. If ~/.local/bin is not on PATH, tell me the exact shell-rc line to add.

3. Verify with `cchist help` — it should print the usage banner.

4. Run `cchist archive` once to snapshot my existing ~/.claude/projects transcripts and ~/.claude/plans into the durable archive. Report how many sessions and plans were archived.

5. Install three lifecycle hooks in ~/.claude/settings.json: PreCompact, SessionStart, SessionEnd. Each runs the command
      /Users/<me>/.local/bin/cchist hook 2>/dev/null || true
   with timeouts of 10s, 5s, 10s respectively (use the absolute path — hooks don't inherit my shell PATH).

   CRITICAL: READ the existing settings.json first and MERGE into the hooks object. Do NOT replace it. I probably have other hooks (formatters, MCP compressors, statusline, etc.) — preserve every one. Validate the final file with `jq -e .` before saving; abort if invalid.

6. Tell me the hooks won't take effect in my already-running Claude sessions — I need to open `/hooks` once (which reloads settings) or start a new session. Offer to also add a one-liner verifier hook command I can paste into `/hooks` to confirm `cchist hook` fires.

7. Report: binary path, binary size, archive totals, exactly which hook entries you added, and link me to the README for workflow commands: https://github.com/qlaxde/cchist/blob/main/README.md
```

The prompt is deliberately explicit — every step references a specific artefact, and the settings.json step spells out the merge/validate dance so the agent doesn't clobber existing hooks.

## Usage

### Search

**Default scope is the current project, across every installed agent.** `search`, `list`, and `threads` filter to the directory you're in (matched by prefix so a subdir like `apps/admin` still resolves to its repo root) and search Claude + Codex transcripts together. Use `--all` / `-a` to broaden to every project. Each row is tagged `[claude]` or `[codex]` so you can see at a glance where a hit came from.

```bash
cchist "sip gemini realtime"      # default: current project, both agents
cchist -a "sip gemini realtime"   # every project, every agent
cchist -p marketplace "…"          # filter by substring of the cwd path
cchist --since 7d "migration"      # recent hits only
cchist --show-forks "…"            # don't dedup fork siblings (see below)
```

When no hits match the default scope, cchist prints a hint pointing at `--all`.

### Browse

```bash
cchist list                           # sessions in current project, newest first
cchist list -a                        # across all projects
cchist show <session-prefix>          # print a full session
cchist show <session-prefix> 12       # print just turn #12
```

### Loose threads

Each Claude REPL you leave open leaks ~200 MB/hour. The reason you leave them open: they represent unfinished work you're afraid to lose. `cchist threads` surfaces them so you can close safely.

```bash
cchist threads                        # open threads in current project
cchist threads -a                     # across all projects
cchist threads --closed               # include completed + deprecated
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

Today `running` and `reap` scan for running **Claude Code** processes only — they rely on Claude's `--session-id` / `--resume` argv and the `SessionStart` hook breadcrumb. Codex process management is a planned extension.

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
~/.claude/projects/<proj-hash>/<session>.jsonl                  # Claude live (auto-deleted after 30 days)
~/.codex/sessions/YYYY/MM/DD/rollout-…-<session>.jsonl         # Codex live
~/.local/share/cchist/
├── conversations/
│   ├── claude/<proj-hash>/<session>.jsonl                      # Claude archive (cchist writes, kept forever)
│   └── codex/sessions/YYYY/MM/DD/rollout-…-<session>.jsonl     # Codex archive
├── plans/<slug>.md                                              # mirror of ~/.claude/plans — same 30-day risk
├── metadata.json                                                # completion / deprecated flags
└── current/<pid>.json                                           # SessionStart markers (Claude)
~/.cache/cchist/
├── corpus.gob                                                   # parsed turns + mtime map (schema v3)
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
