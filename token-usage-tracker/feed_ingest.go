// feed_ingest.go implements the tracker plugin configuration, the bbolt store
// lifecycle, the shared-usage-feed importer, and the statistics query bridge.
//
// Configuration (config_yaml, all optional):
//
//	management_key:        Bearer token for write endpoints (or env
//	                       TOKEN_USAGE_TRACKER_MANAGEMENT_KEY)
//	usage_feed_enabled:    tail the shared feed (default true)
//	usage_feed_path:       shared NDJSON feed (default
//	                       <CLIProxyAPI root>/data/token-usage-feed.ndjson)
//	usage_db_path:         bbolt database (default
//	                       <CLIProxyAPI root>/data/token-usage-tracker.db)
//	usage_retention_days:  1-3650 (default 365)
//	usage_flush_interval:  1s-1h (default 5s)
//	usage_flush_max_records: 1-1000000 (default 100)
//	usage_poll_interval:   feed poll interval, 1s-1h (default 5s)
//
// The importer is the ONLY process that writes the bbolt database (the
// workbuddy plugin never touches it — it only appends to the feed). This
// avoids the bbolt exclusive-flock conflict that two long-lived writers
// would cause.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	usagestats "github.com/sliverkiss/token-usage-tracker/usage_stats"
)

const (
	defaultUsageRetentionDays   = 365
	defaultUsageFlushInterval   = 5 * time.Second
	defaultUsageFlushMaxRecords = 100
	defaultUsagePollInterval    = 5 * time.Second
	defaultUsageFeedFileName    = "token-usage-feed.ndjson"
	defaultUsageDBFileName      = "token-usage-tracker.db"
	// maxUsageFeedLineBytes guards against a corrupt/giant single line.
	maxUsageFeedLineBytes = 1 << 20
)

// trackerConfig is the lock-protected plugin configuration snapshot.
type trackerConfig struct {
	ManagementKey   string
	FeedEnabled     bool
	FeedPath        string
	DBPath          string
	RetentionDays   int
	FlushInterval   time.Duration
	FlushMaxRecords int
	PollInterval    time.Duration
}

var (
	trackerCfgMu  sync.RWMutex
	trackerCfg = trackerConfig{
		FeedEnabled:     true,
		RetentionDays:   defaultUsageRetentionDays,
		FlushInterval:   defaultUsageFlushInterval,
		FlushMaxRecords: defaultUsageFlushMaxRecords,
		PollInterval:    defaultUsagePollInterval,
	}
)

// Store lifecycle + feed importer state.
var (
	storeMu          sync.RWMutex
	usageStore       *usagestats.Store
	importerOnce     sync.Once
	feedSyncMu       sync.Mutex
	feedOffset       int64
	feedOffsetLoaded bool
)

// configure parses config_yaml and (re)opens the store. Called from
// register/reconfigure. Failures are non-fatal: the plugin keeps serving the
// dashboard and only disables statistics ingestion.
func configure(raw []byte) {
	next := trackerConfig{
		FeedEnabled:     true,
		RetentionDays:   defaultUsageRetentionDays,
		FlushInterval:   defaultUsageFlushInterval,
		FlushMaxRecords: defaultUsageFlushMaxRecords,
		PollInterval:    defaultUsagePollInterval,
	}
	next.ManagementKey = strings.TrimSpace(os.Getenv("TOKEN_USAGE_TRACKER_MANAGEMENT_KEY"))

	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				value := func() string {
					_, v, _ := strings.Cut(line, ":")
					return strings.Trim(strings.TrimSpace(v), "\"'")
				}
				switch {
				case strings.HasPrefix(line, "management_key:"):
					if v := value(); v != "" {
						next.ManagementKey = v
					}
				case strings.HasPrefix(line, "usage_feed_enabled:"):
					v := value()
					next.FeedEnabled = v == "true" || v == "1" || v == "yes" || v == "on"
				case strings.HasPrefix(line, "usage_feed_path:"):
					if v := value(); v != "" {
						next.FeedPath = v
					}
				case strings.HasPrefix(line, "usage_db_path:"):
					if v := value(); v != "" {
						next.DBPath = v
					}
				case strings.HasPrefix(line, "usage_retention_days:"):
					if n, err := strconv.Atoi(value()); err == nil && n >= 1 && n <= 3650 {
						next.RetentionDays = n
					}
				case strings.HasPrefix(line, "usage_flush_interval:"):
					if d, err := time.ParseDuration(value()); err == nil && d >= time.Second && d <= time.Hour {
						next.FlushInterval = d
					}
				case strings.HasPrefix(line, "usage_flush_max_records:"):
					if n, err := strconv.Atoi(value()); err == nil && n >= 1 && n <= 1_000_000 {
						next.FlushMaxRecords = n
					}
				case strings.HasPrefix(line, "usage_poll_interval:"):
					if d, err := time.ParseDuration(value()); err == nil && d >= time.Second && d <= time.Hour {
						next.PollInterval = d
					}
				}
			}
		}
	}
	if next.FeedPath == "" {
		next.FeedPath = usagestats.DefaultDataPath(defaultUsageFeedFileName)
	}
	if next.DBPath == "" {
		next.DBPath = usagestats.DefaultDataPath(defaultUsageDBFileName)
	}

	trackerCfgMu.Lock()
	trackerCfg = next
	trackerCfgMu.Unlock()

	reopenStore(next)
	importerOnce.Do(func() {
		go feedImporterLoop()
	})
}

// reopenStore opens (or re-opens) the bbolt store for the given config.
//
// When a new store replaces an old one, the old store is closed INSIDE the
// storeMu critical section. Otherwise the feedImporterLoop may have already
// captured the old reference under the read lock, return after we drop the
// write lock, and discover the store closed mid-ingest. Closing inside the
// critical section guarantees that any subsequent RLock sees usageStore
// pointing at the new store, so old-store ingest calls quickly fail at the
// Store.send() gate instead of panicking into the host's fusePlugin trap.
func reopenStore(cfg trackerConfig) {
	if !cfg.FeedEnabled {
		storeMu.Lock()
		if usageStore != nil {
			_ = usageStore.Close()
			usageStore = nil
		}
		storeMu.Unlock()
		return
	}
	next, err := usagestats.Open(usagestats.Config{
		DataPath:        cfg.DBPath,
		RetentionDays:   cfg.RetentionDays,
		FlushInterval:   cfg.FlushInterval,
		FlushMaxRecords: cfg.FlushMaxRecords,
		SyncOnRecord:    true,
	})
	if err != nil {
		trackerWarnf("storage disabled (open %s: %v)", cfg.DBPath, err)
		storeMu.Lock()
		if usageStore != nil {
			_ = usageStore.Close()
		}
		usageStore = nil
		storeMu.Unlock()
		return
	}
	storeMu.Lock()
	old := usageStore
	usageStore = next
	if old != nil && old != next {
		_ = old.Close()
	}
	storeMu.Unlock()
}

func usageStatsOpen() bool {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return usageStore != nil
}

// handleUsage is the UsagePlugin entry point (pluginabi.MethodUsageHandle).
// The host broadcasts one canonical pluginapi.UsageRecord after every request
// served by non-plugin executors — i.e. third-party api providers. Those
// records are recorded into the local store so the dashboard shows them next
// to the workbuddy records that arrive via the shared feed.
//
// workbuddy plugin-executor requests do NOT arrive here: the host UsagePlugin
// broadcast never fires for plugin executors (that is why workbuddy appends
// the shared NDJSON feed instead), so no cross-path dedup is needed.
//
// Important: we must NEVER return (nil, err) here. The host's RPC adapter
// wraps any error reply in a panic recover that calls fusePlugin(id, ...) and
// permanently disables this plugin's UsagePlugin — every subsequent
// api-provider record would then be silently dropped. The store may be
// transiently closed during a reopenStore swap; the right response is to
// ack with recorded=false and let the host keep broadcasting.
func handleUsage(raw []byte) ([]byte, error) {
	storeMu.RLock()
	store := usageStore
	storeMu.RUnlock()
	if store == nil {
		return okEnvelope(map[string]any{"recorded": false, "reason": "store not initialized"})
	}
	if err := store.RecordUsageRecord(raw); err != nil {
		trackerWarnf("usage.handle: record failed (non-fatal): %v", err)
		return okEnvelope(map[string]any{"recorded": false, "reason": err.Error()})
	}
	return okEnvelope(map[string]any{"recorded": true})
}

// usageStatsQuery dispatches a statistics query against the store.
func usageStatsQuery(method, rel string, query url.Values, body []byte, headers http.Header) usagestats.QueryResult {
	storeMu.RLock()
	store := usageStore
	storeMu.RUnlock()
	if store == nil {
		return usagestats.QueryResult{
			Status:  http.StatusServiceUnavailable,
			Headers: jsonResultHeaders(),
			Body:    []byte(`{"error":"usage statistics is disabled or storage is not initialized"}`),
		}
	}
	return store.HandleQuery(method, rel, query, body, headers)
}

func jsonResultHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return h
}

// -----------------------------------------------------------------------------
// Feed importer
// -----------------------------------------------------------------------------

func feedImporterLoop() {
	trackerCfgMu.RLock()
	interval := trackerCfg.PollInterval
	trackerCfgMu.RUnlock()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		syncUsageFeed()
	}
}

// syncUsageFeed performs one tail pass over the shared feed: read everything
// after the persisted offset, import complete NDJSON lines, advance the
// offset. Serialized so the poll ticker and management queries never race.
func syncUsageFeed() {
	feedSyncMu.Lock()
	defer feedSyncMu.Unlock()

	trackerCfgMu.RLock()
	enabled := trackerCfg.FeedEnabled
	feedPath := trackerCfg.FeedPath
	trackerCfgMu.RUnlock()
	if !enabled || feedPath == "" {
		return
	}
	storeMu.RLock()
	store := usageStore
	storeMu.RUnlock()
	if store == nil {
		return
	}

	f, err := os.Open(feedPath)
	if err != nil {
		return // feed not created yet — workbuddy may not have run
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	offset := loadFeedOffset(feedPath, size)
	if offset >= size {
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return
	}
	processed := ingestFeedChunk(store, chunk)
	newOffset := offset + processed
	if newOffset > size {
		newOffset = size
	}
	if newOffset > offset {
		saveFeedOffset(feedPath, newOffset)
	}
}

// loadFeedOffset returns the persisted read offset, resetting to 0 when the
// feed was rotated (file smaller than the stored offset). The caller holds
// feedSyncMu.
func loadFeedOffset(feedPath string, size int64) int64 {
	if feedOffsetLoaded && feedOffset <= size {
		return feedOffset
	}
	feedOffset = 0
	feedOffsetLoaded = true
	if raw, err := os.ReadFile(feedPath + ".offset"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && n >= 0 && n <= size {
			feedOffset = n
		}
	}
	return feedOffset
}

func saveFeedOffset(feedPath string, offset int64) {
	feedOffset = offset
	feedOffsetLoaded = true
	_ = os.WriteFile(feedPath+".offset", []byte(strconv.FormatInt(offset, 10)+"\n"), 0o644)
}

// ingestFeedChunk parses complete NDJSON lines from chunk and records them.
// Returns the number of bytes consumed (all complete lines; a trailing
// partial line is left for the next poll).
func ingestFeedChunk(store *usagestats.Store, chunk []byte) int64 {
	text := string(chunk)
	lines := strings.Split(text, "\n")
	consumed := int64(0)
	for i, line := range lines {
		isLast := i == len(lines)-1
		// Phantom element after a trailing newline ("a\n" -> ["a", ""]) or a
		// trailing partial line ("a\nb" -> the "b" has no terminator yet):
		// both consume nothing and are left for the next poll.
		if isLast && (line == "" || !strings.HasSuffix(text, "\n")) {
			break
		}
		if len(line) == 0 {
			consumed += 1
			continue
		}
		consumed += int64(len(line)) + 1
		if int64(len(line)) > maxUsageFeedLineBytes {
			trackerWarnf("feed: skipping oversized line (%d bytes)", len(line))
			continue
		}
		if err := store.RecordFeedNDJSON(line); err != nil {
			trackerWarnf("feed: skipping unparsable line: %v", err)
			continue
		}
	}
	return consumed
}

func trackerWarnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[token-usage-tracker] "+format+"\n", args...)
}
