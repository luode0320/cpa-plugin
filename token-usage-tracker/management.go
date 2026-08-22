// management.go implements the token-usage-tracker management API: route
// registration + request dispatch for the embedded dashboard.
//
// Route contract (CPA host routing is EXACT match on method+path — a request
// only reaches HandleManagement when its full path was declared in
// managementRegistration, otherwise the host answers 404):
//
//	Resource routes (GET, unauthenticated browser UI):
//	  /v0/resource/plugins/token-usage-tracker/usage          -> dashboard HTML page
//	  /v0/resource/plugins/token-usage-tracker/stats[/initial|/trends|/groups]
//	  /v0/resource/plugins/token-usage-tracker/requests /costs /prices
//	  /v0/resource/plugins/token-usage-tracker/preferences /exchange-rate
//	Management routes (write API, behind the management_key gate):
//	  PUT  /v0/management/plugins/token-usage-tracker/prices
//	  POST /v0/management/plugins/token-usage-tracker/prices/sync
//	  POST /v0/management/plugins/token-usage-tracker/reset
//	  GET  /v0/management/plugins/token-usage-tracker/backup
//	  POST /v0/management/plugins/token-usage-tracker/restore
package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	usagestats "github.com/luode0320/cpa-workbuddy-plugin/token-usage-tracker/usage_stats"
)

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

// managementRegistration declares every path the dashboard frontend calls.
// Missing any entry here = host-level 404 (exact-match routing), which is
// exactly the bug class that plagued the v0.8.8 workbuddy merge.
func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodPut, Path: base + "/prices", Description: "Save the model price book (JSON body)."},
			{Method: http.MethodPost, Path: base + "/prices/sync", Description: "Sync model prices from models.dev."},
			{Method: http.MethodPost, Path: base + "/reset", Description: "Reset all statistics (body: {\"confirm\":\"reset\"})."},
			{Method: http.MethodGet, Path: base + "/backup", Description: "Download the statistics database (binary)."},
			{Method: http.MethodPost, Path: base + "/restore", Description: "Restore a statistics database backup (X-Confirm-Restore: replace)."},
		},
		Resources: []resourceRoute{
			{Path: "/usage", Menu: "Token 用量", Description: "Token usage dashboard: per-model/account consumption, trends, requests, costs (records produced by the workbuddy plugin via the shared usage feed)."},
			{Path: "/stats", Description: "Statistics summary."},
			{Path: "/stats/initial", Description: "Initial dashboard payload."},
			{Path: "/stats/trends", Description: "Token usage trend series."},
			{Path: "/stats/groups", Description: "Per-dimension group stats."},
			{Path: "/requests", Description: "Request log page."},
			{Path: "/costs", Description: "Estimated cost page."},
			{Path: "/prices", Description: "Model price book (GET)."},
			{Path: "/preferences", Description: "Dashboard preferences (GET/save)."},
			{Path: "/exchange-rate", Description: "Currency exchange rate."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated reads).
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		if resp, ok := serveStatsResource(sub, req.Query); ok {
			return okEnvelope(resp)
		}
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}

	// Management write API: plugin-layer auth + rate limit. Every declared
	// management route mutates or exports data, so all of them pass the gate
	// when management_key is configured (empty key = host middleware only).
	base := loadedManagementBasePath() + "/plugins/" + providerName
	if !strings.HasPrefix(path, base) {
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
	if status, msg := checkManagementAuth(req); status != 0 {
		ip := managementClientIP(req)
		if !allowManagementRequest(ip) {
			return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
				"error": "rate limit exceeded, try again later",
			}))
		}
		return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
	}
	rel := strings.TrimPrefix(path, base)
	if !statsWriteAPIPath(req.Method, rel) {
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
	if !usageStatsOpen() {
		return okEnvelope(mgmtJSONResponse(http.StatusServiceUnavailable, map[string]any{
			"error": "usage statistics is disabled or storage is not initialized",
		}))
	}
	// Best-effort feed sync so writes/queries always see the freshest data.
	syncUsageFeed()
	result := usageStatsQuery(req.Method, rel, req.Query, req.Body, req.Headers)
	return okEnvelope(managementResult(result))
}

// serveStatsResource serves the dashboard page and the read-only statistics
// API. Returns ok=false when the path is not a statistics resource.
func serveStatsResource(sub string, query url.Values) (pluginapi.ManagementResponse, bool) {
	if sub == "/usage" || sub == "/usage/" {
		return mgmtHTMLResponse(usagestats.DashboardHTML()), true
	}
	if !statsReadAPIPath(sub) {
		return pluginapi.ManagementResponse{}, false
	}
	if !usageStatsOpen() {
		return mgmtJSONResponse(http.StatusServiceUnavailable, map[string]any{
			"error": "usage statistics is disabled or storage is not initialized",
		}), true
	}
	// Best-effort feed sync so the dashboard shows near-real-time data even
	// between poll ticks.
	syncUsageFeed()
	result := usageStatsQuery(http.MethodGet, sub, query, nil, nil)
	return managementResult(result), true
}

// statsReadAPIPath reports whether the relative resource path belongs to the
// read-only statistics API consumed by the dashboard frontend. Keep in sync
// with the /stats* /requests /costs /prices /preferences /exchange-rate
// entries in managementRegistration().
func statsReadAPIPath(rel string) bool {
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

// statsWriteAPIPath reports whether the relative management path is a
// statistics write endpoint (mutating or binary transfer).
func statsWriteAPIPath(method, rel string) bool {
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

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit
// -----------------------------------------------------------------------------

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	trackerCfgMu.RLock()
	defer trackerCfgMu.RUnlock()
	return trackerCfg.ManagementKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // plugin-layer auth disabled; rely on host middleware
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
}

// -----------------------------------------------------------------------------
// Response helpers
// -----------------------------------------------------------------------------

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

func managementResult(result usagestats.QueryResult) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: result.Status,
		Headers:    result.Headers,
		Body:       result.Body,
	}
}
