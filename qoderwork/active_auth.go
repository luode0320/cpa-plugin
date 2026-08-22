// active_auth.go tracks the panel-selected QoderWork account used for routing.
//
// Region is always CN for QoderWork —
// no per-request JWT iss decode. Default: first available candidate. When the
// active account is exhausted/disabled/missing, randomly switch to another
// non-exhausted candidate and remember the choice.
package main

import (
	"strings"
	"sync"
)

var (
	activeAuthID string
	activeAuthMu sync.RWMutex
)

func getActiveAuthID() string {
	activeAuthMu.RLock()
	defer activeAuthMu.RUnlock()
	return strings.TrimSpace(activeAuthID)
}

func setActiveAuthID(id string) {
	id = strings.TrimSpace(id)
	activeAuthMu.Lock()
	activeAuthID = id
	activeAuthMu.Unlock()
}

// clearActiveAuthIfMatch clears the selection when the given auth is removed.
func clearActiveAuthIfMatch(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	activeAuthMu.Lock()
	if activeAuthID == id {
		activeAuthID = ""
	}
	activeAuthMu.Unlock()
}

// activeAuthCandidate is a thin view used by pickActiveAuth.
type activeAuthCandidate struct {
	ID        string
	Disabled  bool
	Exhausted bool
}

// pickActiveAuth chooses which qoderwork auth to use from host candidates.
// The panel selection is sticky: it stays on the current account unless that
// account is no longer in the candidate list (disabled/deleted by host),
// is marked exhausted in cache, or is in failover cooldown. When switching,
// it picks the first non-exhausted candidate and updates activeAuthID so the
// panel reflects the change on next dashboard load.
func pickActiveAuth(candidates []activeAuthCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	byID := make(map[string]activeAuthCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ID] = c
	}

	cur := getActiveAuthID()
	// Keep current selection if it's still a live candidate AND not disabled/exhausted/cooling-down.
	if cur != "" {
		if c, ok := byID[cur]; ok && !c.Disabled && !c.Exhausted && !isAccountCoolingDown(cur) {
			return cur
		}
	}

	// Selection is gone, disabled, exhausted or cooling down — pick next
	// non-disabled non-exhausted non-cooling-down, else first.
	var next string
	for _, c := range candidates {
		if !c.Disabled && !c.Exhausted && !isAccountCoolingDown(c.ID) {
			next = c.ID
			break
		}
	}
	if next == "" {
		// All exhausted/disabled/cooling-down — keep current if still alive, else first candidate.
		if cur != "" {
			if _, ok := byID[cur]; ok {
				return cur
			}
		}
		next = candidates[0].ID
	}
	if next != "" && next != cur {
		setActiveAuthID(next)
	}
	return next
}

// ensureDefaultActiveAuth sets the panel-selected account.
// Called from buildDashboardEx on every /accounts and /refresh request.
//
// Rules (single source of truth, same as pickActiveAuth):
//  1. If current selection is live AND not exhausted AND not cooling down → keep it.
//  2. If current selection is exhausted or cooling down → switch to first non-exhausted.
//  3. If current selection is gone (disabled/deleted) → switch to first available.
//  4. If all exhausted → keep current if alive, else first.
//
// This ensures the panel's selected card always matches what scheduler.pick
// actually routes to. No silent drift.
func ensureDefaultActiveAuth(accounts []wbAccount) string {
	cur := getActiveAuthID()
	live := make(map[string]wbAccount, len(accounts))
	for _, a := range accounts {
		live[a.AuthID] = a
	}

	// Rule 1: current selection is live AND not exhausted AND not cooling down → keep.
	if cur != "" {
		if a, ok := live[cur]; ok && !a.Disabled && !a.Exhausted && !isAccountCoolingDown(cur) {
			return cur
		}
	}

	// Rule 2 & 3: selection is exhausted, cooling down or gone → find next.
	var firstAny, firstOK, firstReady string
	for _, a := range accounts {
		if firstAny == "" {
			firstAny = a.AuthID
		}
		if a.Disabled {
			continue
		}
		if firstOK == "" {
			firstOK = a.AuthID
		}
		if !a.Exhausted && !isAccountCoolingDown(a.AuthID) && firstReady == "" {
			firstReady = a.AuthID
		}
	}

	// Prefer first non-exhausted non-disabled.
	next := firstReady
	if next == "" {
		// Rule 4: all exhausted — keep current if still alive (not disabled).
		if cur != "" {
			if a, ok := live[cur]; ok && !a.Disabled {
				return cur
			}
		}
		next = firstOK
	}
	if next == "" {
		next = firstAny
	}
	if next != "" && next != cur {
		setActiveAuthID(next)
	}
	return next
}
