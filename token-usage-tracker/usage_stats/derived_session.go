package usagestats

import (
	"fmt"
	"strings"
	"time"
)

// DefaultDerivedSessionWindow is the default time-bucket size used by
// DeriveAuthSessionKey when the configuration is silent. Records with
// requested_at landing in the same window collapse into one derived key.
const DefaultDerivedSessionWindow = 30 * time.Minute

// MinDerivedSessionWindow / MaxDerivedSessionWindow bound the user-configurable
// bucket size so a misconfigured 1ns window does not fragment the dashboard
// (one entry per request) and a misconfigured 1y window does not glue every
// account's traffic together.
const (
	MinDerivedSessionWindow = 1 * time.Minute
	MaxDerivedSessionWindow = 24 * time.Hour
)

// DerivedSessionKeyPrefix is the namespace tag the dashboard uses to tell a
// derived pseudo-session apart from a host-reported real session. Real keys
// arriving via host UsagePlugin.SessionKey use the namespace chosen by the
// host (e.g. "execution:", "derived:"); tracker-derived keys always carry this
// prefix so the dashboard can render them with a distinct tooltip or filter.
const DerivedSessionKeyPrefix = "auth:"

// DeriveAuthSessionKey returns a stable pseudo session_key for records whose
// host-delivered SessionKey is empty. The key is composed of the credential
// identity (AuthIndex — the per-instance alias surfaced under 来源 in the
// dashboard), the upstream provider, the model alias the user selected, and a
// wall-clock bucket aligned to window boundaries:
//
//	auth:<authIndex>:<provider>:<alias>:w<bucketUnix>
//
// Limitations (documented for the dashboard user):
//
//  1. Multiple users sharing the same upstream credential collapse into one
//     bucket — there is no per-user identity in the host UsagePlugin
//     broadcast.
//  2. A single user driving multiple parallel conversations on the same
//     credential collapses into one bucket — the host does not surface any
//     conversation identifier either.
//
// When the host later starts emitting a real SessionKey (or when the workbuddy
// feed carries one), the caller must keep the real key and skip this
// derivation. The dashboard prefix guarantees both namespaces coexist.
func DeriveAuthSessionKey(provider, alias, authIndex string, requestedAt time.Time, window time.Duration) string {
	authID := sanitizeDerivedSegment(authIndex)
	if authID == "" {
		return ""
	}
	providerSeg := sanitizeDerivedSegment(provider)
	if providerSeg == "" {
		providerSeg = "unknown"
	}
	aliasSeg := sanitizeDerivedSegment(alias)
	if aliasSeg == "" {
		aliasSeg = "unknown"
	}
	bucket := derivedBucketStart(requestedAt, window)
	return fmt.Sprintf("%s%s:%s:%s:w%d", DerivedSessionKeyPrefix, authID, providerSeg, aliasSeg, bucket.Unix())
}

// sanitizeDerivedSegment strips characters that would break the dashboard
// layout (colons, slashes, whitespace) while preserving the visible identity.
// The output is intentionally restricted to a conservative character set so
// downstream grouping, filtering, and rendering treat it as a single token.
func sanitizeDerivedSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxDimensionRunes {
		out = out[:maxDimensionRunes]
	}
	return out
}

// derivedBucketStart aligns requestedAt to the start of its time window. A
// non-positive window is treated as DefaultDerivedSessionWindow so the
// derivation stays well-defined under misconfiguration.
func derivedBucketStart(requestedAt time.Time, window time.Duration) time.Time {
	if window <= 0 {
		window = DefaultDerivedSessionWindow
	}
	t := requestedAt.UTC()
	return t.Truncate(window)
}

// NormalizeDerivedSessionWindow clamps user-supplied window strings to the
// supported range. Returns DefaultDerivedSessionWindow on empty input so the
// dashboard works out of the box without any explicit configuration.
func NormalizeDerivedSessionWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return DefaultDerivedSessionWindow
	}
	if window < MinDerivedSessionWindow {
		return MinDerivedSessionWindow
	}
	if window > MaxDerivedSessionWindow {
		return MaxDerivedSessionWindow
	}
	return window
}