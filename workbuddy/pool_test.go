package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetAuthPool wipes the in-memory pool map so each test starts clean and
// never leaks marks into later tests.
func resetAuthPool(t *testing.T) {
	t.Helper()
	authPoolMu.Lock()
	authPool = make(map[string]string)
	authPoolMu.Unlock()
	t.Cleanup(func() {
		authPoolMu.Lock()
		authPool = make(map[string]string)
		authPoolMu.Unlock()
	})
}

// storeCredits seeds accountCache with a non-exhausted (or exhausted) credits
// entry for one auth ID and auto-clears it on test end.
func storeCredits(t *testing.T, id string, remain, used, total int64) {
	t.Helper()
	accountCache.Store(id, &accountCacheEntry{credits: &creditsSummary{
		TotalRemain: remain,
		TotalUsed:   used,
		TotalSize:   total,
	}})
	t.Cleanup(func() { accountCache.Delete(id) })
}

func TestAuthPool_DefaultAndSet(t *testing.T) {
	resetAuthPool(t)
	// Every account is default unless explicitly marked.
	if poolFor("wb-1") != poolDefault {
		t.Fatalf("unmarked account should be %q, got %q", poolDefault, poolFor("wb-1"))
	}
	if poolFor("") != poolDefault {
		t.Fatalf("empty authID should normalize to %q", poolDefault)
	}
	setPool("wb-1", poolPriority)
	if poolFor("wb-1") != poolPriority {
		t.Fatalf("want %q, got %q", poolPriority, poolFor("wb-1"))
	}
	setPool("wb-1", poolFallback)
	if poolFor("wb-1") != poolFallback {
		t.Fatalf("want %q, got %q", poolFallback, poolFor("wb-1"))
	}
	// Back to default removes the entry (implicit state, not stored).
	setPool("wb-1", poolDefault)
	if poolFor("wb-1") != poolDefault {
		t.Fatalf("reset to default failed: %q", poolFor("wb-1"))
	}
	if len(authPoolSnapshot()) != 0 {
		t.Fatalf("default marks must not be stored, got %v", authPoolSnapshot())
	}
	// Invalid pool names normalize to default and are not stored.
	setPool("wb-2", "bogus")
	if poolFor("wb-2") != poolDefault {
		t.Fatalf("invalid pool should normalize to default, got %q", poolFor("wb-2"))
	}
	clearPoolFor("wb-1") // no-op; must not panic
	if poolFor("wb-1") != poolDefault {
		t.Fatalf("cleared account should be default, got %q", poolFor("wb-1"))
	}
}

// TestParsePoolFromAuthJSON covers the disk-format contract, including the
// legacy v0.9.x `priority: true` boolean migration.
func TestParsePoolFromAuthJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty doc", `{}`, poolDefault},
		{"explicit default", `{"pool":"default"}`, poolDefault},
		{"priority", `{"pool":"priority"}`, poolPriority},
		{"fallback", `{"pool":"fallback"}`, poolFallback},
		{"unknown value", `{"pool":"weird"}`, poolDefault},
		{"legacy priority true", `{"priority":true}`, poolPriority},
		{"legacy priority false", `{"priority":false}`, poolDefault},
		// New field wins when both are present.
		{"pool wins over legacy", `{"priority":true,"pool":"fallback"}`, poolFallback},
	}
	for _, c := range cases {
		if got := parsePoolFromAuthJSON([]byte(c.raw)); got != c.want {
			t.Errorf("%s: parsePoolFromAuthJSON(%s) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

func TestAnyCandidateUsable(t *testing.T) {
	resetAuthPool(t)
	resetFailover(t)
	// Unknown credits + no cooldown → usable.
	storeCredits(t, "wb-live", 10, 0, 10)
	if !anyCandidateUsable([]pluginapi.SchedulerAuthCandidate{{ID: "wb-live"}}) {
		t.Fatal("live candidate should be usable")
	}
	// Exhausted credits → not usable.
	storeCredits(t, "wb-ex", 0, 100, 100)
	if anyCandidateUsable([]pluginapi.SchedulerAuthCandidate{{ID: "wb-ex"}}) {
		t.Fatal("exhausted candidate should not be usable")
	}
	// Mixed: one live member makes the bucket usable.
	if !anyCandidateUsable([]pluginapi.SchedulerAuthCandidate{{ID: "wb-ex"}, {ID: "wb-live"}}) {
		t.Fatal("bucket with one live member should be usable")
	}
	// Cooling-down candidate → not usable.
	recordAccountFailure("wb-cool", 429, "rate limited")
	if anyCandidateUsable([]pluginapi.SchedulerAuthCandidate{{ID: "wb-cool"}}) {
		t.Fatal("cooling-down candidate should not be usable")
	}
	// Empty bucket → not usable.
	if anyCandidateUsable(nil) {
		t.Fatal("empty bucket should not be usable")
	}
}

// TestSchedulerPick_ThreePool_PriorityOnly is the core requirement: as long as
// ≥1 priority account is alive, routing MUST stay inside the priority bucket —
// even when the panel-selected account is a default one.
func TestSchedulerPick_ThreePool_PriorityOnly(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-pri", 10, 0, 10)
	storeCredits(t, "wb-norm", 500, 0, 500)
	setPool("wb-pri", poolPriority)
	// Panel-selected account is the DEFAULT one — priority must override it.
	setActiveAuthID("wb-norm")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-pri" {
		t.Fatalf("want priority account wb-pri, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_SkipsExhaustedMember: inside the priority bucket
// the picker still skips exhausted members and picks a live one.
func TestSchedulerPick_ThreePool_SkipsExhaustedMember(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-pri-ex", 0, 100, 100) // exhausted
	storeCredits(t, "wb-pri-live", 10, 0, 10) // live
	storeCredits(t, "wb-norm", 500, 0, 500)
	setPool("wb-pri-ex", poolPriority)
	setPool("wb-pri-live", poolPriority)
	setActiveAuthID("wb-pri-ex")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri-ex", Provider: providerName},
			{ID: "wb-pri-live", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-pri-live" {
		t.Fatalf("want live priority account wb-pri-live, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_PriorExhausted_FallsToDefault: when every
// priority account is exhausted, traffic cascades to the default bucket.
func TestSchedulerPick_ThreePool_PriorExhausted_FallsToDefault(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-pri", 0, 100, 100) // exhausted
	storeCredits(t, "wb-norm", 300, 0, 300)
	setPool("wb-pri", poolPriority)
	setActiveAuthID("wb-pri")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-norm" {
		t.Fatalf("want cascade to default wb-norm, got %+v", resp)
	}
	if getActiveAuthID() != "wb-norm" {
		t.Fatalf("active should switch to wb-norm, got %q", getActiveAuthID())
	}
}

// TestSchedulerPick_ThreePool_PriorAndDefaultExhausted_FallsToFallback: when
// both the priority and the default bucket are exhausted, traffic cascades all
// the way down to the fallback bucket.
func TestSchedulerPick_ThreePool_PriorAndDefaultExhausted_FallsToFallback(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-pri", 0, 100, 100) // exhausted
	storeCredits(t, "wb-def", 0, 100, 100) // exhausted
	storeCredits(t, "wb-fb", 10, 0, 10)    // live fallback
	setPool("wb-pri", poolPriority)
	setPool("wb-fb", poolFallback)
	setActiveAuthID("wb-def")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri", Provider: providerName},
			{ID: "wb-def", Provider: providerName},
			{ID: "wb-fb", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-fb" {
		t.Fatalf("want cascade to fallback wb-fb, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_NoPriority_DefaultExhausted_FallsToFallback:
// with no priority accounts at all, the cascade starts at default; when the
// whole default bucket is exhausted it still reaches the fallback bucket.
func TestSchedulerPick_ThreePool_NoPriority_DefaultExhausted_FallsToFallback(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-def", 0, 100, 100) // exhausted
	storeCredits(t, "wb-fb", 10, 0, 10)    // live fallback
	setPool("wb-fb", poolFallback)
	setActiveAuthID("wb-def")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-def", Provider: providerName},
			{ID: "wb-fb", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-fb" {
		t.Fatalf("want cascade to fallback wb-fb, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_PriorDisabled_FallsToDefault: host-disabled
// priority accounts are filtered before bucketing, so only default accounts
// remain eligible.
func TestSchedulerPick_ThreePool_PriorDisabled_FallsToDefault(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-norm", 300, 0, 300)
	setPool("wb-pri-off", poolPriority)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri-off", Provider: providerName, Status: "disabled"},
			{ID: "wb-norm", Provider: providerName, Status: "active"},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-norm" {
		t.Fatalf("want cascade to default wb-norm, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_AllCoolingDown_NotLocked: when the whole
// priority bucket is cooling down (and every candidate is, so the pre-filter
// keeps the full list), the picker must not be locked to the priority bucket.
func TestSchedulerPick_ThreePool_AllCoolingDown_NotLocked(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	resetFailover(t)
	setPool("wb-pri", poolPriority)
	recordAccountFailure("wb-pri", 429, "rate limited")
	recordAccountFailure("wb-norm", 429, "rate limited")
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled {
		t.Fatalf("all cooling down should still be handled (fallback), got %+v", resp)
	}
	if resp.AuthID == "wb-pri" {
		t.Fatalf("priority bucket is fully cooling down, must not pick wb-pri, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_NoMarked_Normal: with no pool marks at all,
// routing behaves exactly as before (panel selection is honored).
func TestSchedulerPick_ThreePool_NoMarked_Normal(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	storeCredits(t, "wb-a", 10, 0, 10)
	storeCredits(t, "wb-b", 500, 0, 500)
	setActiveAuthID("wb-a")
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
	if !resp.Handled || resp.AuthID != "wb-a" {
		t.Fatalf("want panel selection wb-a, got %+v", resp)
	}
}

// TestSchedulerPick_ThreePool_SessionRebindsToPrior: in session mode, a
// conversation already pinned to a DEFAULT account must be re-assigned to the
// priority bucket the moment a priority account becomes available.
func TestSchedulerPick_ThreePool_SessionRebindsToPrior(t *testing.T) {
	resetActiveAuth(t)
	resetAuthPool(t)
	resetSessionRouting(t)
	restoreMode := setSchedulerMode(schedulerModeSession)
	t.Cleanup(restoreMode)
	storeCredits(t, "wb-pri", 10, 0, 10)
	storeCredits(t, "wb-norm", 500, 0, 500)
	// Simulate an existing conversation pinned to the default account.
	sessionKey := "execution:call-1"
	sessionAuthMu.Lock()
	sessionAuthBindings[sessionKey] = sessionAuthBinding{AuthID: "wb-norm", ExpiresAt: time.Now().Add(time.Hour)}
	sessionAuthMu.Unlock()
	// Priority account appears.
	setPool("wb-pri", poolPriority)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Options:  pluginapi.SchedulerOptions{Metadata: map[string]any{executionSessionIDMetadataKey: "call-1"}},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-pri", Provider: providerName},
			{ID: "wb-norm", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-pri" {
		t.Fatalf("want session rebound to priority wb-pri, got %+v", resp)
	}
	// The binding must now point at the priority account.
	sessionAuthMu.RLock()
	b := sessionAuthBindings[sessionKey]
	sessionAuthMu.RUnlock()
	if b.AuthID != "wb-pri" {
		t.Fatalf("binding should move to wb-pri, got %q", b.AuthID)
	}
}
