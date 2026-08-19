package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	usagestats "github.com/sliverkiss/token-usage-tracker/usage_stats"
)

// validFeedLine mirrors exactly what the workbuddy plugin appends.
func validFeedLine(ts, model, auth string, input, output, total int64) string {
	return `{"timestamp":"` + ts + `","latency_ms":1500,"source":"workbuddy","auth_index":"` + auth +
		`","provider":"workbuddy","model":"` + model + `","alias":"` + model +
		`","endpoint":"POST /v1/chat/completions","auth_type":"oauth","executor_type":"workbuddy",` +
		`"failed":false,"status_code":200,"tokens":{"input_tokens":` + itoa(input) +
		`,"output_tokens":` + itoa(output) + `,"reasoning_tokens":50,"cached_tokens":0,` +
		`"cache_read_tokens":0,"cache_creation_tokens":0,"total_tokens":` + itoa(total) + `}}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// resetFeedState clears the package-level importer state between tests.
func resetFeedState() {
	feedSyncMu.Lock()
	defer feedSyncMu.Unlock()
	feedOffset = 0
	feedOffsetLoaded = false
	storeMu.Lock()
	if usageStore != nil {
		_ = usageStore.Close()
		usageStore = nil
	}
	storeMu.Unlock()
	trackerCfgMu.Lock()
	trackerCfg = trackerConfig{
		FeedEnabled:     true,
		RetentionDays:   defaultUsageRetentionDays,
		FlushInterval:   defaultUsageFlushInterval,
		FlushMaxRecords: defaultUsageFlushMaxRecords,
		PollInterval:    defaultUsagePollInterval,
	}
	trackerCfgMu.Unlock()
}

func TestFeedIngestEndToEnd(t *testing.T) {
	resetFeedState()
	defer resetFeedState()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats.db")
	feedPath := filepath.Join(dir, "feed.ndjson")

	store, err := usagestats.Open(usagestats.Config{
		DataPath:        dbPath,
		RetentionDays:   365,
		FlushInterval:   time.Second,
		FlushMaxRecords: 100,
		SyncOnRecord:    true,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	storeMu.Lock()
	usageStore = store
	storeMu.Unlock()

	// The store's actor writes asynchronously; retention janitor may also run.
	// Use SyncOnRecord (set above) so Record returns after persistence.

	now := time.Now().UTC().Truncate(time.Second)
	lines := []string{
		validFeedLine(now.Add(-3*time.Minute).Format(time.RFC3339Nano), "deepseek-v4", "u-1", 100, 200, 350),
		validFeedLine(now.Add(-2*time.Minute).Format(time.RFC3339Nano), "deepseek-v4", "u-2", 50, 80, 130),
		validFeedLine(now.Add(-1*time.Minute).Format(time.RFC3339Nano), "claude-sonnet", "u-1", 900, 300, 1200),
	}
	feedData := lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n"
	if err := os.WriteFile(feedPath, []byte(feedData), 0o644); err != nil {
		t.Fatalf("write feed: %v", err)
	}

	// 1. syncUsageFeed imports all three lines and persists the offset.
	trackerCfgMu.Lock()
	trackerCfg.FeedPath = feedPath
	trackerCfg.DBPath = dbPath
	trackerCfgMu.Unlock()
	syncUsageFeed()
	feedSyncMu.Lock()
	offset := feedOffset
	feedSyncMu.Unlock()
	if offset != int64(len(feedData)) {
		t.Fatalf("offset = %d, want %d", offset, len(feedData))
	}
	offsetFile, err := os.ReadFile(feedPath + ".offset")
	if err != nil {
		t.Fatalf("offset file missing: %v", err)
	}
	if string(offsetFile) != itoa(int64(len(feedData)))+"\n" {
		t.Fatalf("offset file content = %q", offsetFile)
	}

	// 2. Queries see the imported data.
	query := url.Values{"range": []string{"24h"}}
	result := store.HandleQuery(http.MethodGet, "/stats", query, nil, nil)
	if result.Status != http.StatusOK {
		t.Fatalf("/stats status = %d body=%s", result.Status, result.Body)
	}
	// /stats returns the counters under "summary".
	stats := struct {
		Summary struct {
			Requests     uint64 `json:"requests"`
			TotalTokens  uint64 `json:"total_tokens"`
			InputTokens  uint64 `json:"input_tokens"`
			OutputTokens uint64 `json:"output_tokens"`
		} `json:"summary"`
	}{}
	if err := jsonUnmarshal(result.Body, &stats); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}
	if stats.Summary.Requests != 3 {
		t.Fatalf("requests = %d, want 3", stats.Summary.Requests)
	}
	if stats.Summary.TotalTokens != 1680 { // 350+130+1200
		t.Fatalf("total_tokens = %d, want 1680", stats.Summary.TotalTokens)
	}
	if stats.Summary.InputTokens != 1050 || stats.Summary.OutputTokens != 580 {
		t.Fatalf("tokens = %+v", stats.Summary)
	}

	// 3. Idempotent: second sync imports nothing new.
	syncUsageFeed()
	result = store.HandleQuery(http.MethodGet, "/stats", query, nil, nil)
	if got := jsonTokensTotal(result.Body); got != 1680 {
		t.Fatalf("after second sync total_tokens = %d, want 1680 (no duplicates)", got)
	}

	// 4. New line appended -> imported on next sync.
	extra := validFeedLine(now.Format(time.RFC3339Nano), "deepseek-v4", "u-1", 10, 20, 30) + "\n"
	f, err := os.OpenFile(feedPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open feed for append: %v", err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
	syncUsageFeed()
	result = store.HandleQuery(http.MethodGet, "/stats", query, nil, nil)
	if got := jsonTokensTotal(result.Body); got != 1710 {
		t.Fatalf("after append total_tokens = %d, want 1710", got)
	}

	// 5. Rotation: truncating the feed resets the offset and re-imports only
	//    the new content (no stale data).
	_ = os.WriteFile(feedPath, []byte(validFeedLine(now.Format(time.RFC3339Nano), "new-model", "u-3", 5, 5, 10)+"\n"), 0o644)
	syncUsageFeed()
	feedSyncMu.Lock()
	reset := feedOffset
	feedSyncMu.Unlock()
	if reset == 0 {
		t.Fatal("rotation did not reset offset to 0")
	}
	result = store.HandleQuery(http.MethodGet, "/stats", query, nil, nil)
	// 3 original + 1 appended + 1 post-rotation = 5 total requests.
	if got := jsonRequestsTotal(result.Body); got != 5 {
		t.Fatalf("after rotation total_requests = %d, want 5", got)
	}
}

func TestFeedIngestPartialTrailingLine(t *testing.T) {
	resetFeedState()
	defer resetFeedState()

	dir := t.TempDir()
	store, err := usagestats.Open(usagestats.Config{DataPath: filepath.Join(dir, "s.db"), RetentionDays: 365, FlushInterval: time.Second, FlushMaxRecords: 100, SyncOnRecord: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	line := validFeedLine(time.Now().UTC().Format(time.RFC3339Nano), "m", "u", 1, 1, 2)
	// No trailing newline: the final line is partial and must be left for the
	// next poll (simulates workbuddy appending concurrently).
	chunk := []byte(line + "\n" + line[:len(line)/2])
	consumed := ingestFeedChunk(store, chunk)
	if consumed != int64(len(line)+1) {
		t.Fatalf("consumed = %d, want %d", consumed, len(line)+1)
	}
	result := store.HandleQuery(http.MethodGet, "/stats", url.Values{"range": []string{"24h"}}, nil, nil)
	if got := jsonRequestsTotal(result.Body); got != 1 {
		t.Fatalf("total_requests = %d, want 1", got)
	}
}

func TestFeedIngestSkipsBadLine(t *testing.T) {
	resetFeedState()
	defer resetFeedState()

	dir := t.TempDir()
	store, err := usagestats.Open(usagestats.Config{DataPath: filepath.Join(dir, "s.db"), RetentionDays: 365, FlushInterval: time.Second, FlushMaxRecords: 100, SyncOnRecord: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	bad := "not-json\n"
	good := validFeedLine(time.Now().UTC().Format(time.RFC3339Nano), "m", "u", 2, 3, 5) + "\n"
	consumed := ingestFeedChunk(store, []byte(bad+good))
	if consumed != int64(len(bad)+len(good)) {
		t.Fatalf("consumed = %d, want %d", consumed, len(bad)+len(good))
	}
	result := store.HandleQuery(http.MethodGet, "/stats", url.Values{"range": []string{"24h"}}, nil, nil)
	if got := jsonRequestsTotal(result.Body); got != 1 {
		t.Fatalf("total_requests = %d, want 1", got)
	}
}

func TestConfigureParsesConfigYAML(t *testing.T) {
	resetFeedState()
	defer resetFeedState()

	dir := t.TempDir()
	cfg := "management_key: \"sekret\"\nusage_feed_enabled: true\nusage_feed_path: \"" +
		filepath.Join(dir, "feed.ndjson") + "\"\nusage_db_path: \"" +
		filepath.Join(dir, "db.bin") + "\"\nusage_retention_days: 30\n" +
		"usage_poll_interval: 2s\n"
	// Host serializes config_yaml as base64 (ConfigYAML []byte on the RPC
	// wire); pass []byte so json.Marshal mimics the real transport.
	raw, _ := jsonMarshal(map[string]any{"config_yaml": []byte(cfg)})
	configure(raw)

	trackerCfgMu.RLock()
	got := trackerCfg
	trackerCfgMu.RUnlock()
	if got.ManagementKey != "sekret" {
		t.Fatalf("management_key = %q", got.ManagementKey)
	}
	if got.FeedPath != filepath.Join(dir, "feed.ndjson") {
		t.Fatalf("feed path = %q", got.FeedPath)
	}
	if got.DBPath != filepath.Join(dir, "db.bin") {
		t.Fatalf("db path = %q", got.DBPath)
	}
	if got.RetentionDays != 30 {
		t.Fatalf("retention = %d", got.RetentionDays)
	}
	if got.PollInterval != 2*time.Second {
		t.Fatalf("poll = %v", got.PollInterval)
	}
	if !usageStatsOpen() {
		t.Fatal("store not open after configure")
	}
}

// ---- small helpers ----

func jsonUnmarshal(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// /stats returns counters under the "summary" object.
func jsonTokensTotal(body []byte) uint64 {
	var s struct {
		Summary struct {
			TotalTokens uint64 `json:"total_tokens"`
		} `json:"summary"`
	}
	_ = json.Unmarshal(body, &s)
	return s.Summary.TotalTokens
}

func jsonRequestsTotal(body []byte) uint64 {
	var s struct {
		Summary struct {
			Requests uint64 `json:"requests"`
		} `json:"summary"`
	}
	_ = json.Unmarshal(body, &s)
	return s.Summary.Requests
}
