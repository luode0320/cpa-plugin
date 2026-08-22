package main

import (
	"testing"
	"time"
)

// resetFailover clears all failover state, restores the enabled flag and the
// default tiers so each test starts from a clean slate.
func resetFailover(t *testing.T) {
	t.Helper()
	clearFailoverStates()
	oldEnabled := failoverActive()
	oldTiers := append([]time.Duration(nil), failoverTiers...)
	t.Cleanup(func() {
		clearFailoverStates()
		setFailoverEnabled(oldEnabled)
		failoverTiers = oldTiers
	})
	setFailoverEnabled(true)
}

// assertCooldownNear verifies the account's cooldown deadline is roughly the
// given tier away from now (allows small test-run latency).
func assertCooldownNear(t *testing.T, authID string, tier time.Duration) {
	t.Helper()
	count, until, ok := failoverStateSnapshot(authID)
	if !ok {
		t.Fatalf("no failover state for %q", authID)
	}
	if count < 1 {
		t.Fatalf("failover count for %q = %d, want >= 1", authID, count)
	}
	remain := time.Until(until)
	if remain <= tier-5*time.Second || remain > tier+5*time.Second {
		t.Fatalf("cooldown remain for %q = %v, want ~%v (count %d)", authID, remain, tier, count)
	}
}

func TestFailoverCooldownFor_Tiers(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 0},
		{1, time.Minute},
		{2, 3 * time.Minute},
		{3, 10 * time.Minute},
		{4, 10 * time.Minute}, // capped
		{99, 10 * time.Minute},
	}
	for _, tc := range cases {
		if got := failoverCooldownFor(tc.count); got != tc.want {
			t.Fatalf("failoverCooldownFor(%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestRecordAccountFailure_BackoffTiers(t *testing.T) {
	resetFailover(t)
	// 1st failure → 1 minute.
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", time.Minute)
	// 2nd failure → 3 minutes.
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", 3*time.Minute)
	// 3rd failure → 10 minutes.
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", 10*time.Minute)
	// 4th failure → still 10 minutes (capped).
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", 10*time.Minute)
}

func TestRecordAccountFailure_ResetOnSuccess(t *testing.T) {
	resetFailover(t)
	recordAccountFailure("acc-1", 429, "rate limit")
	if !isAccountCoolingDown("acc-1") {
		t.Fatal("account should be cooling down after a failure")
	}
	resetAccountFailover("acc-1")
	count, until, ok := failoverStateSnapshot("acc-1")
	if !ok || count != 0 {
		t.Fatalf("after reset: count = %d, ok = %v; want count 0", count, ok)
	}
	if !until.IsZero() {
		t.Fatalf("after reset: cooldownUntil = %v, want zero", until)
	}
	if isAccountCoolingDown("acc-1") {
		t.Fatal("account should not be cooling down after reset")
	}
}

func TestRecordAccountFailure_Business4xxExcluded(t *testing.T) {
	resetFailover(t)
	// Business 400 does NOT count.
	if recordAccountFailure("acc-1", 400, "bad request: unknown model") {
		t.Fatal("business 400 must not be counted as account failure")
	}
	if recordAccountFailure("acc-1", 400, "bad request: unknown model") {
		t.Fatal("business 400 must not be counted as account failure")
	}
	count, _, _ := failoverStateSnapshot("acc-1")
	if count != 0 {
		t.Fatalf("count = %d after two 400s, want 0", count)
	}
	// 429 counts on top of nothing; then another 429 → tier 2.
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", time.Minute)
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", 3*time.Minute)
}

func TestRecordAccountFailure_TransportAnd5xxCounted(t *testing.T) {
	resetFailover(t)
	if !recordAccountFailure("acc-1", 0, "connection refused") {
		t.Fatal("transport-level failure (status 0) must be counted")
	}
	if !recordAccountFailure("acc-2", 502, "upstream gateway error") {
		t.Fatal("5xx must be counted")
	}
	if recordAccountFailure("acc-3", 200, "ok") {
		t.Fatal("success (200) must not be counted")
	}
	if recordAccountFailure("acc-3", 204, "") {
		t.Fatal("success (204) must not be counted")
	}
}

func TestIsAccountFailure_Classification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"transport failure", 0, "dial tcp: connection refused", true},
		{"429 rate limit", 429, "rate limit exceeded", true},
		{"429 empty body", 429, "", true},
		{"402 payment required", 402, "payment required", true},
		{"402 credit marker in body", 200, `{"error":"insufficient credit"}`, true},
		{"5xx", 503, "service unavailable", true},
		{"5xx empty body", 500, "", true},
		{"business 400", 400, "bad request", false},
		{"business 404", 404, "not found", false},
		{"success", 200, "ok", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAccountFailure(tc.status, tc.body); got != tc.want {
				t.Fatalf("isAccountFailure(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestFailoverCooldownExpiry(t *testing.T) {
	resetFailover(t)
	recordAccountFailure("acc-1", 429, "rate limit")
	if !isAccountCoolingDown("acc-1") {
		t.Fatal("account should be cooling down right after failure")
	}
	// Simulate the cooldown window elapsing: the counter persists but the
	// account becomes routable again.
	setFailoverCooldownUntil("acc-1", time.Now().Add(-time.Second))
	if isAccountCoolingDown("acc-1") {
		t.Fatal("account should be routable after cooldown expires")
	}
	// A new failure re-enters cooldown at tier 2 (count preserved).
	recordAccountFailure("acc-1", 429, "rate limit")
	assertCooldownNear(t, "acc-1", 3*time.Minute)
}

func TestFailoverDisabled(t *testing.T) {
	resetFailover(t)
	setFailoverEnabled(false)
	if recordAccountFailure("acc-1", 429, "rate limit") {
		t.Fatal("recordAccountFailure must be a no-op when disabled")
	}
	if isAccountCoolingDown("acc-1") {
		t.Fatal("no cooldown when disabled")
	}
	resetAccountFailover("acc-1") // must not panic
}
