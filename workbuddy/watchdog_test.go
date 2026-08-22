package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetPreserve wipes the in-memory preserve set so each test starts clean
// and never leaks marks into later tests.
func resetPreserve(t *testing.T) {
	t.Helper()
	preserveSetMu.Lock()
	preserveSet = make(map[string]struct{})
	preserveSetMu.Unlock()
	t.Cleanup(func() {
		preserveSetMu.Lock()
		preserveSet = make(map[string]struct{})
		preserveSetMu.Unlock()
	})
}

// storeCredits seeds accountCache with a non-exhausted (or exhausted) credits
// entry for one auth ID and auto-clears it on test end. (Moved here from the
// deleted pool_test.go — scheduler routing tests still need it.)
func storeCredits(t *testing.T, id string, remain, used, total int64) {
	t.Helper()
	accountCache.Store(id, &accountCacheEntry{credits: &creditsSummary{
		TotalRemain: remain,
		TotalUsed:   used,
		TotalSize:   total,
	}})
	t.Cleanup(func() { accountCache.Delete(id) })
}

// TestPreserveShouldFlip is the watchdog's decision table: an account is
// parked in the preserve set when remain < threshold (strictly below) and
// released when credits recover to >= threshold. No-op states must report
// changed=false so the tick skips disk writes.
func TestPreserveShouldFlip(t *testing.T) {
	cases := []struct {
		name        string
		remain      int64
		threshold   int64
		preserved   bool
		wantShould  bool
		wantChanged bool
	}{
		{"below threshold, free", 49, 50, false, true, true},
		{"below threshold, already preserved", 10, 50, true, true, false},
		{"exactly at threshold, free", 50, 50, false, false, false},
		{"above threshold, free", 500, 50, false, false, false},
		{"recovered to threshold, preserved", 50, 50, true, false, true},
		{"recovered above threshold, preserved", 120, 50, true, false, true},
		{"zero threshold, zero remain", 0, 0, false, false, false},
		{"zero threshold, negative remain", -1, 0, false, true, true},
		{"empty credits unknown, free", -1, 50, false, true, true}, // unknown is treated as below threshold (conservative)
	}
	for _, c := range cases {
		should, changed := preserveShouldFlip(c.remain, c.threshold, c.preserved)
		if should != c.wantShould || changed != c.wantChanged {
			t.Errorf("%s: preserveShouldFlip(%d,%d,%v) = (%v,%v), want (%v,%v)",
				c.name, c.remain, c.threshold, c.preserved, should, changed, c.wantShould, c.wantChanged)
		}
	}
}

// TestPreserveConfigDefaultsAndSet covers the runtime-tunable knobs: defaults
// match the documented contract (50 credits / 10m / enabled) and
// setPreserveConfig applies values under their own locks.
func TestPreserveConfigDefaultsAndSet(t *testing.T) {
	if got := preserveThreshold(); got != preserveThresholdDefault {
		t.Fatalf("default threshold = %d, want %d", got, preserveThresholdDefault)
	}
	if got := preserveWatchdogInterval(); got != preserveWatchdogIntervalDefault {
		t.Fatalf("default interval = %v, want %v", got, preserveWatchdogIntervalDefault)
	}
	if got := preserveWatchdogEnabled(); !got {
		t.Fatal("default watchdog should be enabled")
	}
	// Invalid values are ignored: negative threshold and zero interval must
	// not clobber the current configuration.
	setPreserveConfig(-5, 0, false)
	if got := preserveThreshold(); got != preserveThresholdDefault {
		t.Fatalf("negative threshold must be ignored, got %d", got)
	}
	if got := preserveWatchdogInterval(); got != preserveWatchdogIntervalDefault {
		t.Fatalf("zero interval must be ignored, got %v", got)
	}
	if preserveWatchdogEnabled() {
		t.Fatal("enabled=false should apply")
	}
	// Valid values apply.
	setPreserveConfig(30, 5*time.Minute, true)
	if got := preserveThreshold(); got != 30 {
		t.Fatalf("threshold = %d, want 30", got)
	}
	if got := preserveWatchdogInterval(); got != 5*time.Minute {
		t.Fatalf("interval = %v, want 5m", got)
	}
	if !preserveWatchdogEnabled() {
		t.Fatal("enabled=true should apply")
	}
	// Restore defaults for later tests.
	setPreserveConfig(preserveThresholdDefault, preserveWatchdogIntervalDefault, preserveWatchdogEnabledDefault)
}

// TestParsePreserveFromAuthJSON covers the disk-format contract: the top-level
// preserve boolean on the physical auth file.
func TestParsePreserveFromAuthJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty doc", `{}`, false},
		{"preserve true", `{"preserve":true}`, true},
		{"preserve false", `{"preserve":false}`, false},
		{"other fields only (legacy pool ignored)", `{"pool":"priority","disabled":true}`, false},
		{"malformed json", `not-json{`, false},
	}
	for _, c := range cases {
		if got := parsePreserveFromAuthJSON([]byte(c.raw)); got != c.want {
			t.Errorf("%s: parsePreserveFromAuthJSON(%s) = %v, want %v", c.name, c.raw, got, c.want)
		}
	}
}

// TestPreserveSetBasic covers the in-memory set helpers used by the watchdog
// and scheduler: put / clear / is / snapshot.
func TestPreserveSetBasic(t *testing.T) {
	resetPreserve(t)
	if isPreserve("wb-1") {
		t.Fatal("unmarked account should not be preserved")
	}
	if isPreserve("") {
		t.Fatal("empty authID must never be preserved")
	}
	preserveSetPut("wb-1")
	if !isPreserve("wb-1") {
		t.Fatal("preserveSetPut should mark the account")
	}
	preserveSetPut("wb-1") // idempotent
	if len(preserveSnapshot()) != 1 {
		t.Fatalf("snapshot size = %d, want 1", len(preserveSnapshot()))
	}
	preserveSetClear("wb-1")
	if isPreserve("wb-1") {
		t.Fatal("preserveSetClear should unmark the account")
	}
	preserveSetClear("wb-missing") // no-op; must not panic
}

// TestSchedulerPick_PreserveFiltered is the core routing contract: a preserve
// account is excluded even when it is the panel-selected active account — the
// picker must route to a healthy non-preserved account instead.
func TestSchedulerPick_PreserveFiltered(t *testing.T) {
	resetActiveAuth(t)
	resetPreserve(t)
	resetFailover(t)
	storeCredits(t, "wb-pres", 30, 0, 30)
	storeCredits(t, "wb-norm", 500, 0, 500)
	preserveSetPut("wb-pres")
	setActiveAuthID("wb-pres") // panel selects the preserved account
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pres", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-norm" {
		t.Fatalf("want preserved account skipped → wb-norm, got %+v", resp)
	}
}

// TestSchedulerPick_AllPreserved_KeepsFullList: when EVERY workbuddy account
// is preserved the preserve filter keeps the full list so routing falls back
// to the current pin (mirrors the all-cooldown fallback) instead of deferring
// to the built-in scheduler.
func TestSchedulerPick_AllPreserved_KeepsFullList(t *testing.T) {
	resetActiveAuth(t)
	resetPreserve(t)
	resetFailover(t)
	storeCredits(t, "wb-a", 10, 0, 10)
	storeCredits(t, "wb-b", 20, 0, 20)
	preserveSetPut("wb-a")
	preserveSetPut("wb-b")
	setActiveAuthID("wb-b")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-a", Provider: providerName},
			{ID: "wb-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled {
		t.Fatalf("all-preserved fleet should still fall back to the pin, got %+v", resp)
	}
	if resp.AuthID != "wb-b" {
		t.Fatalf("want fallback to panel pin wb-b, got %+v", resp)
	}
}

// TestEvictSessionBindingsForAuth is the session-migration side effect: when
// the watchdog parks an account, every conversation pinned to it must be
// unbound so the next request re-picks a healthy account.
func TestEvictSessionBindingsForAuth(t *testing.T) {
	resetSessionRouting(t)
	// Pin two conversations to wb-a and one to wb-b.
	sessionAuthMu.Lock()
	sessionAuthBindings["execution:call-1"] = sessionAuthBinding{AuthID: "wb-a", ExpiresAt: time.Now().Add(time.Hour)}
	sessionAuthBindings["execution:call-2"] = sessionAuthBinding{AuthID: "wb-b", ExpiresAt: time.Now().Add(time.Hour)}
	sessionAuthBindings["execution:call-3"] = sessionAuthBinding{AuthID: "wb-a", ExpiresAt: time.Now().Add(time.Hour)}
	sessionAuthMu.Unlock()

	if got := evictSessionBindingsForAuth("wb-a"); got != 2 {
		t.Fatalf("evict wb-a returned %d, want 2", got)
	}
	if got := evictSessionBindingsForAuth("wb-a"); got != 0 {
		t.Fatalf("second evict should be a no-op, got %d", got)
	}
	if got := evictSessionBindingsForAuth(""); got != 0 {
		t.Fatalf("empty authID should evict nothing, got %d", got)
	}
	sessionAuthMu.RLock()
	remaining := len(sessionAuthBindings)
	stillB := sessionAuthBindings["execution:call-2"]
	sessionAuthMu.RUnlock()
	if remaining != 1 {
		t.Fatalf("remaining bindings = %d, want 1", remaining)
	}
	if stillB.AuthID != "wb-b" {
		t.Fatalf("call-2 should stay on wb-b, got %q", stillB.AuthID)
	}
}
