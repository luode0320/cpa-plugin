// session_auth.go implements per-conversation account routing for workbuddy.
//
// scheduler_mode=session spreads conversations across the available workbuddy
// accounts: the SAME conversation is pinned to the SAME account for a bounded
// window (sessionStickinessTTL, default 1h), while DIFFERENT conversations are
// assigned to different accounts (unbound-account first, then round-robin).
//
// Session identity comes from the scheduler pick request Options, which the
// host fills before every pick (see sdk/cliproxy/session.Enrich in
// CLIProxyAPI): Options.Metadata carries the host-derived/execution session
// identity, Options.Headers carries explicit client session headers. When no
// session signal is present (rare), routing falls back to the panel-selected
// account via pickActiveAuth — identical behavior to scheduler_mode=credits.
package main

import (
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Session metadata keys written by the host (sdk/cliproxy/executor/types.go).
// Kept as local constants instead of importing the SDK executor package so the
// plugin never depends on a specific CLIProxyAPI version for routing keys.
const (
	executionSessionIDMetadataKey = "execution_session_id"
	derivedSessionIDMetadataKey   = "derived_session_id"
)

// sessionStickinessTTL bounds how long one conversation keeps its account.
// After expiry the binding is released and the next pick re-assigns (variable
// so tests can shrink it; default 1h per product requirement).
var sessionStickinessTTL = time.Hour

// sessionPruneInterval bounds how often expired bindings are swept.
const sessionPruneInterval = 5 * time.Minute

// sessionAuthBinding pins one conversation to one account until ExpiresAt.
type sessionAuthBinding struct {
	AuthID    string
	ExpiresAt time.Time
}

var (
	// sessionAuthMu guards sessionAuthBindings and sessionRR.
	sessionAuthMu sync.RWMutex
	// sessionAuthBindings maps a session key (see extractSessionKey) to its
	// pinned account. Entries are created on first pick of a conversation and
	// removed by the pruner after expiry.
	sessionAuthBindings = make(map[string]sessionAuthBinding)
	// sessionRR is a monotonically increasing counter used to spread new
	// conversations across accounts when every usable account already has
	// bindings.
	sessionRR uint64
)

func init() {
	go func() {
		ticker := time.NewTicker(sessionPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			pruneSessionBindings()
		}
	}()
}

// normalizeSessionID validates and trims a raw session identifier, mirroring
// the host's session.NormalizeExplicitID contract: opaque printable values up
// to 256 bytes; control-bearing or oversized values are rejected.
func normalizeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return raw
}

// sessionHeaderValue returns the first normalized value of a case-insensitive
// header. Only printable non-empty values are accepted.
func sessionHeaderValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if id := normalizeSessionID(value); id != "" {
				return id
			}
		}
	}
	return ""
}

// extractSessionKey derives a stable conversation key from a scheduler pick
// request, using only information the host exposes to scheduler plugins.
//
// Priority order (strongest signal first):
//  1. explicit execution session metadata (e.g. Codex websocket call id)
//  2. explicit client session headers (Claude Code / Codex / Responses / OpenCode)
//  3. host-derived session identity (stable hash of the conversation root)
//
// The host's session.Enrich runs before scheduling, so Options.Metadata is
// already populated for requests without explicit session signals, and
// explicit signals are NOT re-derived (metadata stays empty in that case) —
// hence headers are checked before the derived identity.
//
// Returns "" when no session signal is present; callers then fall back to the
// panel-selected account.
func extractSessionKey(req pluginapi.SchedulerPickRequest) string {
	if v, ok := req.Options.Metadata[executionSessionIDMetadataKey].(string); ok {
		if id := normalizeSessionID(v); id != "" {
			return "execution:" + id
		}
	}
	headers := req.Options.Headers
	for _, header := range []string{
		"X-Claude-Code-Session-Id", "Session-Id", "Session_id",
		"X-Session-ID", "X-Session-Affinity", "X-Client-Request-Id",
	} {
		if sid := sessionHeaderValue(headers, header); sid != "" {
			return headerSessionPrefix(header) + sid
		}
	}
	if v, ok := req.Options.Metadata[derivedSessionIDMetadataKey].(string); ok {
		if id := normalizeSessionID(v); id != "" {
			return "derived:" + id
		}
	}
	return ""
}

// headerSessionPrefix namespaces explicit session headers so different client
// types never collide on the same opaque id.
func headerSessionPrefix(header string) string {
	switch header {
	case "X-Claude-Code-Session-Id":
		return "claude:"
	case "Session-Id", "Session_id":
		return "codex:"
	case "X-Session-ID":
		return "header:"
	case "X-Session-Affinity":
		return "affinity:"
	case "X-Client-Request-Id":
		return "clientreq:"
	default:
		return "header:"
	}
}

// pickSessionAuth selects the account for one conversation.
//
// sessionKey != "" → sticky session routing:
//   - a fresh, still-usable binding is reused unchanged (1h stickiness);
//   - a stale binding (expired) or a binding whose account became
//     disabled/exhausted is re-assigned;
//   - new assignments prefer accounts with no live bindings, then round-robin
//     across all usable accounts;
//   - when every account is disabled/exhausted, the current pin is kept if the
//     account still exists, else the first candidate.
//
// sessionKey == "" → fall back to the panel-selected account (same behavior as
// scheduler_mode=credits).
func pickSessionAuth(sessionKey string, candidates []activeAuthCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	if sessionKey == "" {
		return pickActiveAuth(candidates)
	}

	live := make(map[string]struct{}, len(candidates))
	usable := make([]string, 0, len(candidates))
	usableSet := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		live[c.ID] = struct{}{}
		if !c.Disabled && !c.Exhausted {
			usable = append(usable, c.ID)
			usableSet[c.ID] = struct{}{}
		}
	}

	now := time.Now()
	sessionAuthMu.Lock()
	defer sessionAuthMu.Unlock()

	// Sticky hit: binding fresh AND its account still usable.
	if b, ok := sessionAuthBindings[sessionKey]; ok {
		if _, isLive := live[b.AuthID]; isLive {
			if _, isUsable := usableSet[b.AuthID]; isUsable && now.Before(b.ExpiresAt) {
				return b.AuthID
			}
		}
	}

	if len(usable) == 0 {
		// Everything disabled/exhausted — keep current pin if the account still
		// exists, else the first candidate (mirrors pickActiveAuth fallback).
		if b, ok := sessionAuthBindings[sessionKey]; ok {
			if _, isLive := live[b.AuthID]; isLive {
				return b.AuthID
			}
		}
		return candidates[0].ID
	}

	// Fresh assignment: prefer accounts with no live bindings (spreads
	// conversations across accounts), then round-robin.
	boundCounts := make(map[string]int, len(usable))
	for _, b := range sessionAuthBindings {
		if now.Before(b.ExpiresAt) {
			boundCounts[b.AuthID]++
		}
	}
	next := ""
	for _, id := range usable {
		if boundCounts[id] == 0 {
			next = id
			break
		}
	}
	if next == "" {
		next = usable[int(sessionRR%uint64(len(usable)))]
		sessionRR++
	}

	sessionAuthBindings[sessionKey] = sessionAuthBinding{AuthID: next, ExpiresAt: now.Add(sessionStickinessTTL)}
	return next
}

// pruneSessionBindings removes expired bindings. Called by the background
// pruner; also safe to call from tests directly.
func pruneSessionBindings() {
	now := time.Now()
	sessionAuthMu.Lock()
	defer sessionAuthMu.Unlock()
	for key, b := range sessionAuthBindings {
		if !now.Before(b.ExpiresAt) {
			delete(sessionAuthBindings, key)
		}
	}
}

// clearSessionBindings wipes all session bindings. Test helper; never called
// in production paths.
func clearSessionBindings() {
	sessionAuthMu.Lock()
	sessionAuthBindings = make(map[string]sessionAuthBinding)
	sessionRR = 0
	sessionAuthMu.Unlock()
}
