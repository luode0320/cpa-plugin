// preserve.go manages the "preserve" status — accounts flagged to be kept
// idle so they keep a small credit buffer and never carry traffic.
//
// Why a status field instead of a 4th pool: pool is the user's *intent*
// (priority / default / fallback). Preserve is a *runtime health gate* —
// accounts temporarily shielded because their credit balance just dropped
// below a threshold. Mixing them would mean the user's priority/default/
// fallback selection gets clobbered whenever credits fluctuate, then they
// have to re-click the toggle when credits recover. Splitting the two
// concerns keeps the user-facing pool stable and the health gate dynamic.
//
// Routing contract (scheduler.pick):
//  1. Drop disabled accounts.
//  2. Drop preserve accounts (this file).
//  3. Drop cooling-down accounts (accountFailover.go).
//  4. Cascade priority → default → fallback on the survivors.
//
// Preserve filter is *before* cooldown so a freshly preserved account that
// then 429s doesn't double-count (cooldown's filter still applies to the
// survivors). lastNonEmpty fallback mirrors the cooldown behavior.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// preserveConfig holds the runtime-tunable watchdog knobs. Populated from
// plugin config_yaml in configure() (usage_config.go); readers take the RW
// lock so a concurrent reconfigure is safe.
//
// Defaults are exported (preserveThresholdDefault / preserveWatchdogIntervalDefault
// / preserveWatchdogEnabledDefault) and mirrored by the accessors below.
var (
	preserveThresholdMu  sync.RWMutex
	preserveThresholdCfg = preserveThresholdDefault

	preserveWatchdogIntervalMu  sync.RWMutex
	preserveWatchdogIntervalCfg = preserveWatchdogIntervalDefault

	preserveWatchdogEnabledMu  sync.RWMutex
	preserveWatchdogEnabledCfg = preserveWatchdogEnabledDefault
)

// preserveThreshold returns the current credit-balance threshold below which
// the watchdog flags an account as preserve. RLock-safe; called on every
// watchdog tick.
func preserveThreshold() int64 {
	preserveThresholdMu.RLock()
	defer preserveThresholdMu.RUnlock()
	return preserveThresholdCfg
}

// preserveWatchdogInterval returns the current watchdog tick interval.
// RLock-safe; the loop reads this on every sleep so changes apply at the
// next cycle without restarting the goroutine.
func preserveWatchdogInterval() time.Duration {
	preserveWatchdogIntervalMu.RLock()
	defer preserveWatchdogIntervalMu.RUnlock()
	return preserveWatchdogIntervalCfg
}

// preserveWatchdogEnabled reports whether the watchdog is currently
// active. RLock-safe.
func preserveWatchdogEnabled() bool {
	preserveWatchdogEnabledMu.RLock()
	defer preserveWatchdogEnabledMu.RUnlock()
	return preserveWatchdogEnabledCfg
}

// setPreserveConfig applies the next watchdog configuration under its own
// locks (no nesting). Called from configure() after parsing config_yaml.
// Each setter has a no-nesting contract so a reconfigure cannot deadlock
// against a tick in flight.
func setPreserveConfig(threshold int64, interval time.Duration, enabled bool) {
	preserveThresholdMu.Lock()
	if threshold >= 0 {
		preserveThresholdCfg = threshold
	}
	preserveThresholdMu.Unlock()

	preserveWatchdogIntervalMu.Lock()
	if interval > 0 {
		preserveWatchdogIntervalCfg = interval
	}
	preserveWatchdogIntervalMu.Unlock()

	preserveWatchdogEnabledMu.Lock()
	preserveWatchdogEnabledCfg = enabled
	preserveWatchdogEnabledMu.Unlock()
}

// preserveSet mirrors the top-level `preserve` flag on the physical auth
// file. Membership means "do not route traffic here, keep the credits".
// Lifecycle watchdog toggles membership every ~10 minutes; nothing else
// writes to it (the panel intentionally cannot set preserve — manual
// toggling would defeat the dynamic health-gate contract).
var (
	preserveSetMu sync.RWMutex
	preserveSet   = make(map[string]struct{})
)

// isPreserve reports whether auth.ID is currently in the preserve set.
// scheduler.pick reads this on every request — keep it cheap.
func isPreserve(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	preserveSetMu.RLock()
	_, ok := preserveSet[authID]
	preserveSetMu.RUnlock()
	return ok
}

// preserveSnapshot returns a copy of the current set (auth.ID → true).
// Used by /accounts to surface preserve_pool_size.
func preserveSnapshot() map[string]bool {
	preserveSetMu.RLock()
	defer preserveSetMu.RUnlock()
	out := make(map[string]bool, len(preserveSet))
	for k := range preserveSet {
		out[k] = true
	}
	return out
}

// preserveSetPut adds an auth to the preserve set. Callers MUST persist the
// change via persistPreserveToggle before returning, otherwise the next
// /accounts reload from disk will revert. Idempotent.
func preserveSetPut(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	preserveSetMu.Lock()
	preserveSet[authID] = struct{}{}
	preserveSetMu.Unlock()
}

// preserveSetClear removes an auth from the preserve set. Idempotent.
func preserveSetClear(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	preserveSetMu.Lock()
	delete(preserveSet, authID)
	preserveSetMu.Unlock()
}

// refreshPreserveSetFromDisk rebuilds the in-memory preserve set from the
// host's current auth file list. Called from /accounts and watchdog tick so
// the set is always consistent with disk. Returns the new size.
//
// Errors are intentionally swallowed: a transient host RPC failure shouldn't
// blank the set when the next /accounts will succeed.
func refreshPreserveSetFromDisk() int {
	files, err := hostAuthList()
	if err != nil {
		return len(preserveSnapshot())
	}
	next := make(map[string]struct{}, len(files))
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		phys, err2 := hostAuthGetPhysical(f.AuthIndex)
		if err2 != nil || phys == nil {
			continue
		}
		if parsePreserveFromAuthJSON(phys.JSON) {
			next[f.ID] = struct{}{}
		}
	}
	preserveSetMu.Lock()
	preserveSet = next
	preserveSetMu.Unlock()
	// Prune any in-memory entry whose auth is no longer live on disk.
	for id := range preserveSnapshot() {
		if _, ok := live[id]; !ok {
			preserveSetClear(id)
		}
	}
	return len(preserveSnapshot())
}

// parsePreserveFromAuthJSON reads the top-level preserve flag. The flag is
// the single source of truth — lives on the physical auth JSON, never on the
// host's auth record, because host.auth.save rebuilds the record and drops
// top-level fields the host doesn't recognize (same root cause as
// manual_disable / pool).
func parsePreserveFromAuthJSON(raw []byte) bool {
	var m struct {
		Preserve bool `json:"preserve"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Preserve
}

// persistPreserveToggle writes the preserve flag to the physical auth file.
// Uses writeAuthFileDirect (NOT host.auth.save) because the host silently
// drops unrecognized top-level fields on save. Direct write lets the host's
// file watcher re-synthesize the auth record with the new top-level field
// preserved alongside disabled / note / pool / manual_disable / etc.
//
// on=true sets preserve: true; on=false drops the key entirely so the file
// stays clean and parsePreserveFromAuthJSON returns false on the next read.
func persistPreserveToggle(authIndex, authID string, on bool) error {
	authIndex = strings.TrimSpace(authIndex)
	authID = strings.TrimSpace(authID)
	if authIndex == "" {
		return errAuthIndexRequired()
	}
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return err
	}
	if phys == nil || len(phys.JSON) == 0 {
		return errAuthMissing()
	}
	var doc map[string]any
	if err := json.Unmarshal(phys.JSON, &doc); err != nil {
		// Treat malformed JSON as a fresh doc — losing the existing top-level
		// flags is better than refusing the write (consistent with
		// persistPoolToggle).
		doc = map[string]any{}
	}
	if on {
		doc["preserve"] = true
	} else {
		delete(doc, "preserve")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := persistAuthDirect(phys.Name, phys.Path, "", raw); err != nil {
		return err
	}
	if on {
		preserveSetPut(authID)
	} else {
		preserveSetClear(authID)
	}
	return nil
}
