package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentStub serves one canned fastfetch reply.
func agentStub(t *testing.T, output string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(fetchResponse{Output: output, Host: "xpert", TookMS: 84}); err != nil {
			t.Errorf("encode stub reply: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func failingAgent(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchServesLastSnapshotWhenTheMachineIsUnreachable(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	server := agentStub(t, "OS: NixOS\nCPU: i7\n")
	cfg := &config{URL: server.URL, Token: "secret", Label: "xpert"}

	fresh := fetch(context.Background(), cfg, "")
	if !strings.Contains(fresh, "OS: NixOS") || !strings.Contains(fresh, "84 мс") {
		t.Fatalf("live reply did not render the fetch: %q", fresh)
	}

	// The laptop goes to sleep.
	server.Close()
	stale := fetch(context.Background(), cfg, "")
	if !strings.Contains(stale, "OS: NixOS") {
		t.Fatalf("offline reply lost the snapshot: %q", stale)
	}
	if !strings.Contains(stale, "последний снимок") {
		t.Fatalf("offline reply did not say the output is old: %q", stale)
	}
	// The reason the machine is quiet still has to reach the operator.
	if !strings.Contains(stale, "недоступен") {
		t.Fatalf("offline reply dropped the failure: %q", stale)
	}
}

func TestFetchKeepsRejectedTokenVisibleInsteadOfServingASnapshot(t *testing.T) {
	// A wrong token is a configuration error the operator has to see; hiding it
	// behind old output would leave them hunting a problem already diagnosed.
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	live := agentStub(t, "OS: NixOS")
	if reply := fetch(context.Background(), &config{URL: live.URL, Token: "secret"}, ""); !strings.Contains(reply, "OS: NixOS") {
		t.Fatalf("failed to seed the cache: %q", reply)
	}

	rejecting := failingAgent(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	reply := fetch(context.Background(), &config{URL: rejecting.URL, Token: "wrong"}, "")
	if !strings.Contains(reply, "отклонил токен") {
		t.Fatalf("token failure was not reported: %q", reply)
	}
	if strings.Contains(reply, "OS: NixOS") {
		t.Fatalf("a rejected token answered with a snapshot: %q", reply)
	}
}

func TestFetchServesASnapshotWhenFastfetchTimesOutOnTheMachine(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	live := agentStub(t, "OS: NixOS")
	if reply := fetch(context.Background(), &config{URL: live.URL, Token: "secret"}, ""); !strings.Contains(reply, "OS: NixOS") {
		t.Fatalf("failed to seed the cache: %q", reply)
	}

	slow := failingAgent(t, http.StatusGatewayTimeout, `{"error":"fastfetch timed out after 2.5s"}`)
	reply := fetch(context.Background(), &config{URL: slow.URL, Token: "secret"}, "")
	if !strings.Contains(reply, "OS: NixOS") || !strings.Contains(reply, "последний снимок") {
		t.Fatalf("a timed-out run did not fall back to the snapshot: %q", reply)
	}
}

func TestFetchNotesWhenTheSnapshotUsedOtherArguments(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	server := agentStub(t, "OS: NixOS")
	cfg := &config{URL: server.URL, Token: "secret", Label: "xpert"}
	if reply := fetch(context.Background(), cfg, "--logo NixOS"); !strings.Contains(reply, "OS: NixOS") {
		t.Fatalf("failed to seed the cache: %q", reply)
	}
	server.Close()

	reply := fetch(context.Background(), cfg, "--structure OS:Kernel")
	if !strings.Contains(reply, "другими аргументами: --logo NixOS") {
		t.Fatalf("mismatched arguments were not disclosed: %q", reply)
	}

	// The same arguments must not be reported as a mismatch.
	if same := fetch(context.Background(), cfg, "--logo NixOS"); strings.Contains(same, "другими аргументами") {
		t.Fatalf("matching arguments were reported as different: %q", same)
	}
}

func TestFetchWithoutASnapshotStillReportsTheFailure(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := unreachable.URL
	unreachable.Close()

	reply := fetch(context.Background(), &config{URL: url, Token: "secret", Label: "xpert"}, "")
	if !strings.Contains(reply, "xpert недоступен") {
		t.Fatalf("unexpected reply with an empty cache: %q", reply)
	}
	if strings.Contains(reply, "последний снимок") {
		t.Fatalf("an empty cache claimed to have a snapshot: %q", reply)
	}
}

func TestStatusDatesTheSnapshotWhenTheMachineIsQuiet(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	live := agentStub(t, "OS: NixOS")
	if reply := fetch(context.Background(), &config{URL: live.URL, Token: "secret"}, ""); !strings.Contains(reply, "OS: NixOS") {
		t.Fatalf("failed to seed the cache: %q", reply)
	}
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := unreachable.URL
	unreachable.Close()

	reply := status(context.Background(), &config{URL: url, Token: "secret", Label: "xpert"})
	if !strings.Contains(reply, "недоступен") || !strings.Contains(reply, "Есть снимок") {
		t.Fatalf("status did not date the snapshot: %q", reply)
	}
}

func TestRememberKeepsOneSnapshotPerArgumentSetAndTrims(t *testing.T) {
	t.Setenv("LAVIS_MODULE_STATE_DIR", t.TempDir())
	for index := 0; index <= maxCacheEntries; index++ {
		remember([]string{"--logo", string(rune('a' + index))}, fetchResponse{Host: "xpert"}, "output")
	}
	current := loadCache()
	if len(current.Entries) != maxCacheEntries {
		t.Fatalf("cache holds %d entries, limit %d", len(current.Entries), maxCacheEntries)
	}

	// Re-fetching with known arguments moves that snapshot to the front rather
	// than adding a second copy of it.
	args := []string{"--logo", "b"}
	remember(args, fetchResponse{Host: "xpert"}, "newer")
	current = loadCache()
	if len(current.Entries) != maxCacheEntries {
		t.Fatalf("a repeat fetch changed the entry count: %d", len(current.Entries))
	}
	entry, sameArgs, found := current.lookup(args)
	if !found || !sameArgs || entry.Output != "newer" {
		t.Fatalf("repeat fetch did not replace the snapshot: %#v", entry)
	}
	seen := 0
	for _, candidate := range current.Entries {
		if candidate.Output == "newer" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("argument set stored %d times", seen)
	}
}

func TestLoadCacheTreatsAnUnusableFileAsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAVIS_MODULE_STATE_DIR", dir)
	path := filepath.Join(dir, "cache.json")
	for _, content := range []string{"not json at all", `{"cache_version":99,"entries":[{"output":"x"}]}`} {
		if err := writeFile(path, content); err != nil {
			t.Fatalf("write: %v", err)
		}
		if entries := loadCache().Entries; len(entries) != 0 {
			t.Fatalf("unusable cache %q produced %d entries", content, len(entries))
		}
	}
}

func TestCacheIsWrittenPrivatelyAndAtomically(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAVIS_MODULE_STATE_DIR", dir)
	remember(nil, fetchResponse{Host: "xpert"}, "OS: NixOS")

	info, err := os.Stat(filepath.Join(dir, "cache.json"))
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	// The snapshot is fastfetch output about a private machine; it is no more
	// readable than the config next to it.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cache mode = %#o, want 0600", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file survived the write")
	}
}

func TestStaleReplyRendersAnAgeFromAKnownTimestamp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAVIS_MODULE_STATE_DIR", dir)
	entry := cacheEntry{Output: "OS: NixOS", Host: "xpert", FetchedAt: time.Now().Add(-3 * time.Hour).Unix()}
	if err := writeCache(filepath.Join(dir, "cache.json"), cache{CacheVersion: cacheVersion, Entries: []cacheEntry{entry}}); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	reply := staleReply(&config{Label: "xpert"}, nil, "🔌 xpert недоступен")
	if !strings.Contains(reply, "3ч 0мин назад") {
		t.Fatalf("age was not rendered: %q", reply)
	}
}

func TestStaleReplyIgnoresATimestampFromTheFuture(t *testing.T) {
	// A clock that moved backwards between the fetch and the reply must not
	// print a negative age.
	dir := t.TempDir()
	t.Setenv("LAVIS_MODULE_STATE_DIR", dir)
	entry := cacheEntry{Output: "OS: NixOS", FetchedAt: time.Now().Add(time.Hour).Unix()}
	if err := writeCache(filepath.Join(dir, "cache.json"), cache{CacheVersion: cacheVersion, Entries: []cacheEntry{entry}}); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	reply := staleReply(&config{Label: "xpert"}, nil, "🔌 недоступен")
	if !strings.Contains(reply, "0с назад") {
		t.Fatalf("future timestamp was not clamped: %q", reply)
	}
}

func TestWithFooterKeepsTheWholeReplyInsideTheBudget(t *testing.T) {
	footer := "🕒 xpert не на связи — показан последний снимок, 2д 3ч назад."
	reply := withFooter(strings.Repeat("a", maxReplyUnits*2), footer)
	if utf16Len(reply) > maxReplyUnits {
		t.Fatalf("reply is %d units, limit %d", utf16Len(reply), maxReplyUnits)
	}
	if !strings.HasSuffix(reply, footer) {
		t.Fatalf("footer did not survive clipping: %q", reply[len(reply)-80:])
	}
}

func TestStaleReplyFitsTheRenderingBudget(t *testing.T) {
	// The banner, the mismatched-arguments line and the failure all sit around
	// the output; only the output may be sacrificed to fit.
	dir := t.TempDir()
	t.Setenv("LAVIS_MODULE_STATE_DIR", dir)
	entry := cacheEntry{
		Args:      []string{"--logo", strings.Repeat("N", 512)},
		Output:    strings.Repeat("🦀", maxReplyUnits),
		Host:      "xpert",
		FetchedAt: time.Now().Add(-2 * time.Hour).Unix(),
	}
	if err := writeCache(filepath.Join(dir, "cache.json"), cache{CacheVersion: cacheVersion, Entries: []cacheEntry{entry}}); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	reason := "🔌 xpert недоступен (http://100.120.95.96:8471). Проверь, что он не спит, в сети и lavis-fetchd запущен."
	reply := staleReply(&config{Label: "xpert"}, []string{"--structure", "OS"}, reason)
	if utf16Len(reply) > maxReplyUnits {
		t.Fatalf("stale reply is %d units, limit %d", utf16Len(reply), maxReplyUnits)
	}
	if !strings.HasPrefix(reply, "🕒 xpert не на связи") || !strings.HasSuffix(reply, reason) {
		t.Fatalf("stale reply lost its banner or its reason: %q", reply[:60])
	}
}
