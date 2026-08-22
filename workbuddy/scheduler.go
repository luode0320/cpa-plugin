// scheduler.go implements the CPA scheduler.pick capability for workbuddy.
//
// Routing uses the panel-selected active account (region from that card's
// domain). When the selection is exhausted/disabled/missing, randomly switch
// to another non-exhausted workbuddy candidate. Non-workbuddy candidates are
// always deferred so the built-in scheduler handles them.
//
// scheduler_mode=session additionally enables per-conversation routing: each
// conversation is pinned to one account for up to 1h and conversations are
// spread across accounts (see session_auth.go).
package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Legacy config values kept for configure() compatibility; pick always uses
// panel active-auth selection now (not credit-max ranking).
const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
	schedulerModeSession = "session"
)

var (
	schedulerMode   = schedulerModeSession
	schedulerModeMu sync.RWMutex
)

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a workbuddy auth candidate based on the
// panel-selected active account. Non-workbuddy candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"     → plugin does NOT handle routing; defer everything to built-in.
//   - "credits" → plugin picks via panel-selected active account (sticky, with
//     fallback when that account becomes exhausted/disabled).
//   - "session" → per-conversation routing: same conversation sticks to one
//     account for up to 1h, different conversations spread across accounts;
//     requests without a session identity fall back to the panel-selected
//     account (same as credits).
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode: credits or session in plugin config.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// v0.6.31: actually honor the scheduler_mode toggle. Previously the config
	// was parsed but never read here, so "off" silently behaved like "credits".
	mode := loadedSchedulerMode()
	if mode != schedulerModeCredits && mode != schedulerModeSession {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect workbuddy candidates only. Accounts in failover cooldown are
	// skipped so new requests route to a healthy account instead — but only
	// when at least one healthy candidate remains. If EVERY workbuddy account
	// is cooling down, keep the full list so the pickers fall back to the
	// current pin (mirrors the all-exhausted fallback) instead of deferring.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	filtered := make([]pluginapi.SchedulerAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		if !isAccountCoolingDown(c.ID) {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) > 0 {
		wbCandidates = filtered
	}

	// Build thin view for active-auth picker.
	cands := make([]activeAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		_, exhausted := cachedCreditsScore(c.ID)
		cands = append(cands, activeAuthCandidate{
			ID:        c.ID,
			Disabled:  false, // already filtered
			Exhausted: exhausted,
		})
	}
	var picked string
	if mode == schedulerModeSession {
		picked = pickSessionAuth(extractSessionKey(req), cands)
	} else {
		picked = pickActiveAuth(cands)
	}
	if picked == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// cachedCreditsScore returns (remain, exhausted) from accountCache.
// remain is -1 when unknown; exhausted uses isCreditsExhausted.
// Key is auth.ID (same as SchedulerAuthCandidate.ID and activeAuthID).
func cachedCreditsScore(authID string) (int64, bool) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, false
	}
	return entry.credits.TotalRemain, isCreditsExhausted(entry.credits)
}
