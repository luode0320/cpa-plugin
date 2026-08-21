// keepalive.go implements proactive daily token refresh for workbuddy auths.
//
// Motivation: upstream (codebuddy.cn / workbuddy.ai) periodically kills
// Keycloak offline sessions; once that happens the stored refresh token is
// rejected with 12153 "Offline user session not found" and every billing /
// chat call for the account returns a 401 HTML page (surfacing in the panel
// as "parse failed: invalid character '<'"). Refreshing the access token
// daily keeps the offline session alive, the same way a real client would.
//
// Design:
//   - Runs on the existing schedulerLoop at 22:00 local (keepaliveHours is
//     separate from checkinHours so the two cadences can evolve independently).
//   - Iterates all workbuddy auths via host.auth.list/get, calls
//     {realm-base}/v2/plugin/auth/token/refresh with X-Refresh-Token via
//     the host HTTP bridge (host.http.do).
//   - On success the auth file is persisted via host.auth.save (host watcher
//     reloads it; in-memory host token stays stale until then, which is fine
//     since the old access token typically remains valid for a long time).
//   - On 12153 (session dead) the auth is flagged disabled + note prefixed
//     "[SESSION-DEAD]" so it stops receiving traffic until manual re-login.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// keepaliveHours is the daily refresh schedule (local time). Kept separate
// from checkinHours so the two cadences can evolve independently.
var keepaliveHours = []int{22}

// keepaliveAuto gates the daily refresh. Default true; configurable via
// plugin config key "token_keepalive" (config_yaml line "token_keepalive: false").
var (
	keepaliveAuto   = true
	keepaliveAutoMu sync.RWMutex
)

func keepaliveEnabled() bool {
	keepaliveAutoMu.RLock()
	defer keepaliveAutoMu.RUnlock()
	return keepaliveAuto
}

// sessionDeadMarkers identify a server-side revoked offline session.
var sessionDeadMarkers = []string{
	"Offline user session not found",
	"12153",
}

func isSessionDeadError(msg string) bool {
	for _, m := range sessionDeadMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// refreshCall posts to the upstream token/refresh endpoint and returns the
// decoded envelope data plus the raw body (raw needed for error classification
// — doJSON collapses 4xx bodies into "http_error: upstream NNN" and drops the
// business code, e.g. 12153 "Offline user session not found").
//
// v0.8.0: routed via host.http.do so request-log captures the call.
func refreshCall(sa *storedAuth) (json.RawMessage, []byte, int, error) {
	url := endpointTokenRefreshFor(sa)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	commonHeaders(req)
	req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", providerName)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, nil, 0, err
	}
	raw := resp.Body
	if resp.StatusCode >= 400 {
		return nil, raw, resp.StatusCode, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, raw, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, raw, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, raw, resp.StatusCode, nil
}

// refreshOneAuth refreshes the access token for a single workbuddy auth and
// persists the result. Returns a short status string for logging/tests.
func refreshOneAuth(authIndex, authID string) (string, error) {
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return "error", fmt.Errorf("get auth: %w", err)
	}
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return "skipped", fmt.Errorf("no refreshToken")
	}

	data, raw, status, err := refreshCall(sa)
	if err != nil {
		msg := string(raw)
		if status == 401 && isSessionDeadError(msg) {
			// Upstream killed the offline session: refresh token is dead and
			// every API call for this account will 401. Flag disabled so the
			// scheduler stops routing traffic to it until manual re-login.
			if derr := markSessionDead(authIndex, authID, sa); derr != nil {
				return "session-dead", fmt.Errorf("session dead; flag failed: %v", derr)
			}
			return "session-dead", fmt.Errorf("session dead (12153): flagged disabled")
		}
		return "failed", fmt.Errorf("refresh rejected (HTTP %d): %s", status, truncateRedacted(err.Error(), 120))
	}

	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "failed", fmt.Errorf("refresh_failed: no accessToken in response")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = preserveExpiry(
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second).Unix(),
		sa.Auth.ExpiresAt,
	)
	if err := persistAuthTokens(authIndex, sa); err != nil {
		return "error", fmt.Errorf("persist: %w", err)
	}
	return "refreshed", nil
}

// persistAuthTokens writes the updated credential back through the host API.
// The host's file watcher reloads it; we deliberately do NOT dual-write the
// physical path (same rule as hostAuthPersist).
//
// Bug A fix: the previous json.Marshal(sa) serialized only the nested
// auth/account struct and OVERWROTE the whole file, silently dropping every
// top-level metadata key (disabled/note/type/provider/logo/manual_disable).
// Result: each 22:00 keepalive token refresh re-enabled accounts the user had
// disabled via the panel. We now merge the refreshed nested storage into the
// existing physical doc so all top-level fields survive.
func persistAuthTokens(authIndex string, sa *storedAuth) error {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return err
	}
	name := phys.Name
	if name == "" {
		name = authFileNameFor(sa)
	}
	// Keep the existing top-level doc as-is; replace only auth/account.
	raw, err := mergeAuthDoc(phys.JSON, sa)
	if err != nil {
		return err
	}
	return hostAuthSaveJSON(name, raw)
}

// markSessionDead flags an auth disabled via the host's standard `disabled`
// field (CPA natively skips disabled auths in scheduling). The note records
// the reason so the panel can surface "session dead, re-login required"
// without needing a custom [SESSION-DEAD] marker.
func markSessionDead(authIndex, authID string, sa *storedAuth) error {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return err
	}
	if phys.Disabled {
		return nil // already disabled; nothing to do
	}
	var doc map[string]any
	if err := json.Unmarshal(phys.JSON, &doc); err != nil {
		return err
	}
	doc["disabled"] = true
	doc["note"] = "Session dead (12153): re-login required"
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	name := phys.Name
	if name == "" {
		name = authFileNameFor(sa)
	}
	return hostAuthSaveJSON(name, raw)
}

// keepaliveSummary is one row of the daily run, surfaced via the
// management route for observability.
type keepaliveSummary struct {
	When    time.Time      `json:"when"`
	Results []keepaliveRow `json:"results"`
}

type keepaliveRow struct {
	AuthIndex string `json:"auth_index"`
	Nickname  string `json:"nickname,omitempty"`
	Region    string `json:"region"`
	Status    string `json:"status"` // refreshed | skipped | failed | session-dead | error
	Detail    string `json:"detail,omitempty"`
}

var (
	lastKeepaliveMu sync.RWMutex
	lastKeepalive   *keepaliveSummary
)

func recordKeepalive(s *keepaliveSummary) {
	lastKeepaliveMu.Lock()
	lastKeepalive = s
	lastKeepaliveMu.Unlock()
}

func getLastKeepalive() *keepaliveSummary {
	lastKeepaliveMu.RLock()
	defer lastKeepaliveMu.RUnlock()
	return lastKeepalive
}

// runTokenKeepalive refreshes every workbuddy auth once. Returns the summary.
func runTokenKeepalive() *keepaliveSummary {
	sum := &keepaliveSummary{When: time.Now()}
	if !keepaliveEnabled() {
		return sum
	}
	files, err := hostAuthList()
	if err != nil {
		sum.Results = append(sum.Results, keepaliveRow{Status: "error", Detail: err.Error()})
		recordKeepalive(sum)
		return sum
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := keepaliveRow{AuthIndex: f.AuthIndex}
			// Cheap pre-read for nickname/region; refreshOneAuth re-reads anyway
			// but the row should be populated even when refresh errors early.
			if sa, err := hostAuthGet(f.AuthIndex); err == nil {
				row.Nickname = sa.Account.Nickname
				row.Region = accountRegion(sa)
			}
			status, err := refreshOneAuth(f.AuthIndex, f.ID)
			row.Status = status
			if err != nil {
				row.Detail = truncateRedacted(err.Error(), 200)
			}
			mu.Lock()
			sum.Results = append(sum.Results, row)
			mu.Unlock()
		}()
	}
	wg.Wait()
	recordKeepalive(sum)
	return sum
}

// nextKeepaliveTime mirrors nextCheckinTime but for keepaliveHours.
func nextKeepaliveTime(now time.Time) time.Time {
	var earliest time.Time
	for _, h := range keepaliveHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// shouldRunKeepaliveNow reports whether the current local time is within
// one hour after any scheduled keepalive hour today. Used by schedulerLoop
// to fire keepalive on the same tick as checkin when the schedules coincide.
func shouldRunKeepaliveNow(now time.Time) bool {
	for _, h := range keepaliveHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		// Within [t, t+1h) window.
		if !now.Before(t) && now.Before(t.Add(time.Hour)) {
			return true
		}
	}
	return false
}

// handleKeepaliveNow triggers a manual refresh (all accounts, or one when the
// body carries auth_index). Manual runs ignore the token_keepalive toggle —
// the toggle gates only the 22:00 auto-run.
func handleKeepaliveNow(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		sum := runTokenKeepalive()
		return map[string]any{"when": sum.When, "results": sum.Results}
	}
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	row := keepaliveRow{AuthIndex: authIndex, Nickname: sa.Account.Nickname, Region: accountRegion(sa)}
	row.Status, err = refreshOneAuth(authIndex, "")
	if err != nil {
		row.Detail = truncateRedacted(err.Error(), 200)
	}
	return map[string]any{"when": time.Now(), "results": []keepaliveRow{row}}
}

// handleKeepaliveStatus returns the last scheduled-run summary plus config.
func handleKeepaliveStatus() map[string]any {
	return map[string]any{
		"enabled":  keepaliveEnabled(),
		"schedule": keepaliveHours,
		"last_run": getLastKeepalive(),
	}
}
