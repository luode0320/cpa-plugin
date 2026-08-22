// watchdog.go runs the credits-preserve watchdog — every interval (default
// 10 minutes) it pulls fresh credits for every workbuddy account and flips
// the preserve flag on disk when an account's remaining credits drop below
// the configured threshold (default 50).
//
// Why a watchdog instead of event-driven only: scheduler.pick only reads the
// cached credits snapshot. Without a periodic refresh, an account can sit at
// remain=49 in the cache forever even though the user manually recharged it
// via the workbuddy web UI. The watchdog closes that gap — every interval it
// hits /v2/billing/meter/get-user-resource through cachedAccountDetails
// (which already singleflight-dedups concurrent dashboard fetches), then
// decides whether the account needs to be parked in the preserve set so
// routing stops burning its remaining credits.
//
// Side effects on flip:
//   - enter preserve (remain < threshold): write preserve:true to disk +
//     evictSessionBindingsForAuth so any sticky session is forced to re-pick
//     a non-preserved account on its next request.
//   - exit preserve (remain >= threshold): clear preserve from disk. No
//     session action — when credits recover, the account is healthy again
//     and can carry sessions if picker selects it.
//
// Lifecycle: the loop is started in init() and runs forever. config-driven
// enable/disable is checked every iteration so we never need to restart the
// goroutine (the plugin shutdown path is a no-op due to SIGSEGV risk from
// touching Go sync primitives during host teardown — see checkin.go:31).
package main

import "time"

// Defaults for the preserve watchdog. All overridable via config_yaml.
const (
	preserveThresholdDefault        int64         = 50
	preserveWatchdogIntervalDefault time.Duration = 10 * time.Minute
	preserveWatchdogEnabledDefault                = true
	preserveWatchdogDisabledPoll                  = 30 * time.Second // how often we re-read config when disabled
)

// preserveWatchdogLoop runs forever. First tick fires immediately so a
// freshly-started plugin brings the preserve set in sync with current
// credits without waiting a full interval.
func preserveWatchdogLoop() {
	runPreserveWatchdogTick()
	for {
		enabled := preserveWatchdogEnabled()
		interval := preserveWatchdogInterval()
		sleep := preserveWatchdogDisabledPoll
		if enabled && interval > 0 {
			sleep = interval
		}
		time.Sleep(sleep)
		if !enabled {
			continue
		}
		runPreserveWatchdogTick()
	}
}

// preserveShouldFlip computes whether an account's preserve flag must change
// given a fresh credits snapshot and its current preserve state. Pure logic
// (no host RPC) so the watchdog's decision is unit-testable.
//
// Contract: preserve is entered when remain < threshold (strictly below —
// an account exactly at the threshold keeps routing). Exiting happens when
// credits recover to >= threshold.
func preserveShouldFlip(remain int64, threshold int64, currentlyPreserved bool) (shouldPreserve bool, changed bool) {
	shouldPreserve = remain < threshold
	return shouldPreserve, shouldPreserve != currentlyPreserved
}

// runPreserveWatchdogTick walks every workbuddy auth, pulls a fresh credits
// snapshot, and flips the preserve flag based on the threshold. Failures
// are best-effort: a single account's host RPC error doesn't stop the rest
// of the fleet from being evaluated.
func runPreserveWatchdogTick() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	threshold := preserveThreshold()
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		// Pull fresh credits through cachedAccountDetails so concurrent
		// dashboard fetches share the upstream call (singleflight) instead
		// of stampeding the billing API.
		_, _, cr, _ := cachedAccountDetails(f.ID, sa, true)
		if cr == nil {
			continue
		}
		currentlyPreserved := isPreserve(f.ID)
		shouldPreserve, changed := preserveShouldFlip(cr.TotalRemain, threshold, currentlyPreserved)
		if !changed {
			continue // already in the right state, no-op (no disk write, no binding churn)
		}
		if shouldPreserve {
			// Entering preserve: write the flag, then force-migrate any
			// session pinned to this account so the next request picks a
			// non-preserved account instead of staying on this buffer-
			// starved one and finishing off its remaining credits.
			if err := persistPreserveToggle(f.AuthIndex, f.ID, true); err != nil {
				continue
			}
			evictSessionBindingsForAuth(f.ID)
		} else {
			// Exiting preserve: just clear the flag. The picker will see
			// the account as routable again on the next scheduler.pick.
			if err := persistPreserveToggle(f.AuthIndex, f.ID, false); err != nil {
				continue
			}
		}
	}
}
