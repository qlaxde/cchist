package main

import (
	"encoding/gob"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// cacheSchemaVersion bumps every time the on-disk format of a parsed Turn
// changes in a way that old caches can't represent. A mismatched Version on
// load discards the cache and forces a full reparse — a few seconds of
// one-off work, infinitely preferable to silently returning stale data.
const cacheSchemaVersion = 4

// Cache is the persisted index state. Gob keeps it compact and fast to load;
// a typical ~10k-turn history round-trips in well under 100ms.
type Cache struct {
	Version     int
	// FileMTimes holds the modification time we last parsed each JSONL file
	// at, as Unix nanoseconds. When a file's mtime changes we re-parse only
	// that file rather than the whole history.
	FileMTimes  map[string]int64
	TurnsByFile map[string][]Turn
	// UpdatedAt lets us short-circuit the filesystem rescan when the cache
	// was refreshed very recently — "run a few searches in a row" is the
	// common case and scanning 5k files every time is wasted work.
	UpdatedAt int64
}

// persistedIndex mirrors BM25 in a gob-friendly shape. Kept in its own file
// so we only rewrite it when the corpus actually changed.
type persistedIndex struct {
	Postings map[string][]posting
	DocLens  []uint32
	AvgDocL  float64
}

// refreshOptions controls behaviour of refreshCache.
type refreshOptions struct {
	Force        bool          // reparse every file regardless of mtime
	RescanWindow time.Duration // skip FS scan if cache was updated within this window
	Verbose      bool
}

const defaultRescanWindow = 30 * time.Second

// loadCache reads the on-disk cache or returns an empty one on any failure
// (missing file, corrupt gob, etc.). We never surface load errors to the user
// because the fallback — rebuilding from scratch — is always correct.
func loadCache(path string) *Cache {
	empty := &Cache{
		Version:     cacheSchemaVersion,
		FileMTimes:  make(map[string]int64),
		TurnsByFile: make(map[string][]Turn),
	}
	f, err := os.Open(path)
	if err != nil {
		return empty
	}
	defer f.Close()
	var loaded Cache
	if err := gob.NewDecoder(f).Decode(&loaded); err != nil {
		return empty
	}
	if loaded.Version != cacheSchemaVersion {
		// Older gob without the fields we need — throw it away.
		return empty
	}
	if loaded.FileMTimes == nil {
		loaded.FileMTimes = make(map[string]int64)
	}
	if loaded.TurnsByFile == nil {
		loaded.TurnsByFile = make(map[string][]Turn)
	}
	return &loaded
}

func saveCache(path string, c *Cache) error {
	return saveGob(path, c)
}

// saveGob encodes any value to `path` atomically via a sibling tempfile.
func saveGob(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadIndex reads a previously persisted BM25 into a live struct. Missing or
// corrupt files yield nil so the caller falls back to rebuilding.
func loadIndex(path string) *BM25 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var p persistedIndex
	if err := gob.NewDecoder(f).Decode(&p); err != nil {
		return nil
	}
	if p.Postings == nil {
		return nil
	}
	return &BM25{
		postings: p.Postings,
		docLens:  p.DocLens,
		avgDocL:  p.AvgDocL,
	}
}

func saveIndex(path string, b *BM25) error {
	return saveGob(path, persistedIndex{
		Postings: b.postings,
		DocLens:  b.docLens,
		AvgDocL:  b.avgDocL,
	})
}

// migrateOnce makes the one-shot v2→v3 archive layout migration invisible
// to the user — first refreshCache call per process runs it, every later
// one is a no-op.
var migrateOnce sync.Once

// refreshCache walks every registered Source's roots, re-parses any file
// whose mtime changed, drops files that have disappeared, and persists the
// updated cache. Returns the cache and whether anything changed.
func refreshCache(cachePath string, opts refreshOptions) (*Cache, bool, error) {
	migrateOnce.Do(func() { _ = migrateLegacyArchive() })
	cache := loadCache(cachePath)

	// Fast-path: if the cache is fresh and we aren't forcing a rescan, skip
	// the directory walk entirely. Saves ~0.5s on typical repeat queries.
	if !opts.Force && opts.RescanWindow > 0 && cache.UpdatedAt > 0 {
		age := time.Since(time.Unix(0, cache.UpdatedAt))
		if age >= 0 && age < opts.RescanWindow {
			return cache, false, nil
		}
	}

	// Discover from every source; each source walks archive first, live after,
	// so archived pre-compact copies shadow rewritten live transcripts.
	found, err := discoverAll(sources)
	if err != nil {
		return cache, false, err
	}

	changed := false
	seen := make(map[string]struct{}, len(found))

	for _, fi := range found {
		seen[fi.Path] = struct{}{}
		prev, ok := cache.FileMTimes[fi.Path]
		if !opts.Force && ok && prev == fi.MTime {
			continue
		}
		src := sourceByID(fi.Source)
		if src == nil {
			continue
		}
		turns, err := src.Parse(fi.Path)
		if err != nil {
			if opts.Verbose {
				fmt.Fprintf(os.Stderr, "  ! parse failed %s: %v\n", fi.Path, err)
			}
			continue
		}
		cache.TurnsByFile[fi.Path] = turns
		cache.FileMTimes[fi.Path] = fi.MTime
		changed = true
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "  + [%s] %s (%d turns)\n", fi.Source, filepath.Base(fi.Path), len(turns))
		}
	}

	// Drop records for files that no longer exist on disk.
	for p := range cache.TurnsByFile {
		if _, still := seen[p]; !still {
			delete(cache.TurnsByFile, p)
			delete(cache.FileMTimes, p)
			changed = true
			if opts.Verbose {
				fmt.Fprintf(os.Stderr, "  - removed %s\n", p)
			}
		}
	}

	if changed || opts.Force {
		cache.Version = cacheSchemaVersion
		cache.UpdatedAt = time.Now().UnixNano()
		if err := saveCache(cachePath, cache); err != nil {
			return cache, changed, err
		}
	}
	return cache, changed, nil
}

type fileInfo struct {
	Source string
	Path   string
	MTime  int64
}

// discoverAll walks every registered Source's Roots in priority order and
// returns one fileInfo per distinct transcript. Dedup is per-source on the
// relative path under each root: an archive copy shadows the live file of the
// same session, but two sessions with the same relative name under different
// sources are kept separately.
func discoverAll(srcs []Source) ([]fileInfo, error) {
	var out []fileInfo
	for _, src := range srcs {
		seen := make(map[string]struct{})
		for _, root := range src.Roots() {
			if root == "" {
				continue
			}
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if !src.Match(path) {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return nil
				}
				if _, dup := seen[rel]; dup {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				seen[rel] = struct{}{}
				out = append(out, fileInfo{
					Source: src.ID(),
					Path:   path,
					MTime:  info.ModTime().UnixNano(),
				})
				return nil
			})
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	return out, nil
}

// allTurns returns every turn across every cached file in a stable order.
// Callers that need to map BM25 doc IDs back to turns should use the same
// iteration order we use when building the index — i.e. call this function.
func (c *Cache) allTurns() []Turn {
	// Preallocate based on an estimate to avoid growslice churn.
	total := 0
	for _, ts := range c.TurnsByFile {
		total += len(ts)
	}
	out := make([]Turn, 0, total)
	// Stable order: iterate by sorted file path so the BM25 doc IDs are
	// deterministic across runs.
	paths := make([]string, 0, len(c.TurnsByFile))
	for p := range c.TurnsByFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		out = append(out, c.TurnsByFile[p]...)
	}
	return out
}
