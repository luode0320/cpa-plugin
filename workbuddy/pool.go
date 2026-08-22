// pool.go manages the three-tier routing pools — default / priority / fallback —
// persisted as the top-level `pool` field on the physical auth file.
//
// Every workbuddy account belongs to exactly one pool:
//   - default  = the normal pool. Accounts with no explicit pool marker land here.
//   - priority = the preferred pool. As long as ≥1 priority account is usable,
//     scheduler.pick routes ONLY from this bucket.
//   - fallback = the last-resort pool. Used only when both the priority and the
//     default bucket have no usable account.
//
// Routing cascade (scheduler.pick): priority bucket (≥1 usable) → default bucket
// (≥1 usable) → fallback bucket (≥1 usable) → defer to built-in scheduler. This
// guarantees:
//   - Priority accounts never share traffic with default/fallback accounts while
//     alive (hard bucketing, not probability).
//   - When priority accounts are exhausted/cooling-down, traffic cascades to
//     default; when default is also exhausted, it cascades to fallback — no
//     4xx/5xx cascade.
//   - Toggling is live: every dashboard refresh re-reads disk, and the next
//     scheduler.pick honors the new pool immediately.
//
// The on-disk `pool` field is the single source of truth (host.auth.save drops
// unrecognized top-level fields — same root cause as manual_disable), so all
// writes go through persistAuthDirect and the in-memory map is only a mirror
// rebuilt on /accounts and /pool calls.
package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// Pool names. The empty/unknown string always normalizes to poolDefault.
const (
	poolDefault  = "default"
	poolPriority = "priority"
	poolFallback = "fallback"
)

// poolOrder is the routing cascade: pick the first bucket that has ≥1 usable
// account. Declared as an array (not a slice) so tests can't mutate it.
var poolOrder = [3]string{poolPriority, poolDefault, poolFallback}

// authPool is the in-memory mirror of the on-disk `pool` field, keyed by
// auth.ID (the durable account identifier, same as SchedulerAuthCandidate.ID).
// Accounts absent from the map are implicitly poolDefault.
var (
	authPoolMu sync.RWMutex
	authPool   = make(map[string]string)
)

// validPool reports whether name is one of the three supported pools.
func validPool(name string) bool {
	switch name {
	case poolDefault, poolPriority, poolFallback:
		return true
	}
	return false
}

// poolFor returns the pool an account currently belongs to. Unknown or empty
// values normalize to poolDefault — every account is default unless marked.
func poolFor(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return poolDefault
	}
	authPoolMu.RLock()
	p := authPool[authID]
	authPoolMu.RUnlock()
	if !validPool(p) {
		return poolDefault
	}
	return p
}

// authPoolSnapshot returns a copy of the pool map (auth.ID → pool name).
// RLock-safe; safe to call from scheduler.pick on every request.
func authPoolSnapshot() map[string]string {
	authPoolMu.RLock()
	defer authPoolMu.RUnlock()
	out := make(map[string]string, len(authPool))
	for k, v := range authPool {
		out[k] = v
	}
	return out
}

// setPool updates the in-memory pool entry. Callers must persist the change to
// disk via persistPoolToggle before returning, otherwise the next /accounts
// reload from disk will revert. Setting poolDefault removes the entry (default
// is the implicit state, so an explicit "default" mark is not stored).
func setPool(authID, pool string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if !validPool(pool) {
		pool = poolDefault
	}
	authPoolMu.Lock()
	if pool == poolDefault {
		delete(authPool, authID)
	} else {
		authPool[authID] = pool
	}
	authPoolMu.Unlock()
}

// clearPoolFor removes the auth from the pool map (used on auth deletion so a
// deleted account never lingers and never gets picked).
func clearPoolFor(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	authPoolMu.Lock()
	delete(authPool, authID)
	authPoolMu.Unlock()
}

// refreshAuthPoolFromDisk rebuilds the in-memory pool map from the host's
// current auth file list and returns per-pool sizes (only priority/fallback —
// default is implicit and omitted). Called from /accounts and /pool
// (post-write) so the pool is always consistent with disk. Errors are
// intentionally swallowed: a transient host RPC failure shouldn't blank the
// pool when the next /accounts will succeed.
func refreshAuthPoolFromDisk() map[string]int {
	files, err := hostAuthList()
	if err != nil {
		return map[string]int{}
	}
	next := make(map[string]string, len(files))
	live := make(map[string]struct{}, len(files))
	sizes := make(map[string]int)
	for _, f := range files {
		live[f.ID] = struct{}{}
		phys, err2 := hostAuthGetPhysical(f.AuthIndex)
		if err2 != nil || phys == nil {
			continue
		}
		p := parsePoolFromAuthJSON(phys.JSON)
		if p != poolDefault {
			next[f.ID] = p
			sizes[p]++
		}
	}
	authPoolMu.Lock()
	authPool = next
	authPoolMu.Unlock()
	// Prune any in-memory entry whose auth is no longer live on disk.
	for id := range authPoolSnapshot() {
		if _, ok := live[id]; !ok {
			clearPoolFor(id)
		}
	}
	return sizes
}

// persistPoolToggle writes the pool marker to the physical auth file.
//
// Uses writeAuthFileDirect (NOT host.auth.save) because the host silently drops
// unrecognized top-level fields on save — same root cause as manual_disable in
// keepalive.go. Direct write lets the host's file watcher re-synthesize the auth
// record with the new top-level fields preserved.
//
// The write is an in-place JSON merge: read existing raw, set the pool key in
// place (and drop the legacy `priority` boolean if present so the two fields
// can never disagree), marshal back. Every other top-level key stays intact.
func persistPoolToggle(authIndex, authID, pool string) error {
	authIndex = strings.TrimSpace(authIndex)
	authID = strings.TrimSpace(authID)
	if authIndex == "" {
		return errAuthIndexRequired()
	}
	if !validPool(pool) {
		pool = poolDefault
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
		// flags is better than refusing the write.
		doc = map[string]any{}
	}
	if pool == poolDefault {
		delete(doc, "pool")
	} else {
		doc["pool"] = pool
	}
	// Migrate away from the legacy `priority: true` boolean (two-pool era) so
	// the two fields can never disagree on the next parse.
	delete(doc, "priority")
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	name := phys.Name
	if err := persistAuthDirect(name, phys.Path, "", raw); err != nil {
		return err
	}
	setPool(authID, pool)
	return nil
}

// errAuthIndexRequired / errAuthMissing are tiny helpers so persistPoolToggle
// doesn't litter the import block with fmt.Errorf.
func errAuthIndexRequired() error { return &poolErr{msg: "auth_index is required"} }
func errAuthMissing() error       { return &poolErr{msg: "auth file missing or empty"} }

type poolErr struct{ msg string }

func (e *poolErr) Error() string { return e.msg }
