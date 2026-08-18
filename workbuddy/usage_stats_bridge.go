// usage_stats_bridge.go integrates the merged local token-usage statistics
// stack (usage_stats subpackage, ported from AITNR/cap-token-usage-tracker)
// into the workbuddy plugin.
//
// Data source: the workbuddy executor chain. Every completed request already
// flows through publishUsage() with a parsed usage.Detail (see usage.go); we
// additionally record that detail into a local bbolt database so the plugin
// can show token consumption per model/account/time without depending on the
// host's UsagePlugin broadcast (which never fires for plugin executors).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	usagestats "github.com/sliverkiss/workbuddy-plus/usage_stats"
)

// Local statistics defaults. The database lives next to the plugins directory
// by default (discovered the same way the tracker plugin locates it), so
// Docker bind-mounts of the plugins dir also persist statistics.
const (
	defaultUsageStatsPath          = "" // resolved by usagestats.DefaultDataPath
	defaultUsageRetentionDays      = 365
	defaultUsageFlushInterval      = 5 * time.Second
	defaultUsageFlushMaxRecords    = 100
	usageStatsSourceServiceAddress = "https://www.codebuddy.cn"
)

// usageStatsState is the lock-protected snapshot of the local statistics
// store. It mirrors the pattern used by the rest of the plugin config.
var (
	usageStatsMu      sync.RWMutex
	usageStatsStore   *usagestats.Store
	usageStatsEnabled = true
	usageStatsPath    = ""
)

func usageStatsOpen() bool {
	usageStatsMu.RLock()
	defer usageStatsMu.RUnlock()
	return usageStatsStore != nil
}

// configureUsageStats parses the usage_stats_* fields from config_yaml and
// opens/reconfigures the local statistics store. Called from configure().
// Failures are non-fatal: the plugin keeps serving chat and only disables
// local statistics (the CPAMP forward path is unaffected).
func configureUsageStats(raw []byte) {
	enabled := true
	dataPath := ""
	retentionDays := defaultUsageRetentionDays
	flushInterval := defaultUsageFlushInterval
	flushMaxRecords := defaultUsageFlushMaxRecords

	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "usage_stats_enabled:"):
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_stats_enabled:")), "\"'")
					enabled = v == "true" || v == "1" || v == "yes" || v == "on"
				case strings.HasPrefix(line, "usage_stats_path:"):
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_stats_path:")), "\"'")
					if v != "" {
						dataPath = v
					}
				case strings.HasPrefix(line, "usage_retention_days:"):
					if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "usage_retention_days:"))); err == nil && n >= 1 && n <= 3650 {
						retentionDays = n
					}
				case strings.HasPrefix(line, "usage_flush_interval:"):
					if d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "usage_flush_interval:"))); err == nil && d >= time.Second && d <= time.Hour {
						flushInterval = d
					}
				case strings.HasPrefix(line, "usage_flush_max_records:"):
					if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "usage_flush_max_records:"))); err == nil && n >= 1 && n <= 1_000_000 {
						flushMaxRecords = n
					}
				}
			}
		}
	}

	if dataPath == "" {
		dataPath = usagestats.DefaultDataPath("usage-stats.db")
	}

	cfg := usagestats.Config{
		DataPath:        dataPath,
		RetentionDays:   retentionDays,
		FlushInterval:   flushInterval,
		FlushMaxRecords: flushMaxRecords,
		SyncOnRecord:    true,
	}

	usageStatsMu.Lock()
	wasEnabled := usageStatsEnabled
	old := usageStatsStore
	usageStatsEnabled = enabled
	usageStatsPath = dataPath
	usageStatsMu.Unlock()

	if !enabled {
		if old != nil {
			_ = old.Close()
			usageStatsMu.Lock()
			if usageStatsStore == old {
				usageStatsStore = nil
			}
			usageStatsMu.Unlock()
		}
		return
	}

	next, err := usagestats.Open(cfg)
	if err != nil {
		// Non-fatal: statistics are a bonus feature, chat must not break.
		usageStatsMu.Lock()
		usageStatsStore = nil
		usageStatsMu.Unlock()
		logWarnf("usage stats: disabled (open %s: %v)", dataPath, err)
		return
	}
	usageStatsMu.Lock()
	usageStatsStore = next
	usageStatsMu.Unlock()
	if old != nil && old != next {
		_ = old.Close()
	}
	_ = wasEnabled
}

// recordLocalUsage records one request into the local statistics store. It is
// called from publishUsage so every completed request (success or failure) is
// captured. Never blocks the executor hot path beyond an actor channel send.
func recordLocalUsage(alias, model, authUID string, started time.Time, detail usage.Detail, failed bool, statusCode int) {
	usageStatsMu.RLock()
	store := usageStatsStore
	usageStatsMu.RUnlock()
	if store == nil {
		return
	}
	latencyNS := uint64(0)
	if !started.IsZero() {
		if d := time.Since(started); d > 0 {
			latencyNS = uint64(d.Nanoseconds())
		}
	}
	counters := usagestats.NewCounters(
		detail.InputTokens, detail.OutputTokens, detail.ReasoningTokens,
		detail.CachedTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
		detail.TotalTokens,
	)
	record := usagestats.NewUsage(
		providerName, "workbuddy", model, alias, usageStatsSourceServiceAddress,
		"oauth", authUID, started, latencyNS, failed, statusCode, counters,
	)
	_ = store.Record(record)
}

// usageStatsManagementResult wraps a statistics QueryResult into a
// ManagementResponse for the host.
func usageStatsManagementResult(result usagestats.QueryResult) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: result.Status,
		Headers:    result.Headers,
		Body:       result.Body,
	}
}

// handleUsageStatsResource serves the token usage dashboard page and the
// read-only statistics API under /v0/resource/plugins/workbuddy/*. The
// dashboard frontend (ported from the tracker plugin) derives its API base
// from the page URL and calls:
//
//	GET /usage                 -> dashboard HTML page
//	GET /stats[/initial|/trends|/groups] /requests /costs /prices
//	    /preferences /exchange-rate -> read-only statistics API
//
// Writes are intentionally NOT exposed here (see handleUsageStatsManagement).
// Returns ok=false when the path does not belong to the statistics dashboard.
func handleUsageStatsResource(sub string, query url.Values) (pluginapi.ManagementResponse, bool) {
	if sub == "/usage" || sub == "/usage/" {
		return mgmtHTMLResponse(usagestats.DashboardHTML()), true
	}
	if !usageStatsReadAPIPath(sub) {
		return pluginapi.ManagementResponse{}, false
	}
	usageStatsMu.RLock()
	store := usageStatsStore
	usageStatsMu.RUnlock()
	if store == nil {
		return usageStatsManagementResult(usagestats.QueryResult{
			Status:  http.StatusServiceUnavailable,
			Headers: jsonHeaders(),
			Body:    []byte(`{"error":"usage statistics is disabled or storage is not initialized"}`),
		}), true
	}
	// Read endpoints are always GET; the resource route is GET-only.
	result := store.HandleQuery(http.MethodGet, sub, query, nil, nil)
	return usageStatsManagementResult(result), true
}

// usageStatsReadAPIPath reports whether the relative resource path belongs to
// the read-only statistics API consumed by the dashboard frontend.
func usageStatsReadAPIPath(rel string) bool {
	switch {
	case rel == "/stats" || strings.HasPrefix(rel, "/stats/"),
		rel == "/requests" || strings.HasPrefix(rel, "/requests/"),
		rel == "/costs" || strings.HasPrefix(rel, "/costs/"),
		rel == "/prices",
		rel == "/preferences",
		rel == "/exchange-rate":
		return true
	}
	return false
}

// handleUsageStatsManagement dispatches the statistics WRITE endpoints under
// /v0/management/plugins/workbuddy/* (the dashboard calls these via
// managementBase). Returns ok=false when the path does not belong to the
// statistics API.
func handleUsageStatsManagement(method, path string, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, bool) {
	prefix := loadedManagementBasePath() + "/plugins/" + providerName
	if !strings.HasPrefix(path, prefix) {
		return pluginapi.ManagementResponse{}, false
	}
	rel := strings.TrimPrefix(path, prefix)
	if !usageStatsWriteAPIPath(method, rel) {
		return pluginapi.ManagementResponse{}, false
	}
	usageStatsMu.RLock()
	store := usageStatsStore
	usageStatsMu.RUnlock()
	if store == nil {
		return usageStatsManagementResult(usagestats.QueryResult{
			Status:  http.StatusServiceUnavailable,
			Headers: jsonHeaders(),
			Body:    []byte(`{"error":"usage statistics is disabled or storage is not initialized"}`),
		}), true
	}
	result := store.HandleQuery(method, rel, req.Query, req.Body, req.Headers)
	return usageStatsManagementResult(result), true
}

// usageStatsWriteAPIPath reports whether the relative management path is a
// statistics write endpoint (mutating or binary transfer).
func usageStatsWriteAPIPath(method, rel string) bool {
	switch {
	case method == http.MethodPut && rel == "/prices",
		method == http.MethodPost && rel == "/prices/sync",
		method == http.MethodPost && rel == "/reset",
		method == http.MethodGet && rel == "/backup",
		method == http.MethodPost && rel == "/restore":
		return true
	}
	return false
}

// usageStatsManagementAuthRequired reports whether a statistics request is a
// mutating operation that must pass the plugin management auth/rate-limit
// gate when management_key is configured. POST endpoints are already covered
// by the gate's method check; this adds the non-POST mutators (PUT /prices).
func usageStatsManagementAuthRequired(req pluginapi.ManagementRequest) bool {
	prefix := loadedManagementBasePath() + "/plugins/" + providerName
	path := strings.TrimRight(req.Path, "/")
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rel := strings.TrimPrefix(path, prefix)
	return req.Method == http.MethodPut && rel == "/prices"
}

func jsonHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	return h
}

func logWarnf(format string, args ...any) {
	// WorkBuddy has no structured logger in the main package; mirror the
	// ported subpackage convention (stderr) so failures are visible in the
	// host log without introducing a logging dependency.
	fmt.Fprintf(os.Stderr, "[workbuddy] "+format+"\n", args...)
}
