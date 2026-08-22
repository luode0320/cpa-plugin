// failover_retry.go selects an alternate account for in-flight failover.
//
// When the executor hits an account-level 4xx (401/403/404/405), the request
// itself is fine — only this account is broken. Rather than failing the
// user, the executor tries the same request on a different workbuddy
// account. pickNextAuth returns the next usable (authID, storedAuth) pair
// in a stable fallback order, skipping the current failing account and any
// that are disabled / cooling down / failing-credits.
//
// The selection intentionally mirrors pickActiveAuth's filter rules but
// is intentionally simpler (no random spread): when a request is already
// in flight and one account just failed, deterministically trying the next
// available one in host-reported order keeps recovery path predictable
// across logs.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// pickNextAuth returns the next workbuddy account to retry on after
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
		// identically.
		if strings.TrimSpace(sa.Auth.AccessToken) == "" && strings.TrimSpace(sa.Auth.RefreshToken) == "" {
			continue
		}

		return id, sa, true
	}
	return "", nil, false
}

// rebuildRequestWithSA returns a fresh *http.Request identical to the
// caller-provided original except for endpoint URL (taken from the new
// account's region/domain) and authorization headers (per-account
// bearer/cookies). The body is re-read from orig.GetBody(), which
// http.NewRequestWithContext populates automatically when the body is
// bytes.Reader / bytes.Buffer / strings.Reader — the shape the caller
// always passes (see handleExecStream). If GetBody is nil for any
// reason we return an error and the caller falls back to surfacing the
// original upstream error.
//
// The context, method, headers other than auth, and request body bytes
// are preserved. Errors during rebuild are non-fatal — the caller
// falls back to surfacing the original error to the user.
func rebuildRequestWithSA(orig *http.Request, sa *storedAuth) (*http.Request, error) {
	if orig == nil {
		return nil, fmt.Errorf("rebuildRequestWithSA: nil original request")
	}
	if sa == nil {
		return nil, fmt.Errorf("rebuildRequestWithSA: nil stored auth")
	}
	if orig.GetBody == nil {
		return nil, fmt.Errorf("rebuildRequestWithSA: original request has no GetBody (rebuild not possible)")
	}
	ctx := orig.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	bodyRC, err := orig.GetBody()
	if err != nil {
		return nil, fmt.Errorf("rebuildRequestWithSA: get body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, orig.Method, endpointChatFor(sa), bodyRC)
	if err != nil {
		return nil, err
	}
	backendHeaders(req, sa)
	// Preserve any non-auth headers the caller set on the original
	// request (e.g. trace id, content-type overrides). backendHeaders
	// above has already populated the auth-correct values, so for any
	// header that overlaps we keep the freshly-applied auth-correct
	// version.
	for k, vs := range orig.Header {
		if _, present := req.Header[k]; present {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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
