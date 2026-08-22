// retry_config.go owns the per-request retry-on-4xx budget.
//
// When a request hits an account-level 4xx (401/403/404/405) the executor
// may try the same request on the next available account up to budget
// times before giving up. The budget is parsed from plugin.yaml's
// `retry_on_4xx:` field on every configure()/reconfigure call, with 3 as
// the default. Setting it to 0 disables on-request account failover (the
// old behavior) — useful as a kill-switch when a global outage makes
// retries destructive.
package main

import (
	"strconv"
	"strings"
	"sync"
)

// retryOn4xxDefault is the default per-request retry budget when
// `retry_on_4xx:` is absent from plugin.yaml. 3 means up to 4 total
// attempts (initial + 3 retries) per request, across distinct accounts.
const retryOn4xxDefault = 3

// retryOn4xxMin and retryOn4xxMax clamp user config to a sane range.
// Upper bound 5 prevents a misconfiguration from spinning the request
// forever when the upstream is universally 4xx-failing.
const (
	retryOn4xxMin = 0
	retryOn4xxMax = 5
)

var (
	retryOn4xx   = retryOn4xxDefault
	retryOn4xxMu sync.RWMutex
)

// setRetryOn4xx applies a parsed budget under the lock. Caller is
// expected to have already clamped the value.
func setRetryOn4xx(n int) {
	retryOn4xxMu.Lock()
	retryOn4xx = n
	retryOn4xxMu.Unlock()
}

// loadedRetryOn4xx returns the currently configured budget (default 3
// when nothing has been parsed yet).
func loadedRetryOn4xx() int {
	retryOn4xxMu.RLock()
	defer retryOn4xxMu.RUnlock()
	return retryOn4xx
}

// clampRetryOn4xx returns n clamped to [retryOn4xxMin, retryOn4xxMax].
// Out-of-range and unparseable values fall back to the default; the call
// site decides whether to log the substitution.
func clampRetryOn4xx(n int) int {
	if n < retryOn4xxMin {
		return retryOn4xxDefault
	}
	if n > retryOn4xxMax {
		return retryOn4xxMax
	}
	return n
}

// parseRetryOn4xxLine extracts the integer from a `retry_on_4xx: N`
// config_yaml line. Returns (value, ok). Whitespace and inline comments
// after the number are tolerated.
func parseRetryOn4xxLine(line string) (int, bool) {
	v := strings.TrimSpace(strings.TrimPrefix(line, "retry_on_4xx:"))
	v = strings.Trim(v, "\"'")
	if v == "" {
		return 0, false
	}
	// Strip a trailing YAML comment ("# something").
	if i := strings.Index(v, "#"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}
