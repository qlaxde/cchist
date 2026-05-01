package main

// Source abstracts over one agent's on-disk transcript format. Consumers
// (discovery, cache, archive, hook) iterate the registry rather than hard-
// coding a single agent's paths or parser.
//
// Keep the interface small: every method is pulled in by exactly one consumer
// and adding a method means extending every implementation, so methods should
// earn their place.
type Source interface {
	// ID is the stable, short identifier stamped into Turn.Source and into
	// archive paths. Use lower-case single words: "claude", "codex".
	ID() string

	// DisplayName is the human-readable label shown in UI badges and help
	// output ("Claude Code", "Codex CLI").
	DisplayName() string

	// Match reports whether the given on-disk path is a transcript this
	// source should parse. Paths that return false are ignored during
	// discovery even if they fall under the source's Roots.
	Match(path string) bool

	// Roots returns directories to walk, in priority order (archive copies
	// first so they shadow live transcripts when both exist — live files
	// can be rewritten or deleted by the agent, the archive cannot). Missing
	// directories are tolerated by the discovery walker.
	Roots() []string

	// Parse reads one transcript file and extracts turns in this source's
	// format. Errors propagate to refreshCache, which logs and continues.
	Parse(path string) ([]Turn, error)

	// ArchiveDst returns the archive destination for a live transcript path
	// under this source, or "" if the path isn't recognised as live content
	// for this source. Callers use the first non-empty result from the
	// registry.
	ArchiveDst(livePath string) string
}

// sources is the registry of known agents. Order is informational only —
// consumers dedupe by ID.
var sources = []Source{&claudeSource{}, &codexSource{}}

// sourceByID returns the registered Source with the given ID, or nil if no
// such agent is known.
func sourceByID(id string) Source {
	for _, s := range sources {
		if s.ID() == id {
			return s
		}
	}
	return nil
}
