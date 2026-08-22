// failover_retry.go selects an alternate account for in-flight failover.
//
// When the executor hits an account-level 4xx (401/403/404/405), the request
// itself is fine — only this account is broken. Rather than failing the
// user, the executor tries the same request on a different qoderwork
// account. pickNextAuth returns the next usable (authID, storedAuth) pair
// in a stable fallback order, skipping the current failing account and any
// that are disabled / cooling down / failing-credits.
//
// The selection intentionally mirrors pickActiveAuth's filter rules but
// is intentionally simpler (no random spread): when a request is already
// in flight and one account just failed, deterministically trying the next
// available one in host-reported order keeps recovery path predictable
// across logs.
//
// Unlike the workbuddy plugin, qoderwork signs every request with COSY
// (applyCosyHeaders) against a fixed gateway endpoint, so a retry rebuild
// means: same encoded body + same endpoint, fresh COSY signature for the
// new account. rebuildRequestWithQoderAuth encapsulates that.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// pickNextAuth returns the next qoderwork account to retry on after
// currentAuthID failed. It is best-effort: any error reading the host's
// auth list, loading a candidate's bundle, or evaluating a credential's
// state results in ok=false so the caller falls back to surfacing the
// original error to the user.
//
// ok=false means no alternate account was usable; the caller should stop
// retrying and propagate the last upstream error.
func pickNextAuth(currentAuthID string) (nextAuthID string, nextSA *storedAuth, ok bool) {
	currentAuthID = strings.TrimSpace(currentAuthID)
	files, err := hostAuthList()
	if err != nil || len(files) == 0 {
		return "", nil, false
	}

	// First pass: find the first file-entry whose host-reported ID is
	// NOT currentAuthID and passes the cheap filters (disabled,
	// cooling down). Order is host-provided so successive retries
	// walk the same predictable path.
	for _, f := range files {
		id := strings.TrimSpace(f.ID)
		if id == "" {
			continue
		}
		if id == currentAuthID {
			continue
		}
		if f.Disabled {
			continue
		}
		if isAccountCoolingDown(id) {
			continue
		}
		// Skip candidates with no usable auth index — we can't load
		// their stored body, so retrying on them would fail
		// identically and burn a budget slot.
		if strings.TrimSpace(f.AuthIndex) == "" {
			continue
		}

		sa, phys, loadErr := hostAuthGetBundle(f.AuthIndex)
		if loadErr != nil || sa == nil {
			// Treat as not-usable; keep walking. Don't surface the
			// load error — there may be a viable next candidate.
			_ = phys
			continue
		}

		// Defensive: ensure the stored auth actually carries a token
		// worth retrying on. An empty Access/Refresh would 401
		// identically (COSY would also fail to build a session).
		if strings.TrimSpace(sa.Auth.AccessToken) == "" && strings.TrimSpace(sa.Auth.RefreshToken) == "" {
			continue
		}

		return id, sa, true
	}
	return "", nil, false
}

// rebuildRequestWithQoderAuth builds a fresh request against the same
// QoderWork gateway with the same encoded body, but signed for a different
// account. COSY headers are per-account (session + signature over body+URL),
// so every account swap must re-run applyCosyHeaders.
//
// The caller keeps the original request untouched for its own retry book-
// keeping; this function only needs the (account-independent) encoded body
// and the upstream model key that goes into x-model-key.
func rebuildRequestWithQoderAuth(sa *storedAuth, encodedBody, modelKey string) (*http.Request, error) {
	if sa == nil {
		return nil, fmt.Errorf("rebuildRequestWithQoderAuth: nil stored auth")
	}
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointChat, strings.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, encodedBody, endpointChat, modelKey, true); err != nil {
		return nil, fmt.Errorf("cosy: %w", err)
	}
	return req, nil
}

// readAllUpstreamErr drains the response body of a failed 4xx/5xx upstream
// call into a bounded byte slice so we can pass it to noteAccountFailure /
// publishUsage without holding the body open indefinitely. Empty bodies
// collapse to "" — callers already handle that.
//
// We intentionally read via the same host stream bridge as the success
// path so the framing behavior (chunked, capped) stays consistent.
func readAllUpstreamErr(r io.Reader) string {
	if r == nil {
		return ""
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return string(buf)
}

// parseUpstreamStatusFromErr extracts the HTTP status code from an error
// produced by the "upstream N: ..." error shape shared by doExecuteOnceQoder
// / collectUpstreamStreamQoder / pumpUpstreamStream. Returns 0 for
// transport-level errors, parse failures, and nil.
func parseUpstreamStatusFromErr(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "upstream ") {
		return 0
	}
	rest := strings.TrimPrefix(msg, "upstream ")
	idx := strings.Index(rest, ":")
	if idx <= 0 {
		return 0
	}
	n, perr := strconv.Atoi(strings.TrimSpace(rest[:idx]))
	if perr != nil {
		return 0
	}
	return n
}
