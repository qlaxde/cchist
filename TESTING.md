# Testing conventions

Opinionated notes on how cchist is tested. Distilled from the Go Wiki, Russ
Cox's testing talks, Dave Cheney, Mitchell Hashimoto's *Advanced Testing with
Go*, the Go toolchain's own tests, and recent (2024–2026) write-ups.

The goal is high-signal tests written the stdlib way. No testify, no gomock,
no mockery, no goldie.

## Stack

- Stdlib `testing` for everything.
- `github.com/google/go-cmp/cmp` for `Diff` output on anything bigger than a
  scalar. The only non-stdlib test dependency.
- `testing/fstest.MapFS` when a unit needs a fake filesystem.
- `rogpeppe/go-internal/testscript` *iff* we grow end-to-end scripts — not
  adopted yet.

## Table-driven, map-keyed

Default shape. Use a map keyed on a human-readable scenario name so Go's
randomised map iteration exposes inter-case coupling. Since Go 1.22 the
`tt := tt` loop capture is no longer necessary.

```go
func TestParseLine(t *testing.T) {
    tests := map[string]struct {
        in   string
        want Turn
    }{
        "empty":          {in: "", want: Turn{}},
        "unknown record": {in: `{"type":"foo"}`, want: Turn{}},
        "user turn":      {in: `{"type":"user"}`, want: Turn{/*…*/}},
    }
    for name, tt := range tests {
        t.Run(name, func(t *testing.T) {
            got := parseLine(tt.in)
            if diff := cmp.Diff(tt.want, got); diff != "" {
                t.Errorf("parseLine(%q) mismatch (-want +got):\n%s", tt.in, diff)
            }
        })
    }
}
```

Split into separate `TestX`/`TestY` the moment cases diverge in setup or
expectations — don't bloat rows with optional fields and `if tt.wantErr`
branches.

## Isolation helpers

- **`t.TempDir()`** for every filesystem-touching test. Auto-cleanup, race- and
  parallel-safe.
- **`t.Setenv`** replaces manual save/restore for env vars (`$CCHIST_CACHE`,
  `$CCHIST_ARCHIVE`, `$CLAUDE_HISTORY_DIR`, `$CODEX_HOME`, `$HOME`). Forbids
  `t.Parallel()` in the test and its ancestors — that's fine.
- **`t.Helper()`** on every local helper that calls `t.Fatal`/`t.Error`. Without
  it, failures point at the helper, not the call site.
- **`t.Parallel()`** is opt-in, not default. Only reach for it once a suite is
  actually slow. Never combine with `t.Setenv` or shared temp resources.

## Fixtures

Three tools, one niche each:

| Need                                             | Use                                              |
|--------------------------------------------------|--------------------------------------------------|
| Pure parser / decoder, no real FS                | `strings.NewReader` or `fstest.MapFS`            |
| Code calling `os.Open`/`os.Stat`/honoring perms  | `t.TempDir()` + write files inline               |
| Large / visually-reviewed expected output        | `testdata/` with a `-update` flag                |

Prefer readers over paths. Parsers in this repo take `io.Reader`, not file
paths — callers wrap with `os.Open` before handing the reader in. That keeps
parser tests three lines: `r := strings.NewReader(jsonl); turns, err := parse(r)`.

## Interfaces stay small

Dave Cheney's rule of thumb — *accept interfaces, return structs* — holds.
Rules:

- Define the interface at the consumer, not the implementer.
- One or two methods max. Five methods = two interfaces.
- Prefer a hand-written fake over a mock. A `fakeSource` struct returning a
  canned slice is 10 lines; a gomock setup is 10 lines of `EXPECT()` plus
  codegen plumbing that nobody enjoys.
- Export test helpers from the package (Mitchell Hashimoto's `testing.go`
  pattern) when multiple test files need the same fake.

## Testing CLI dispatch

`main()` is a one-liner that calls `run(argv, stdin, stdout, stderr) int`.
That single refactor unlocks every CLI-level test without spawning a binary:

```go
func TestRun_SearchTagsSource(t *testing.T) {
    t.Setenv("HOME", t.TempDir())
    // … seed a codex rollout under $HOME/.codex/sessions …

    var stdout, stderr bytes.Buffer
    code := run([]string{"-a", "some-codex-only-phrase"}, nil, &stdout, &stderr)
    if code != 0 {
        t.Fatalf("exit %d, stderr=%s", code, stderr.String())
    }
    if !strings.Contains(stdout.String(), "[codex]") {
        t.Errorf("want [codex] tag in output, got:\n%s", stdout.String())
    }
}
```

For end-to-end coverage of argv parsing + multi-subcommand dispatch across a
real FS, reach for `rogpeppe/go-internal/testscript` and keep each scenario as
a `.txtar` file. Skip `os/exec` spawning the compiled binary — slow, kills
coverage, rarely catches anything unit tests don't.

## Golden files

Stdlib-only, with an `-update` flag. Don't pull in `goldie`/`cupaloy` — 15
lines covers it:

```go
var update = flag.Bool("update", false, "update golden files")

func TestFormat(t *testing.T) {
    got := Format(input)
    golden := filepath.Join("testdata", t.Name()+".golden")
    if *update {
        if err := os.WriteFile(golden, got, 0o644); err != nil { t.Fatal(err) }
    }
    want, err := os.ReadFile(golden)
    if err != nil { t.Fatal(err) }
    if diff := cmp.Diff(string(want), string(got)); diff != "" {
        t.Errorf("format mismatch (-want +got):\n%s", diff)
    }
}
```

Use only for large, human-reviewable output (rendered listings, help banners,
rich tables). Small structs are clearer as inline `want` values.

## JSONL / streaming state machines

This is cchist's hot loop. Structure each parser as a `Decoder` over
`io.Reader` with a pure state-transition function, then table-test it:

```go
func TestDecoder_TurnBoundaries(t *testing.T) {
    in := strings.Join([]string{
        `{"type":"user","content":"hi"}`,
        `{"type":"tool_use","id":"1"}`,
        `{"type":"assistant","content":"hello"}`,
        `{"type":"unknown_future_kind"}`, // must skip, not error
    }, "\n")

    got, err := parseClaude(strings.NewReader(in))
    if err != nil { t.Fatalf("parseClaude: %v", err) }

    want := []Turn{/* … */}
    if diff := cmp.Diff(want, got); diff != "" {
        t.Errorf("mismatch (-want +got):\n%s", diff)
    }
}
```

Patterns that earn their keep:

- One table of `(lines, expected turns)` covers 80% of cases. Each row is a
  scenario: empty file, trailing newline, partial line, unknown record,
  turn-boundary interleaving, malformed JSON mid-stream.
- `iotest.ErrReader` / `iotest.HalfReader` catch buffer-boundary bugs that the
  happy path hides.
- `cmp.Diff` on turn slices — field-by-field output is unreadable.
- Fuzz the line splitter with `func FuzzDecode(f *testing.F)` seeded on real
  JSONL snippets once the parser is stable. Typically finds a panic-on-empty-
  line within a minute.

Property-based testing (`testing/quick`, `pgregory.net/rapid`) is only worth
it for round-trip / total-order invariants. A curated table beats random
inputs for "skip unknown kinds" and "flush turn on real user message" logic.

## Anti-patterns

- `testify/assert` — sub-language, hides the real comparison.
- `gomock`, `uber-go/mock`, `testify/mock` — codegen + verbose DSL + brittle
  runtime typing. Archived (original `gomock`) or overkill for a single-package
  CLI.
- Mirror tests: one `*_test.go` per source file, one test per private function.
  Test via the exported API; internal refactors shouldn't turn the suite red.
- Reaching into unexported functions. If a private function needs direct
  testing, it probably wants to be its own package.
- `reflect.DeepEqual` — use `cmp.Diff`.
- `afero` or any third-party FS abstraction — `fstest.MapFS` is enough.

## Workflow rule

Vertical slices only — one test, one implementation, repeat. Never write all
tests first and then all code; that produces tests that verify shape rather
than behavior, and an incomplete understanding of what actually matters.

## Sources

- [Go Wiki: TestComments](https://go.dev/wiki/TestComments)
- [Go Wiki: TableDrivenTests](https://go.dev/wiki/TableDrivenTests)
- [testing](https://pkg.go.dev/testing) · [testing/fstest](https://pkg.go.dev/testing/fstest)
- [rogpeppe/go-internal/testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript)
- [Russ Cox — Go Testing By Example](https://research.swtch.com/testing)
- [Dave Cheney — Prefer table-driven tests](https://dave.cheney.net/2019/05/07/prefer-table-driven-tests)
- [Mitchell Hashimoto — Advanced Testing with Go](https://speakerdeck.com/mitchellh/advanced-testing-with-go)
- [Brandur — On using Go's t.Parallel()](https://brandur.org/t-parallel)
- [google/go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp)
