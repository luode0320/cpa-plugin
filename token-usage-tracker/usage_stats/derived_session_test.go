package usagestats

import (
	"strings"
	"testing"
	"time"
)

func TestDeriveAuthSessionKeyStableWithinBucket(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute

	first := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-abc", base, window)
	// 10 minutes later — same bucket.
	second := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-abc", base.Add(10*time.Minute), window)
	if first != second {
		t.Fatalf("expected identical keys within window: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, DerivedSessionKeyPrefix) {
		t.Fatalf("derived key missing namespace prefix: %q", first)
	}
	if !strings.Contains(first, "key-abc") {
		t.Fatalf("derived key lost authIndex: %q", first)
	}
	if !strings.Contains(first, "openai-compatible-provider") {
		t.Fatalf("derived key lost provider: %q", first)
	}
	if !strings.Contains(first, "gpt-5.6-sol") {
		t.Fatalf("derived key lost alias: %q", first)
	}
}

func TestDeriveAuthSessionKeyChangesAcrossBuckets(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute
	first := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-abc", base, window)
	// 31 minutes later — next bucket.
	second := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-abc", base.Add(31*time.Minute), window)
	if first == second {
		t.Fatalf("expected different keys across bucket boundary: %q vs %q", first, second)
	}
}

func TestDeriveAuthSessionKeyDifferentiatesCredentials(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute
	a := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-aaa", base, window)
	b := DeriveAuthSessionKey("openai-compatible-provider", "gpt-5.6-sol", "key-bbb", base, window)
	if a == b {
		t.Fatalf("expected different keys for different credentials, both = %q", a)
	}
}

func TestDeriveAuthSessionKeyDifferentiatesProvidersAndAliases(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute
	a := DeriveAuthSessionKey("openai", "gpt-5", "key-abc", base, window)
	b := DeriveAuthSessionKey("deepseek", "deepseek-v4-flash", "key-abc", base, window)
	if a == b {
		t.Fatalf("expected different keys for different provider/alias, both = %q", a)
	}
}

func TestDeriveAuthSessionKeyEmptyWithoutAuthIndex(t *testing.T) {
	// Empty authIndex → no anchor to group by → empty string so the
	// dashboard still renders "—" rather than fabricating a misleading tag.
	got := DeriveAuthSessionKey("openai", "gpt-5", "", time.Now(), 30*time.Minute)
	if got != "" {
		t.Fatalf("expected empty key without credential identity, got %q", got)
	}
}

func TestDeriveAuthSessionKeySanitizesUntrustedInput(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute
	got := DeriveAuthSessionKey("openai compatible provider", "gpt-5.6-sol/with/slashes", "key/with:special chars", base, window)
	// The structural ":" between segments is fine; what must not survive is
	// untrusted input leaking through as a "/" / ":" / whitespace inside any
	// of the segments the user-facing path will render.
	segments := strings.Split(strings.TrimPrefix(got, DerivedSessionKeyPrefix), ":")
	if len(segments) < 4 {
		t.Fatalf("derived key shape unexpected: %q", got)
	}
	for i, seg := range segments {
		if i == len(segments)-1 {
			// last segment is the unix bucket; only digits expected.
			continue
		}
		if strings.ContainsAny(seg, "/: ") {
			t.Fatalf("segment %d (%q) leaked untrusted input: %q", i, seg, got)
		}
	}
}

func TestDeriveAuthSessionKeyHandlesUnknownProviderAndAlias(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	window := 30 * time.Minute
	got := DeriveAuthSessionKey("", "", "key-abc", base, window)
	if !strings.Contains(got, ":unknown:unknown:w") {
		t.Fatalf("expected unknown placeholders, got %q", got)
	}
}

func TestDeriveAuthSessionKeyUsesDefaultWindowOnZero(t *testing.T) {
	base := time.Date(2026, 8, 23, 3, 12, 0, 0, time.UTC)
	zero := DeriveAuthSessionKey("openai", "gpt-5", "key-abc", base, 0)
	defaulted := DeriveAuthSessionKey("openai", "gpt-5", "key-abc", base, DefaultDerivedSessionWindow)
	if zero != defaulted {
		t.Fatalf("expected zero window to fall back to default: %q vs %q", zero, defaulted)
	}
}

func TestNormalizeDerivedSessionWindowClamps(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, DefaultDerivedSessionWindow},
		{-1 * time.Minute, DefaultDerivedSessionWindow},
		{500 * time.Millisecond, MinDerivedSessionWindow},
		{48 * time.Hour, MaxDerivedSessionWindow},
		{2 * time.Hour, 2 * time.Hour},
		{5 * time.Minute, 5 * time.Minute},
	}
	for _, c := range cases {
		got := NormalizeDerivedSessionWindow(c.in)
		if got != c.want {
			t.Errorf("NormalizeDerivedSessionWindow(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}