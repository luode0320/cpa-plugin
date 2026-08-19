package usagestats

import (
	"strings"
)

// This file retains the pure presentation helpers from the ported tracker.
// The host auth identity resolver (authRuntimeLookup) is dropped: the
// workbuddy plugin records usage with AuthIndex=UID directly, so no
// host.auth.get_runtime round-trip is needed.

func displayAuthProvider(value string) string {
	value = normalizeDimension(value)
	switch strings.ToLower(value) {
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	case "xai", "x-ai", "grok":
		return "Grok"
	default:
		return value
	}
}

func safeAuthAccount(value string) string {
	value = normalizeDimension(value)
	if looksLikeCredential(value) {
		return ""
	}
	return value
}

func safeAuthLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || looksLikeCredential(value) {
		return ""
	}
	return normalizeDimension(value)
}

func firstNonEmptyIdentity(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
