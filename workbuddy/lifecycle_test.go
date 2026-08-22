package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestShouldActOnCredits(t *testing.T) {
	cases := []struct {
		name string
		cr   *creditsSummary
		want bool
	}{
		{"nil unknown", nil, false},
		{"empty unknown", &creditsSummary{}, false},
		{"remain>0", &creditsSummary{TotalRemain: 1}, false},
		{"exhausted used", &creditsSummary{TotalRemain: 0, TotalUsed: 10}, true},
		{"exhausted packages", &creditsSummary{TotalRemain: 0, Packages: []packageSummary{{Name: "p"}}}, true},
	}
	for _, tc := range cases {
		if got := shouldActOnCredits(tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsHardCreditError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, "", false},
		{500, "internal", false},
		{429, "too many requests", false}, // soft unless credit semantics
		{403, "insufficient credit", true},
		{400, "积分不足", true},
		{402, "payment required", true},
		{403, "额度不足", true},
		{400, "no credit left", true},
		{400, "余额不足", true},
		{403, "credit exhausted", true},
		{429, "rate limit exceeded", false},
		{400, "invalid request", false},
	}
	for _, tc := range cases {
		if got := isHardCreditError(tc.status, tc.body); got != tc.want {
			t.Fatalf("status=%d body=%q: got %v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestIsSoftRateLimit(t *testing.T) {
	if !isSoftRateLimit(429, "too many requests") {
		t.Fatal("429 should be soft")
	}
	if isSoftRateLimit(403, "insufficient credit") {
		t.Fatal("hard credit must not be soft")
	}
	if isSoftRateLimit(500, "error") {
		t.Fatal("5xx not soft rate limit")
	}
}

func TestLifecycleActionFor(t *testing.T) {
	ex := &creditsSummary{TotalRemain: 0, TotalUsed: 5}
	ok := &creditsSummary{TotalRemain: 10, TotalUsed: 1}
	cases := []struct {
		name   string
		region string
		cr     *creditsSummary
		want   lifecycleAction
	}{
		{"cn exhausted", "cn", ex, lifecycleDisable},
		{"global exhausted", "global", ex, lifecycleDelete},
		{"cn ok", "cn", ok, lifecycleNone},
		{"global ok", "global", ok, lifecycleNone},
		{"unknown", "cn", nil, lifecycleNone},
	}
	for _, tc := range cases {
		if got := lifecycleActionFor(tc.region, tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestShouldReenableCN(t *testing.T) {
	if shouldReenableCN(false, &creditsSummary{TotalRemain: 10}) {
		t.Fatal("enabled account should not reenable")
	}
	if shouldReenableCN(true, nil) {
		t.Fatal("unknown credits must not reenable")
	}
	if shouldReenableCN(true, &creditsSummary{TotalRemain: 0, TotalUsed: 5}) {
		t.Fatal("exhausted must not reenable")
	}
	if !shouldReenableCN(true, &creditsSummary{TotalRemain: 3}) {
		t.Fatal("disabled + remain should reenable")
	}
}

func TestDisplayNote(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	gl := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	note := displayNote(cn, &creditsSummary{TotalRemain: 12, TotalUsed: 8, TotalSize: 20}, false)
	if !strings.Contains(note, "CN") || !strings.Contains(note, "12") || !strings.Contains(note, "8") {
		t.Fatalf("cn note = %q", note)
	}
	note = displayNote(gl, &creditsSummary{TotalRemain: 0, TotalUsed: 250, TotalSize: 250}, false)
	if !strings.Contains(note, "Global") || !strings.Contains(note, "耗尽") {
		t.Fatalf("global note = %q", note)
	}
	note = displayNote(cn, &creditsSummary{TotalRemain: 0, TotalUsed: 5}, true)
	if !strings.Contains(note, "禁用") && !strings.Contains(strings.ToLower(note), "disabled") {
		t.Fatalf("disabled note = %q", note)
	}
}

func TestBuildAuthFileJSON_ContainsDisabledAndNote(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", Nickname: "nick"},
	}
	raw, err := buildAuthFileJSON(sa, true, "CN · test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != providerName {
		t.Fatalf("type=%v", m["type"])
	}
	if m["disabled"] != true {
		t.Fatalf("disabled=%v", m["disabled"])
	}
	if m["note"] != "CN · test" {
		t.Fatalf("note=%v", m["note"])
	}
	if m["logo"] == nil || m["logo"] == "" {
		t.Fatal("logo missing")
	}
	auth, _ := m["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "at" {
		t.Fatalf("auth tokens lost: %v", m["auth"])
	}
}

func TestSafeWorkbuddyAuthPath(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-abc.json")
	if err := os.WriteFile(ok, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isSafeWorkbuddyAuthPath(ok) {
		t.Fatalf("expected safe: %s", ok)
	}
	if isSafeWorkbuddyAuthPath(filepath.Join(dir, "other.json")) {
		t.Fatal("other.json must be rejected")
	}
	if isSafeWorkbuddyAuthPath("") {
		t.Fatal("empty rejected")
	}
	if isSafeWorkbuddyAuthPath("/etc/passwd") {
		t.Fatal("passwd rejected")
	}
}

func TestDeleteAuthFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-x.json")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(`{"type":%q}`, providerName)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuthFileAt(p); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	if err := deleteAuthFileAt(p); err != nil {
		t.Fatalf("second delete should be idempotent: %v", err)
	}
}

func TestAuthFileNameFor(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-1"}}
	if got := authFileNameFor(sa); got != "workbuddy-uid-1.json" {
		t.Fatalf("got %q", got)
	}
	if got := authFileNameFor(nil); got != authFileName {
		t.Fatalf("nil got %q", got)
	}
}

func TestParseDisabledFromAuthJSON(t *testing.T) {
	if parseDisabledFromAuthJSON([]byte(`{"disabled":true}`)) != true {
		t.Fatal("want true")
	}
	if parseDisabledFromAuthJSON([]byte(`{"disabled":false}`)) != false {
		t.Fatal("want false")
	}
	if parseDisabledFromAuthJSON([]byte(`{"auth":{}}`)) != false {
		t.Fatal("missing defaults false")
	}
}

func TestLabelForAuth(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{Domain: "www.workbuddy.ai"},
		Account: storedAccount{Nickname: "Bob"},
	}
	got := labelForAuth(sa)
	if !strings.Contains(got, "Bob") || !strings.Contains(got, "Global") {
		t.Fatalf("label=%q", got)
	}
}

func TestIsSafeWorkbuddyAuthPath(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-safe.json")
	if !isSafeWorkbuddyAuthPath(ok) {
		t.Fatalf("want safe: %s", ok)
	}
	legacy := filepath.Join(dir, "workbuddy.json")
	if !isSafeWorkbuddyAuthPath(legacy) {
		t.Fatalf("want legacy safe: %s", legacy)
	}
	bad := filepath.Join(dir, "evil.json")
	if isSafeWorkbuddyAuthPath(bad) {
		t.Fatalf("want unsafe: %s", bad)
	}
	if isSafeWorkbuddyAuthPath("") {
		t.Fatal("empty path must be unsafe")
	}
	// Traversal: explicit ".." in the raw string before filepath.Join cleans it.
	if isSafeWorkbuddyAuthPath("/auth/../workbuddy-x.json") {
		t.Fatal("path with .. must be rejected")
	}
	// /etc/workbuddy-evil.json has valid basename but is NOT under auth dir.
	// isSafeWorkbuddyAuthPath only validates basename + traversal; the
	// directory confinement is enforced by deleteAuthFileInDir + isPathUnder.
	// So this path PASSES isSafe (baseline) but must FAIL deleteAuthFileInDir.
	// (Tested in TestDeleteAuthFileInDir.)
}

func TestIsPathUnder(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-uid.json")
	if !isPathUnder(ok, dir) {
		t.Fatalf("path under dir should pass: %s", ok)
	}
	// Sibling dir
	other := filepath.Join(dir, "..", "other", "workbuddy-uid.json")
	if isPathUnder(other, dir) {
		t.Fatalf("path outside dir should fail: %s", other)
	}
	// Empty dir = no constraint
	if !isPathUnder("/anywhere/workbuddy.json", "") {
		t.Fatal("empty dir should allow")
	}
	// Path is dir itself
	if isPathUnder(dir, dir) {
		t.Fatal("dir itself is not 'under' dir")
	}
}

func TestDeleteAuthFileInDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workbuddy-x.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Delete under correct dir — succeeds.
	if err := deleteAuthFileInDir(target, dir); err != nil {
		t.Fatalf("delete in dir: %v", err)
	}
	// Delete outside dir — rejected (A-23: basename-only was insufficient).
	outside := "/etc/workbuddy-evil.json"
	if err := deleteAuthFileInDir(outside, dir); err == nil {
		t.Fatal("delete outside dir should fail (A-23)")
	}
	// Traversal outside dir
	traversal := filepath.Join(dir, "..", "workbuddy-evil.json")
	if err := deleteAuthFileInDir(traversal, dir); err == nil {
		t.Fatal("delete with .. traversal should fail")
	}
	// Relative path — rejected (A-27: CWD deletion hazard).
	if err := deleteAuthFileInDir("workbuddy-evil.json", ""); err == nil {
		t.Fatal("relative path should fail (A-27)")
	}
	// Unsafe name — rejected.
	if err := deleteAuthFileInDir(filepath.Join(dir, "evil.json"), dir); err == nil {
		t.Fatal("unsafe name should fail")
	}
}

func TestLifecycleActionFor_IdempotentPolicy(t *testing.T) {
	// Applying action twice with same inputs must stay disable/delete (not flip).
	ex := &creditsSummary{TotalRemain: 0, TotalUsed: 1}
	if lifecycleActionFor("cn", ex) != lifecycleDisable {
		t.Fatal("cn")
	}
	if lifecycleActionFor("global", ex) != lifecycleDelete {
		t.Fatal("global")
	}
	// Soft rate limit body never hard-credit alone.
	if isHardCreditError(429, "too many requests") {
		t.Fatal("429 body without credit markers is not hard")
	}
	if !isSoftRateLimit(429, "rate limit") {
		t.Fatal("429 soft")
	}
}

func TestParseDisabledFromAuthJSON_StringTruth(t *testing.T) {
	// Only JSON boolean true counts; string "true" is false for strict parse.
	if parseDisabledFromAuthJSON([]byte(`{"disabled":"true"}`)) {
		t.Fatal("string true should not parse as bool true with current schema")
	}
}

func TestManualDisableFromAuthJSON(t *testing.T) {
	if !manualDisableFromAuthJSON([]byte(`{"disabled":true,"manual_disable":true}`)) {
		t.Fatal("want true")
	}
	if manualDisableFromAuthJSON([]byte(`{"disabled":true}`)) {
		t.Fatal("missing defaults false")
	}
	if manualDisableFromAuthJSON([]byte(`{"manual_disable":"true"}`)) {
		t.Fatal("string true must not count")
	}
	if manualDisableFromAuthJSON(nil) {
		t.Fatal("nil defaults false")
	}
}

func TestMergeAuthDoc_PreservesTopLevel(t *testing.T) {
	// Bug A regression: a token-refresh persist must NOT drop top-level
	// metadata (disabled/note/type/provider/logo/manual_disable) the way the
	// old json.Marshal(sa) whole-file overwrite did.
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: 42, Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "e1", Nickname: "nick"},
	}
	raw, err := mergeAuthDoc([]byte(fmt.Sprintf(`{
		"type":%q,"provider":%q,"logo":"https://x/logo.png",
		"disabled":true,"note":"CN · 手动禁用","manual_disable":true,
		"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1,"domain":"www.codebuddy.cn"},
		"account":{"uid":"u1","enterpriseId":"e1","nickname":"nick"}
	}`, providerName, providerName)), sa)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Top-level fields survive.
	if m["disabled"] != true {
		t.Fatalf("disabled lost: %v", m["disabled"])
	}
	if m["manual_disable"] != true {
		t.Fatalf("manual_disable lost: %v", m["manual_disable"])
	}
	if m["note"] != "CN · 手动禁用" {
		t.Fatalf("note lost: %v", m["note"])
	}
	if m["type"] != providerName || m["provider"] != providerName {
		t.Fatalf("type/provider lost: %v / %v", m["type"], m["provider"])
	}
	if m["logo"] == nil || m["logo"] == "" {
		t.Fatal("logo lost")
	}
	// Nested auth replaced with refreshed values.
	auth, _ := m["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "at-new" || auth["refreshToken"] != "rt-new" {
		t.Fatalf("auth not refreshed: %v", m["auth"])
	}
	if auth["expiresAt"] != float64(42) {
		t.Fatalf("expiresAt not refreshed: %v", auth["expiresAt"])
	}
}

func TestMergeAuthDoc_EmptyRaw(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}
	raw, err := mergeAuthDoc(nil, sa)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	auth, _ := m["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "at" {
		t.Fatalf("empty raw must still carry storage: %v", m)
	}
}

func TestBuildAuthFileJSON_ManualDisableMarker(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}
	// Manual disable path: extra carries the marker.
	raw, err := buildAuthFileJSON(sa, true, "CN · 手动禁用", map[string]any{"manual_disable": true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["manual_disable"] != true {
		t.Fatalf("marker missing: %v", m["manual_disable"])
	}
	// Manual re-enable path: no extra → marker cleared.
	raw2, err := buildAuthFileJSON(sa, false, "CN · 可用", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(raw2, &m2); err != nil {
		t.Fatal(err)
	}
	if _, ok := m2["manual_disable"]; ok {
		t.Fatalf("reenable must clear marker: %v", m2["manual_disable"])
	}
}

func TestWriteAuthFileDirect_BasicAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-test-uid.json")
	first := []byte(`{"disabled":true,"note":"手动禁用"}`)
	if err := writeAuthFileDirect(path, first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(first) {
		t.Fatalf("content mismatch: %s", got)
	}
	// Overwrite semantics: second write fully replaces the file.
	second := []byte(`{"disabled":false}`)
	if err := writeAuthFileDirect(path, second); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after overwrite: %v", err)
	}
	if string(got) != string(second) {
		t.Fatalf("overwrite mismatch: %s", got)
	}
	// No leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteAuthFileDirect_RejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	// Literal ".." must survive into the path string — filepath.Join would
	// Clean it away and defeat the traversal check.
	traversal := filepath.Join(dir, "dummy") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "workbuddy-esc.json"
	cases := []string{
		"",                                            // empty
		filepath.Join(dir, "other-provider.json"),     // wrong prefix
		filepath.Join(dir, "workbuddy-evil.txt"),      // wrong suffix
		traversal,                                     // literal .. traversal
		"workbuddy-relative.json",                     // relative path
		filepath.Join(dir, "workbuddy.json"),          // canonical legacy name is allowed shape
	}
	// The last case is expected to SUCCEED (legacy canonical name is legal).
	for i, p := range cases[:len(cases)-1] {
		if err := writeAuthFileDirect(p, []byte(`{}`)); err == nil {
			t.Errorf("case %d (%q): expected error, got nil", i, p)
		}
	}
	// Legacy canonical name must still work.
	if err := writeAuthFileDirect(cases[len(cases)-1], []byte(`{}`)); err != nil {
		t.Fatalf("legacy canonical name rejected: %v", err)
	}
	if _, err := os.Stat(cases[len(cases)-1]); err != nil {
		t.Fatalf("legacy canonical file missing: %v", err)
	}
}

func TestListEntryMatchesUID(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	want := "workbuddy-" + uid + ".json"
	cases := []struct {
		name string
		f    pluginapi.HostAuthFileEntry
		want bool
	}{
		{"name exact", pluginapi.HostAuthFileEntry{Name: want}, true},
		{"id exact", pluginapi.HostAuthFileEntry{ID: want}, true},
		{"basename id", pluginapi.HostAuthFileEntry{Name: "workbuddy-" + uid}, true},
		{"case", pluginapi.HostAuthFileEntry{Name: strings.ToUpper(want)}, true},
		{"other uid", pluginapi.HostAuthFileEntry{Name: "workbuddy-other.json"}, false},
		{"legacy bare", pluginapi.HostAuthFileEntry{Name: "workbuddy.json"}, false},
		{"empty uid", pluginapi.HostAuthFileEntry{Name: want}, false},
	}
	for _, tc := range cases {
		u := uid
		if tc.name == "empty uid" {
			u = ""
		}
		got := listEntryMatchesUID(tc.f, u, want)
		if got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
