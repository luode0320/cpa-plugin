package usagestats

// bridge_smoke_test.go exercises the exact call path the workbuddy bridge
// (usage_stats_bridge.go) uses — Open -> NewCounters/NewUsage -> Record ->
// HandleQuery — to catch wiring regressions early. It is a black-box test of
// the exported API layer and never touches the host RPC surface.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestBridgeSmoke(t *testing.T) {
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

	now := time.Now().UTC().Truncate(time.Second)
	ok := NewUsage("workbuddy", "workbuddy", "gpt-4o", "gpt-4o", "https://www.codebuddy.cn", "oauth", "uid-1", now.Add(-2*time.Minute), 1_500_000_000, false, 200, NewCounters(1000, 500, 0, 200, 150, 50, 0))
	if err := store.Record(ok); err != nil {
		t.Fatalf("Record ok: %v", err)
	}
	fail := NewUsage("workbuddy", "workbuddy", "gpt-4o-mini", "gpt-4o-mini", "https://www.codebuddy.cn", "oauth", "uid-2", now.Add(-time.Minute), 300_000_000, true, 429, NewCounters(50, 0, 0, 0, 0, 0, 0))
	if err := store.Record(fail); err != nil {
		t.Fatalf("Record fail: %v", err)
	}

	// GET /stats (the dashboard's primary read)
	q := url.Values{"range": []string{"24h"}}
	res := store.HandleQuery(http.MethodGet, "/stats", q, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/stats status=%d body=%s", res.Status, res.Body)
	}
	var stats StatsResponse
	if err := json.Unmarshal(res.Body, &stats); err != nil {
		t.Fatalf("/stats decode: %v", err)
	}
	if stats.Summary.Requests != 2 {
		t.Errorf("/stats Summary.Requests=%d want 2", stats.Summary.Requests)
	}
	if stats.Summary.TotalTokens != 1500+50 {
		t.Errorf("/stats Summary.TotalTokens=%d want 1550", stats.Summary.TotalTokens)
	}
	if stats.Summary.FailedRequests != 1 {
		t.Errorf("/stats Summary.FailedRequests=%d want 1", stats.Summary.FailedRequests)
	}

	// GET /requests
	res = store.HandleQuery(http.MethodGet, "/requests", url.Values{"limit": []string{"10"}}, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/requests status=%d body=%s", res.Status, res.Body)
	}
	var page RequestPage
	if err := json.Unmarshal(res.Body, &page); err != nil {
		t.Fatalf("/requests decode: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("/requests Total=%d want 2", page.Total)
	}

	// GET /stats/trends
	res = store.HandleQuery(http.MethodGet, "/stats/trends", url.Values{"range": []string{"24h"}}, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/stats/trends status=%d body=%s", res.Status, res.Body)
	}

	// preferences save + read (GET with save=1, then plain GET)
	res = store.HandleQuery(http.MethodGet, "/preferences", url.Values{"save": []string{"1"}, "request_page_size": []string{"50"}, "dimension_page_size": []string{"100"}}, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/preferences save status=%d body=%s", res.Status, res.Body)
	}
	res = store.HandleQuery(http.MethodGet, "/preferences", nil, nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("/preferences read status=%d body=%s", res.Status, res.Body)
	}
	var prefs DashboardPreferences
	if err := json.Unmarshal(res.Body, &prefs); err != nil {
		t.Fatalf("/preferences decode: %v", err)
	}
	if prefs.RequestPageSize != 50 {
		t.Errorf("prefs.RequestPageSize=%d want 50", prefs.RequestPageSize)
	}

	// unknown route must 404 (so the workbuddy switch can fall through)
	res = store.HandleQuery(http.MethodGet, "/accounts", nil, nil, nil)
	if res.Status != http.StatusNotFound {
		t.Errorf("unknown route status=%d want 404", res.Status)
	}

	// method not allowed on GET /reset
	res = store.HandleQuery(http.MethodGet, "/reset", nil, nil, nil)
	if res.Status != http.StatusMethodNotAllowed {
		t.Errorf("GET /reset status=%d want 405", res.Status)
	}
}
