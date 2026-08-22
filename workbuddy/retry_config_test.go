package main

import (
	"encoding/json"
	"testing"
)

func TestClampRetryOn4xx(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-1, retryOn4xxDefault},
		{0, 0}, // 0 is valid: kill switch
		{1, 1},
		{3, 3},
		{5, 5},
		{6, retryOn4xxMax},
		{99, retryOn4xxMax},
	}
	for _, tc := range cases {
		if got := clampRetryOn4xx(tc.in); got != tc.want {
			t.Fatalf("clampRetryOn4xx(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseRetryOn4xxLine(t *testing.T) {
	cases := []struct {
		line string
		want int
		ok   bool
	}{
		{"retry_on_4xx: 0", 0, true},
		{"retry_on_4xx: 1", 1, true},
		{"retry_on_4xx: 3", 3, true},
		{"retry_on_4xx: 5", 5, true},
		{`retry_on_4xx: "3"`, 3, true},
		{"retry_on_4xx:   3  ", 3, true},
		{"retry_on_4xx: 3 # hot path kill switch", 3, true},
		{"retry_on_4xx:", 0, false},
		{"retry_on_4xx: abc", 0, false},
		{"retry_on_4xx: 99", 99, true}, // not clamped here — clamp is caller's job
	}
	for _, tc := range cases {
		got, ok := parseRetryOn4xxLine(tc.line)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseRetryOn4xxLine(%q) = (%d, %v), want (%d, %v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRetryOn4xxConfig_LoadsViaConfigure(t *testing.T) {
	old := loadedRetryOn4xx()
	t.Cleanup(func() { setRetryOn4xx(old) })
	setRetryOn4xx(retryOn4xxDefault)

	// NOTE: []byte fields are base64-encoded by encoding/json, so the
	// config_yaml payload MUST be marshalled from []byte — a raw string
	// JSON literal fails with "illegal base64 data" and configure()
	// silently skips the parse (err == nil guard), leaving the default.
	cfg := func(yaml string) []byte {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
		return raw
	}

	// Valid value flows through configure().
	configure(cfg("retry_on_4xx: 2\n"))
	if got := loadedRetryOn4xx(); got != 2 {
		t.Fatalf("after configure(retry_on_4xx: 2) loaded = %d, want 2", got)
	}

	// Out-of-range values collapse to defaults / max.
	configure(cfg("retry_on_4xx: 99\n"))
	if got := loadedRetryOn4xx(); got != retryOn4xxMax {
		t.Fatalf("after configure(retry_on_4xx: 99) loaded = %d, want %d", got, retryOn4xxMax)
	}

	// Negative / unparseable falls back to default.
	configure(cfg("retry_on_4xx: -7\n"))
	if got := loadedRetryOn4xx(); got != retryOn4xxDefault {
		t.Fatalf("after configure(retry_on_4xx: -7) loaded = %d, want %d", got, retryOn4xxDefault)
	}
	configure(cfg("retry_on_4xx: notanumber\n"))
	if got := loadedRetryOn4xx(); got != retryOn4xxDefault {
		t.Fatalf("after configure(retry_on_4xx: notanumber) loaded = %d, want %d", got, retryOn4xxDefault)
	}

	// Absent key keeps current value.
	setRetryOn4xx(4)
	configure(cfg("checkin_auto: true\n"))
	if got := loadedRetryOn4xx(); got != 4 {
		t.Fatalf("after configure(other-only) loaded = %d, want 4 (unchanged)", got)
	}
}

func TestParseUpstreamStatusFromErr(t *testing.T) {
	cases := []struct {
		errMsg string
		want   int
	}{
		{"upstream 405: method not allowed", 405},
		{"upstream 401: token expired", 401},
		{"upstream 500: server error", 500},
		{"upstream 0: ", 0},
		{"http_error: dial tcp: connection refused", 0}, // not "upstream N:" shape
		{"plain error", 0},
		{"upstream :", 0},      // malformed
		{"upstream abc: x", 0}, // non-numeric
	}
	for _, tc := range cases {
		t.Run(tc.errMsg, func(t *testing.T) {
			err := &fakeErr{msg: tc.errMsg}
			if got := parseUpstreamStatusFromErr(err); got != tc.want {
				t.Fatalf("parseUpstreamStatusFromErr(%q) = %d, want %d", tc.errMsg, got, tc.want)
			}
		})
	}
	t.Run("nil_error", func(t *testing.T) {
		if got := parseUpstreamStatusFromErr(nil); got != 0 {
			t.Fatalf("parseUpstreamStatusFromErr(nil) = %d, want 0", got)
		}
	})
}

type fakeErr struct{ msg string }

func (f *fakeErr) Error() string { return f.msg }
