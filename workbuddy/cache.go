// cache.go holds the per-account in-memory cache for plan / checkin / credits
// snapshots and the singleflight machinery that dedups concurrent upstream
// fetches for the same account. The cache is the coordination point between
// the dashboard, reconcile, and scheduler pick paths.
package main

import (
	"sort"
	"sync"
	"time"
)

// the failed field falls back to the previous value instead of being wiped.
type accountCacheEntry struct {
	checkin *checkinSummary
	credits *creditsSummary
	plan    string
	fetched time.Time
}

var (
	accountCache    sync.Map // auth_id (auth.ID) -> *accountCacheEntry
	accountCacheTTL = 45 * time.Second
)

// accountDetailFlight is a per-authID singleflight: concurrent dashboard /
// reconcile callers for the same account share one upstream fetch instead of
// stampeding the billing API. Without this, two parallel panel refreshes would
// each spawn 3 goroutines per account → 6× upstream QPS and last-writer-wins
// cache corruption.
var accountDetailFlight sync.Map // authID -> *accountDetailCall

type accountDetailCall struct {
	done chan struct{}
	plan string
	ci   *checkinSummary
	cr   *creditsSummary
	errs []string
}

// cachedAccountDetails fetches plan/checkin/credits concurrently (upstream
// round-trip dominates; 3 serial calls ≈ 3× latency). On any individual
// failure the previous cached value is kept (stale-while-error) so a
// transient upstream 500 does not blank the panel row.
func cachedAccountDetails(authID string, sa *storedAuth, force bool) (plan string, ci *checkinSummary, cr *creditsSummary, errs []string) {
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev = v.(*accountCacheEntry)
		if !force && time.Since(prev.fetched) < accountCacheTTL {
			// Return cached values. Do NOT mutate prev.credits here — concurrent
			// goroutines (reconcileOneAccount) may read the same entry.
			// FetchedAt is stamped at Store time; if it's empty (legacy entry),
			// the panel can derive it from prev.fetched if needed.
			return prev.plan, prev.checkin, prev.credits, nil
		}
	}

	// Singleflight: only one goroutine performs the upstream fetch per authID.
	// Others wait on the in-flight call's done channel and reuse its result.
	// Note: force=true callers DO join the flight (P1-1 trade-off). This is
	// intentional: the flight window is short (~3 concurrent fetches), and
	// skipping it would re-introduce the P0-2 race where concurrent writers
	// overwrite each other's cache entries. The result a force caller gets
	// is at most a few hundred ms old — fresh enough for lifecycle decisions.
	call := &accountDetailCall{done: make(chan struct{})}
	actual, loaded := accountDetailFlight.LoadOrStore(authID, call)
	if loaded {
		// Someone else is fetching — wait for their result.
		other := actual.(*accountDetailCall)
		<-other.done
		// Re-read cache: fetcher already Stored; use whatever won the race.
		if v, ok := accountCache.Load(authID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				return e.plan, e.checkin, e.credits, other.errs
			}
		}
		return other.plan, other.ci, other.cr, other.errs
	}
	// We are the fetcher. Make sure waiters wake up and the flight entry is
	// released even on panic.
	defer func() {
		call.plan, call.ci, call.cr, call.errs = plan, ci, cr, errs
		close(call.done)
		accountDetailFlight.Delete(authID)
	}()

	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		errList []string
	)
	addErr := func(msg string) {
		errMu.Lock()
		errList = append(errList, msg)
		errMu.Unlock()
	}
	wg.Add(3)
	go func() { defer wg.Done(); plan = fetchPaymentType(sa) }()
	go func() {
		defer wg.Done()
		if c, err := fetchCheckinStatus(sa); err == nil {
			ci = c
		} else {
			addErr("checkin: " + err.Error())
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := fetchUserResource(sa); err == nil {
			cr = r
		} else {
			addErr("credits: " + err.Error())
		}
	}()
	wg.Wait()
	// Stale-while-error: carry over previous values for fields that failed.
	if prev != nil {
		if ci == nil {
			ci = prev.checkin
		}
		if cr == nil {
			cr = prev.credits
		}
		if plan == "" {
			plan = prev.plan
		}
	}
	now := time.Now()
	if cr != nil {
		// Stamp snapshot time for panel/API consumers (A-09 observability).
		cr.FetchedAt = now.UTC().Format(time.RFC3339)
	}
	accountCache.Store(authID, &accountCacheEntry{checkin: ci, credits: cr, plan: plan, fetched: now})
	// Soft cap: if map is huge, drop oldest-looking entries beyond bound.
	pruneAccountCacheSoftCap(accountCacheSoftCap)
	return plan, ci, cr, errList
}

// accountCacheSoftCap limits concurrent cache entries (auth churn / index thrash).
const accountCacheSoftCap = 256

// pruneAccountCacheSoftCap drops excess entries with the oldest fetched time.
// Called after Store; O(n) over map size — fine for dozens of accounts.
func pruneAccountCacheSoftCap(capN int) {
	if capN <= 0 {
		return
	}
	type item struct {
		key string
		at  time.Time
	}
	var items []item
	accountCache.Range(func(key, value any) bool {
		k, _ := key.(string)
		e, ok := value.(*accountCacheEntry)
		if !ok || k == "" {
			accountCache.Delete(key)
			return true
		}
		items = append(items, item{key: k, at: e.fetched})
		return true
	})
	if len(items) <= capN {
		return
	}
	// Sort oldest first (was O(n²) bubble — sort.Slice is O(n log n)).
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	drop := len(items) - capN
	for i := 0; i < drop; i++ {
		accountCache.Delete(items[i].key)
	}
}

// cachedCredits returns the cached credits snapshot for an auth ID, or nil
// when absent. Used by handlers that only need the last known values (e.g.
// displayNote in manual enable/disable) without triggering an upstream fetch.
func cachedCredits(authID string) *creditsSummary {
	v, ok := accountCache.Load(authID)
	if !ok {
		return nil
	}
	e, ok := v.(*accountCacheEntry)
	if !ok || e == nil {
		return nil
	}
	return e.credits
}

// cachedCheckinToday returns cached today_checked_in when present.
func cachedCheckinToday(authID string) *bool {
	v, ok := accountCache.Load(authID)
	if !ok {
		return nil
	}
	e, ok := v.(*accountCacheEntry)
	if !ok || e == nil || e.checkin == nil {
		return nil
	}
	b := e.checkin.TodayCheckedIn
	return &b
}
