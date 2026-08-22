package usagestats

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// usageFeedSourceServiceAddress marks records produced by the workbuddy
// plugin (the "source" dimension shown in the dashboard filters).
const usageFeedSourceServiceAddress = "https://www.codebuddy.cn"

// RecordFeedNDJSON parses one NDJSON line from the shared usage feed written
// by the workbuddy plugin and records it into the store. The line shape is
// owned here (not by the producer) so the parsing contract lives with the
// consumer:
//
//	{"timestamp":"...","latency_ms":123,"source":"workbuddy","auth_index":"...",
//	 "provider":"workbuddy","model":"...","alias":"...","auth_type":"oauth",
//	 "executor_type":"workbuddy","failed":false,"status_code":200,
//	 "session_key":"execution:abc123def","reasoning_effort":"high","ttft_ns":850000000,
//	 "tokens":{"input_tokens":..,"output_tokens":..,"reasoning_tokens":..,
//	           "cached_tokens":..,"cache_read_tokens":..,
//	           "cache_creation_tokens":..,"total_tokens":..}}
func (s *Store) RecordFeedNDJSON(line string) error {
	if s == nil {
		return fmt.Errorf("store is not initialized")
	}
	usage, err := decodeFeedLine(line)
	if err != nil {
		return err
	}
	return s.Record(usage)
}

func decodeFeedLine(line string) (normalizedUsage, error) {
	var raw struct {
		Timestamp        string `json:"timestamp"`
		LatencyMS        int64  `json:"latency_ms"`
		Source           string `json:"source"`
		AuthIndex        string `json:"auth_index"`
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		Alias            string `json:"alias"`
		AuthType         string `json:"auth_type"`
		ExecutorType     string `json:"executor_type"`
		Failed           bool   `json:"failed"`
		StatusCode       int    `json:"status_code"`
		SessionKey       string `json:"session_key"`
		ReasoningEffort  string `json:"reasoning_effort"`
		TTFTNS           int64  `json:"ttft_ns"`
		Tokens           struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			ReasoningTokens     int64 `json:"reasoning_tokens"`
			CachedTokens        int64 `json:"cached_tokens"`
			CacheReadTokens     int64 `json:"cache_read_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return normalizedUsage{}, fmt.Errorf("decode feed line: %w", err)
	}
	requestedAt := time.Now().UTC()
	if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil && !ts.IsZero() {
		requestedAt = ts.UTC()
	}
	latencyNS := uint64(0)
	if raw.LatencyMS > 0 {
		latencyNS = uint64(raw.LatencyMS) * uint64(time.Millisecond)
	}
	provider := normalizeDimension(strings.TrimSpace(raw.Provider))
	if provider == "" {
		provider = "unknown"
	}
	executorType := normalizeDimension(strings.TrimSpace(raw.ExecutorType))
	if executorType == "" {
		executorType = provider
	}
	authType := normalizeDimension(strings.TrimSpace(raw.AuthType))
	if authType == "" {
		authType = "oauth"
	}
	source := normalizeDimension(strings.TrimSpace(raw.Source))
	if source == "" {
		source = usageFeedSourceServiceAddress
	}
	t := raw.Tokens
	counters := NewCounters(
		t.InputTokens, t.OutputTokens, t.ReasoningTokens,
		t.CachedTokens, t.CacheReadTokens, t.CacheCreationTokens,
		t.TotalTokens,
	)
	usage := NewUsage(
		provider, executorType, strings.TrimSpace(raw.Model),
		strings.TrimSpace(raw.Alias), source, authType,
		strings.TrimSpace(raw.AuthIndex), requestedAt, latencyNS,
		raw.Failed, raw.StatusCode, counters,
	)
	// Feed-only dimensions the producer populates: session_key (the same
	// per-conversation key scheduler.pick used to pin the account so the
	// dashboard's 会话 column surfaces whether rows come from the same
	// stickiness-bound conversation), reasoning_effort (the value actually
	// sent upstream) and ttft_ns (time-to-first-token).
	usage.Dimensions.SessionKey = normalizeDimension(strings.TrimSpace(raw.SessionKey))
	usage.Dimensions.ReasoningEffort = normalizeDimension(strings.TrimSpace(raw.ReasoningEffort))
	usage.TTFTNS = positiveUint(raw.TTFTNS)
	return usage, nil
}
