package usagestats

// usage_record_test.go covers the UsagePlugin broadcast entry point
// (RecordUsageRecord): it must accept the canonical pluginapi.UsageRecord JSON
// that the CPA host delivers on usage.handle — the path that captures
// third-party api-provider requests outside the workbuddy NDJSON feed.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// sampleUsageRecord mirrors pluginapi.UsageRecord JSON as serialized by the
// CPA host (PascalCase field names, time.Duration as integer nanoseconds).
func sampleUsageRecord(provider, executorType, model, authID string, failed bool, statusCode int, input, output, total int64) []byte {
	raw, _ := json.Marshal(map[string]any{
		"Provider":        provider,
		"ExecutorType":    executorType,
		"Model":           model,
		"Alias":           model,
		"APIKey":          "",
		"AuthID":          authID,
		"AuthIndex":       authID,
		"AuthType":        "apikey",
		"Source":          "https://api.example.com/v1",
		"ReasoningEffort": "",
		"ServiceTier":     "",
		"Generate":        true,
		"RequestedAt":     time.Now().UTC().Add(-2 * time.Minute),
		"Latency":         int64(1_500_000_000),
		"TTFT":            int64(350_000_000),
		"Failed":          failed,
		"Failure": map[string]any{
			"StatusCode": statusCode,
			"Body":       "upstream rejected",
		},
		"Detail": map[string]any{
			"InputTokens":         input,
			"OutputTokens":        output,
			"ReasoningTokens":     int64(0),
			"CachedTokens":        int64(0),
			"CacheReadTokens":     int64(0),
			"CacheCreationTokens": int64(0),
			"TotalTokens":         total,
		},
	})
	return raw
}

func TestRecordUsageRecordAPIServiceProvider(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{
		DataPath:        filepath.Join(dir, "usage-stats.db"),
		RetentionDays:   365,
		FlushInterval:   100 * time.Millisecond,
		FlushMaxRecords: 10,
		SyncOnRecord:    true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// api-provider record delivered via the host UsagePlugin broadcast.
	raw := sampleUsageRecord("openai-compatible-provider", "openaicompatexecutor", "gpt-5.6-sol", "key-abc", false, 200, 1000, 500, 1500)
	if err := store.RecordUsageRecord(raw); err != nil {
		t.Fatalf("RecordUsageRecord: %v", err)
	}

	// Query the request log to confirm the record landed with the right model.
	res := store.HandleQuery(http.MethodGet, "/requests", url.Values{"limit": []string{"10"}}, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/requests status=%d body=%s", res.Status, res.Body)
	}
	var page RequestPage
	if err := json.Unmarshal(res.Body, &page); err != nil {
		t.Fatalf("/requests decode: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("/requests Total=%d want 1", page.Total)
	}
	item := page.Items[0]
	if item.Model != "gpt-5.6-sol" {
		t.Errorf("item.Model=%q want gpt-5.6-sol", item.Model)
	}
	if item.Provider != "openai-compatible-provider" {
		t.Errorf("item.Provider=%q want openai-compatible-provider", item.Provider)
	}
	if item.Counters.TotalTokens != 1500 {
		t.Errorf("item.Counters.TotalTokens=%d want 1500", item.Counters.TotalTokens)
	}
	if item.Counters.Requests != 1 {
		t.Errorf("item.Counters.Requests=%d want 1", item.Counters.Requests)
	}

	// A failed record must also land (dashboard failure column).
	rawFail := sampleUsageRecord("openai-compatible-provider", "openaicompatexecutor", "gpt-5.6-sol", "key-abc", true, 429, 50, 0, 50)
	if err := store.RecordUsageRecord(rawFail); err != nil {
		t.Fatalf("RecordUsageRecord failed: %v", err)
	}
	res = store.HandleQuery(http.MethodGet, "/stats", url.Values{"range": []string{"24h"}}, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/stats status=%d body=%s", res.Status, res.Body)
	}
	var stats StatsResponse
	if err := json.Unmarshal(res.Body, &stats); err != nil {
		t.Fatalf("/stats decode: %v", err)
	}
	if stats.Summary.Requests != 2 {
		t.Errorf("Summary.Requests=%d want 2", stats.Summary.Requests)
	}
	if stats.Summary.FailedRequests != 1 {
		t.Errorf("Summary.FailedRequests=%d want 1", stats.Summary.FailedRequests)
	}
}

func TestRecordUsageRecordNilStore(t *testing.T) {
	var store *Store
	if err := store.RecordUsageRecord([]byte(`{}`)); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestRecordUsageRecordMalformed(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Config{
		DataPath:        filepath.Join(dir, "usage-stats.db"),
		RetentionDays:   365,
		FlushInterval:   100 * time.Millisecond,
		FlushMaxRecords: 10,
		SyncOnRecord:    true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.RecordUsageRecord([]byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
