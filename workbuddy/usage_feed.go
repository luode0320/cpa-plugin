// usage_feed.go implements the shared NDJSON usage feed for the standalone
// token-usage-tracker plugin.
//
// Why a file feed instead of merging the tracker into workbuddy (v0.8.8) or
// sharing a bbolt database:
//   - The host's UsagePlugin broadcast never fires for plugin executors, so a
//     pure UsagePlugin consumer cannot observe workbuddy requests.
//   - bbolt holds an exclusive flock for the lifetime of a writer, so two
//     long-lived processes (workbuddy recording + tracker dashboard) cannot
//     share one DB file.
//
// The feed is a simple append-only NDJSON file at a well-known path
// (<CLIProxyAPI root>/data/token-usage-feed.ndjson by default). workbuddy is
// the only producer (O_APPEND, one line per completed request, opened and
// closed per line so rotation is safe); token-usage-tracker is the only
// consumer (tracks its read offset, imports into its own bbolt store). No
// locks, no cross-process coordination, replayable after restart.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	// defaultUsageFeedName is the file name inside the CLIProxyAPI "data" dir.
	defaultUsageFeedName = "token-usage-feed.ndjson"
)

// maxUsageFeedBytes bounds feed growth; on exceeding it the producer
// truncates (the consumer detects truncation by offset > file size and
// resets to 0, so nothing is double-imported). A var (not const) so tests can
// shrink it.
var maxUsageFeedBytes int64 = 128 << 20

var (
	usageFeedMu      sync.RWMutex
	usageFeedEnabled = true
	usageFeedPath    = ""
)

// configureUsageFeed parses the usage_feed_* fields from config_yaml. Failures
// are non-fatal: the plugin keeps serving chat, only the feed is disabled.
func configureUsageFeed(raw []byte) {
	enabled := true
	dataPath := ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "usage_feed_enabled:"):
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_feed_enabled:")), "\"'")
					enabled = v == "true" || v == "1" || v == "yes" || v == "on"
				case strings.HasPrefix(line, "usage_feed_path:"):
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_feed_path:")), "\"'")
					if v != "" {
						dataPath = v
					}
				}
			}
		}
	}
	if dataPath == "" {
		dataPath = defaultUsageFeedPath()
	}
	usageFeedMu.Lock()
	usageFeedEnabled = enabled
	usageFeedPath = dataPath
	usageFeedMu.Unlock()
}

// defaultUsageFeedPath resolves the feed location next to the discovered
// CLIProxyAPI root ("<root>/data/token-usage-feed.ndjson"), matching the
// token-usage-tracker plugin's default so both sides agree out of the box.
func defaultUsageFeedPath() string {
	if root, ok := cliProxyRootFromWorkingDir(); ok {
		return filepath.Join(root, "data", defaultUsageFeedName)
	}
	if root, ok := cliProxyRootFromExecutable(); ok {
		return filepath.Join(root, "data", defaultUsageFeedName)
	}
	return filepath.Join("data", defaultUsageFeedName)
}

// cliProxyRootFromWorkingDir returns the first ancestor of the working
// directory that contains a "plugins" directory (the CPA install root).
func cliProxyRootFromWorkingDir() (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	absolute, err := filepath.Abs(wd)
	if err != nil {
		return "", false
	}
	return cliProxyRootFromDir(absolute)
}

// cliProxyRootFromExecutable checks the directory containing the CPA binary.
func cliProxyRootFromExecutable() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return cliProxyRootFromDir(filepath.Dir(exe))
}

func cliProxyRootFromDir(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for {
		info, err := os.Stat(filepath.Join(dir, "plugins"))
		if err == nil && info.IsDir() && filepath.Dir(dir) != dir {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// recordUsageFeed appends one NDJSON line for a completed request. Called from
// publishUsage inside its async goroutine, so a slow filesystem never stalls
// the executor. The record shape mirrors forwardUsageToCPAMP's payload so the
// tracker plugin and CPAMP see identical data.
func recordUsageFeed(alias, model, authUID string, started time.Time, detail usage.Detail, failed bool, statusCode int) {
	usageFeedMu.RLock()
	enabled := usageFeedEnabled
	path := usageFeedPath
	usageFeedMu.RUnlock()
	if !enabled || path == "" {
		return
	}
	ts := started
	if ts.IsZero() {
		ts = time.Now()
	}
	latencyMs := int64(0)
	if !started.IsZero() {
		if d := time.Since(started).Milliseconds(); d > 0 {
			latencyMs = d
		}
	}
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	record := map[string]any{
		"timestamp":     ts.UTC().Format(time.RFC3339Nano),
		"latency_ms":    latencyMs,
		"source":        "workbuddy",
		"auth_index":    strings.TrimSpace(authUID),
		"provider":      providerName,
		"model":         model,
		"alias":         alias,
		"endpoint":      "POST /v1/chat/completions",
		"auth_type":     "oauth",
		"executor_type": "workbuddy",
		"failed":        failed,
		"status_code":   statusCode,
		"tokens": map[string]any{
			"input_tokens":          detail.InputTokens,
			"output_tokens":         detail.OutputTokens,
			"reasoning_tokens":      detail.ReasoningTokens,
			"cached_tokens":         detail.CachedTokens,
			"cache_read_tokens":     detail.CacheReadTokens,
			"cache_creation_tokens": detail.CacheCreationTokens,
			"total_tokens":          total,
		},
	}
	body, err := json.Marshal(record)
	if err != nil {
		usageFeedWarnf("marshal: %v", err)
		return
	}
	body = append(body, '\n')
	appendUsageFeedLine(path, body)
}

// appendUsageFeedLine opens the feed with O_APPEND and writes one line.
// Opens per line (not a held handle) so the consumer can rotate/truncate and
// so a rename-based rotation by future tooling stays safe. Rotation uses
// os.Truncate BEFORE the O_APPEND open: on Windows, truncating through an
// O_APPEND handle fails with access denied (FILE_APPEND_DATA != WRITE_DATA).
func appendUsageFeedLine(path string, line []byte) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			usageFeedWarnf("mkdir %s: %v", dir, err)
			return
		}
	}
	// Rotation guard: truncate a feed that outgrew the cap. The consumer
	// treats "file smaller than my offset" as a rotation signal and re-reads
	// from the start, so truncation never causes missed records.
	if info, err := os.Stat(path); err == nil && info.Size() > maxUsageFeedBytes {
		if err := os.Truncate(path, 0); err != nil {
			usageFeedWarnf("truncate %s: %v", path, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		usageFeedWarnf("open %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		usageFeedWarnf("write %s: %v", path, err)
	}
}

func usageFeedWarnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[workbuddy] usage feed: "+format+"\n", args...)
}
