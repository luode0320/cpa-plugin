// accountFailover.go implements per-account exponential backoff for routing.
//
// When an upstream account fails (HTTP 429 / 402 / 5xx or a transport-level
// error), the account enters a temporary cooldown window. While cooling down,
// every new request routed by the scheduler skips that account, so sessions
// fail over to a healthy account instead of piling more failures onto the
// same exhausted one.
//
// Backoff tiers follow the product requirement (1 / 3 / 10 minutes):
//
//	1st failure   -> cooldown 1 minute
//	2nd failure   -> cooldown 3 minutes
//	3rd failure   -> cooldown 10 minutes
//	4+  failures  -> cooldown stays 10 minutes (capped)
//
// A successful request resets the counter and lifts the cooldown immediately.
// Cooldown state is in-memory only: no auth files, no DB writes; a process
// restart clears everything. The mechanism can be disabled wholesale via
// plugin config `account_failover: false`.
package main

import (
	"net/http"
	"sync"
	"time"
)

// failoverTiers are the exponential backoff steps, indexed by consecutive
// failure count - 1. Any count beyond the last tier keeps the last duration
// (capped at 10 minutes).
var failoverTiers = []time.Duration{
	1 * time.Minute,
	3 * time.Minute,
	10 * time.Minute,
}

// failoverPruneInterval bounds how often stale (zero-count) failover states
// are swept from memory. Aligned with the session-binding pruner.
const failoverPruneInterval = 5 * time.Minute

var (
	// failoverEnabled gates the whole mechanism. Default true; set false via
	// plugin config account_failover: false to restore pre-failover behavior.
	failoverEnabled   = true
	failoverEnabledMu sync.RWMutex

	// failoverMu guards failoverStates.
	failoverMu     sync.Mutex
	failoverStates = make(map[string]*authFailoverState)
)

// authFailoverState tracks consecutive failures for one account.
type authFailoverState struct {
	count         int       // consecutive failures; reset to 0 on success
	cooldownUntil time.Time // zero means not cooling down
}

func init() {
	go func() {
		ticker := time.NewTicker(failoverPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			pruneFailoverStates()
		}
	}()
}

// failoverActive reports whether the failover mechanism is enabled.
func failoverActive() bool {
	failoverEnabledMu.RLock()
	defer failoverEnabledMu.RUnlock()
	return failoverEnabled
}

// setFailoverEnabled toggles the whole mechanism (config / tests).
func setFailoverEnabled(on bool) {
	failoverEnabledMu.Lock()
	failoverEnabled = on
	failoverEnabledMu.Unlock()
}

// failoverCooldownFor returns the cooldown duration for a consecutive failure
// count, capped at the last tier. count <= 0 yields zero.
func failoverCooldownFor(count int) time.Duration {
	if count <= 0 {
		return 0
	}
	if count > len(failoverTiers) {
		count = len(failoverTiers)
	}
	return failoverTiers[count-1]
}

// isAccountFailure reports whether an upstream response counts as an account
// failure for failover purposes. Transport-level failures (status 0), 5xx,
// rate limiting (429 / body markers), hard credit errors and account-level
// 4xx (401/403/404/405 — wrong token, missing permission, endpoint or
// method not available for THIS account) all count. Business 4xx (400) is
// excluded: it reflects the request, not the account.
func isAccountFailure(status int, body string) bool {
	if status == 0 || status >= 500 {
		return true
	}
	if isSoftRateLimit(status, body) || isHardCreditError(status, body) {
		return true
	}
	return isAccountLevel4xx(status)
}

// isAccountLevel4xx reports whether a 4xx status reflects an account-level
// problem (the credential/endpoint on this account is wrong) rather than a
// request-level problem. 401/403/404/405 mean the upstream rejected access
// for THIS account: token expired, no permission, route missing, or method
// not allowed. Retrying with a different account has a real chance of
// succeeding. 400 is intentionally excluded — it almost always means the
// request body was malformed by us and would fail identically on every
// other account.
func isAccountLevel4xx(status int) bool {
	switch status {
	case http.StatusUnauthorized, // 401
		http.StatusForbidden,        // 403
		http.StatusNotFound,         // 404
		http.StatusMethodNotAllowed: // 405
		return true
	}
	return false
}

// recordAccountFailure increments the consecutive-failure counter for the
// account and extends its cooldown window using the backoff tier for the new
// count. Returns true when the failure was counted (i.e. isAccountFailure).
// Callers are expected to key on the same auth.ID the scheduler uses.
func recordAccountFailure(authID string, status int, body string) bool {
	if !failoverActive() || !isAccountFailure(status, body) {
		return false
	}
	now := time.Now()
	failoverMu.Lock()
	defer failoverMu.Unlock()
	st := failoverStates[authID]
	if st == nil {
		st = &authFailoverState{}
		failoverStates[authID] = st
	}
	st.count++
	st.cooldownUntil = now.Add(failoverCooldownFor(st.count))
	return true
}

// isAccountCoolingDown reports whether the account is currently inside its
// cooldown window and should be skipped by routing.
func isAccountCoolingDown(authID string) bool {
	if !failoverActive() {
		return false
	}
	failoverMu.Lock()
	defer failoverMu.Unlock()
	st := failoverStates[authID]
	if st == nil {
		return false
	}
	return time.Now().Before(st.cooldownUntil)
}

// resetAccountFailover clears the failure counter and cooldown after a
// successful request. Call it on every upstream success.
func resetAccountFailover(authID string) {
	if !failoverActive() {
		return
	}
	failoverMu.Lock()
	defer failoverMu.Unlock()
	st := failoverStates[authID]
	if st == nil {
		return
	}
	st.count = 0
	st.cooldownUntil = time.Time{}
}

// pruneFailoverStates removes zero-count states (successfully reset, no
// longer cooling down). Failed-but-not-cooling-down states are kept: their
// counter persists until a success resets it.
func pruneFailoverStates() {
	failoverMu.Lock()
	defer failoverMu.Unlock()
	for k, st := range failoverStates {
		if st.count == 0 {
			delete(failoverStates, k)
		}
	}
}

// failoverStateSnapshot returns a copy of the account's failover state.
// Test helper; never called in production paths.
func failoverStateSnapshot(authID string) (count int, cooldownUntil time.Time, ok bool) {
	failoverMu.Lock()
	defer failoverMu.Unlock()
	st, ok := failoverStates[authID]
	if !ok {
		return 0, time.Time{}, false
	}
	return st.count, st.cooldownUntil, true
}

// clearFailoverStates wipes all failover state. Test helper; never called in
// production paths.
func clearFailoverStates() {
	failoverMu.Lock()
	failoverStates = make(map[string]*authFailoverState)
	failoverMu.Unlock()
}

// setFailoverCooldownUntil overrides the cooldown deadline for an account.
// Test helper (lets tests simulate cooldown expiry); never called in
// production paths.
func setFailoverCooldownUntil(authID string, until time.Time) {
	failoverMu.Lock()
	defer failoverMu.Unlock()
	st := failoverStates[authID]
	if st == nil {
		st = &authFailoverState{}
		failoverStates[authID] = st
	}
	st.cooldownUntil = until
}
