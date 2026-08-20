package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestConfigureUsageFeedParsesConfigYAML(t *testing.T) {
	usageFeedMu.Lock()
	usageFeedEnabled = true
	usageFeedPath = ""
	usageFeedMu.Unlock()
	defer func() {
		usageFeedMu.Lock()
		usageFeedEnabled = true
		usageFeedPath = ""
		usageFeedMu.Unlock()
	}()

	dir := t.TempDir()
	// The host serializes config_yaml as base64 (ConfigYAML []byte over the
	// RPC wire); pass a []byte value so json.Marshal mimics the real transport.
	raw, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("usage_feed_enabled: true\nusage_feed_path: \"" + filepath.Join(dir, "feed.ndjson") + "\"\n"),
	})
	configureUsageFeed(raw)
	usageFeedMu.RLock()
	enabled := usageFeedEnabled
	path := usageFeedPath
	usageFeedMu.RUnlock()
	if !enabled {
		t.Fatal("feed should be enabled")
	}
	if path != filepath.Join(dir, "feed.ndjson") {
		t.Fatalf("feed path = %q", path)
	}

	// Disabled case.
	raw, _ = json.Marshal(map[string]any{
		"config_yaml": []byte("usage_feed_enabled: false\n"),
	})
	configureUsageFeed(raw)
	usageFeedMu.RLock()
	enabled = usageFeedEnabled
	usageFeedMu.RUnlock()
	if enabled {
		t.Fatal("feed should be disabled")
	}
}

func TestRecordUsageFeedAppendsNDJSON(t *testing.T) {
	dir := t.TempDir()
	feedPath := filepath.Join(dir, "feed.ndjson")
	usageFeedMu.Lock()
	usageFeedEnabled = true
	usageFeedPath = feedPath
	usageFeedMu.Unlock()
	defer func() {
		usageFeedMu.Lock()
		usageFeedEnabled = true
		usageFeedPath = ""
		usageFeedMu.Unlock()
	}()

	started := time.Now().Add(-3 * time.Second)
	detail := usage.Detail{
		InputTokens:     100,
		OutputTokens:    200,
		ReasoningTokens: 50,
		TotalTokens:     350,
	}
	recordUsageFeed("alias-m", "deepseek-v4", "u-1", started, detail, false, 200, "high", 850_000_000, "account-bob")
	recordUsageFeed("alias-m", "deepseek-v4", "u-1", started.Add(time.Second), detail, true, 502, "", 0, "account-alice")

	raw, err := os.ReadFile(feedPath)
	if err != nil {
		t.Fatalf("read feed: %v", err)
	}
	lines := splitLines(string(raw))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var rec struct {
		Timestamp        string `json:"timestamp"`
		Source           string `json:"source"`
		AuthIndex        string `json:"auth_index"`
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		Failed           bool   `json:"failed"`
		ServiceTier      string `json:"service_tier"`
		ReasoningEffort  string `json:"reasoning_effort"`
		TTFTNS           int64  `json:"ttft_ns"`
		Tokens           struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode line 0: %v", err)
	}
	if rec.Source != "workbuddy" || rec.Provider != providerName || rec.Model != "deepseek-v4" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.AuthIndex != "u-1" {
		t.Fatalf("auth_index = %q", rec.AuthIndex)
	}
	if rec.Tokens.TotalTokens != 350 {
		t.Fatalf("total_tokens = %d", rec.Tokens.TotalTokens)
	}
	if rec.Failed {
		t.Fatal("line 0 should be success")
	}
	if rec.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", rec.ReasoningEffort)
	}
	if rec.ServiceTier != "account-bob" {
		t.Fatalf("service_tier = %q, want account-bob", rec.ServiceTier)
	}
	if rec.TTFTNS != 850_000_000 {
		t.Fatalf("ttft_ns = %d, want 850000000", rec.TTFTNS)
	}
	// Line 2: failed request.
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("decode line 1: %v", err)
	}
	if !rec.Failed {
		t.Fatal("line 1 should be failed")
	}
	if rec.ReasoningEffort != "" {
		t.Fatalf("line 1 reasoning_effort = %q, want empty", rec.ReasoningEffort)
	}
	if rec.ServiceTier != "account-alice" {
		t.Fatalf("line 1 service_tier = %q, want account-alice", rec.ServiceTier)
	}
	if rec.TTFTNS != 0 {
		t.Fatalf("line 1 ttft_ns = %d, want 0", rec.TTFTNS)
	}
	// Timestamp format must be parseable RFC3339Nano (the tracker imports it).
	if _, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err != nil {
		t.Fatalf("timestamp %q not RFC3339Nano: %v", rec.Timestamp, err)
	}
}

func TestAppendUsageFeedLineRotation(t *testing.T) {
	dir := t.TempDir()
	feedPath := filepath.Join(dir, "feed.ndjson")

	oldCap := maxUsageFeedBytes
	maxUsageFeedBytes = 64 // tiny cap for the test
	defer func() { maxUsageFeedBytes = oldCap }()

	// Exceed the cap: a 100-byte line triggers truncation on the next append.
	bigLine := string(make([]byte, 100)) // 100 NUL bytes; only the size matters
	if err := os.WriteFile(feedPath, []byte(bigLine), 0o644); err != nil {
		t.Fatal(err)
	}
	appendUsageFeedLine(feedPath, []byte("{\"a\":1}\n"))
	raw, err := os.ReadFile(feedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\"a\":1}\n" {
		t.Fatalf("feed content after rotation = %q", raw)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
