package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	cacheVersion = 1
	// maxCacheEntries bounds the file: one snapshot per distinct argument set,
	// newest first. A handful covers the bare fetch plus the few structures an
	// operator actually repeats.
	maxCacheEntries = 4
	// maxCacheBytes guards the read. The agent caps its own output at 64 KiB,
	// so anything past this is a corrupt or foreign file, not our cache.
	maxCacheBytes = 1024 * 1024
)

// cacheEntry is one successful fetch, kept so a sleeping machine still has
// something to say.
type cacheEntry struct {
	Args      []string `json:"args"`
	Output    string   `json:"output"`
	Host      string   `json:"host"`
	TookMS    int64    `json:"took_ms"`
	Truncated bool     `json:"truncated"`
	FetchedAt int64    `json:"fetched_at"`
}

type cache struct {
	CacheVersion int          `json:"cache_version"`
	Entries      []cacheEntry `json:"entries"`
}

// cachePath returns where snapshots are kept. Lavis starts modules with a
// cleared environment, so the per-module state directory is the only location
// the host guarantees; the home path is for runs outside Lavis.
func cachePath() string {
	if dir := os.Getenv("LAVIS_MODULE_STATE_DIR"); dir != "" {
		return filepath.Join(dir, "cache.json")
	}
	if home := homeDir(); home != "" {
		return filepath.Join(home, ".cache", "lavis", "remotefetch.json")
	}
	return ""
}

// loadCache never fails outward: a missing, oversized, corrupt or foreign
// cache is the same situation as an empty one — there is nothing stale to show.
func loadCache() cache {
	empty := cache{CacheVersion: cacheVersion}
	path := cachePath()
	if path == "" {
		return empty
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxCacheBytes {
		return empty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var loaded cache
	if err := json.Unmarshal(data, &loaded); err != nil || loaded.CacheVersion != cacheVersion {
		return empty
	}
	return loaded
}

// lookup prefers the snapshot taken with the same arguments and otherwise
// falls back to the newest one, reporting that the arguments differ so the
// reply can say so instead of quietly answering a different question.
func (c cache) lookup(args []string) (entry cacheEntry, sameArgs bool, found bool) {
	if len(c.Entries) == 0 {
		return cacheEntry{}, false, false
	}
	for _, candidate := range c.Entries {
		if slices.Equal(candidate.Args, args) {
			return candidate, true, true
		}
	}
	return c.Entries[0], false, true
}

// newest returns the most recent snapshot regardless of its arguments; status
// uses it to date the last contact with the machine.
func (c cache) newest() (cacheEntry, bool) {
	if len(c.Entries) == 0 {
		return cacheEntry{}, false
	}
	return c.Entries[0], true
}

// remember records one successful fetch, replacing any earlier snapshot taken
// with the same arguments. A cache that cannot be written is not worth failing
// a good reply over, so the error only reaches `lm logs`.
func remember(args []string, result fetchResponse, output string) {
	path := cachePath()
	if path == "" {
		return
	}
	entries := []cacheEntry{{
		Args:      args,
		Output:    output,
		Host:      result.Host,
		TookMS:    result.TookMS,
		Truncated: result.Truncated,
		FetchedAt: time.Now().Unix(),
	}}
	for _, old := range loadCache().Entries {
		if len(entries) == maxCacheEntries {
			break
		}
		if slices.Equal(old.Args, args) {
			continue
		}
		entries = append(entries, old)
	}
	if err := writeCache(path, cache{CacheVersion: cacheVersion, Entries: entries}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

// writeCache replaces the file atomically so a fetch interrupted by the host's
// lifecycle deadline cannot leave a half-written snapshot behind.
func writeCache(path string, current cache) error {
	data, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace cache: %w", err)
	}
	return nil
}
