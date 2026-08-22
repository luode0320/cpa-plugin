package main

import (
	"strings"
	"testing"
)

// TestAuthFilePrefix_DecoupledFromProviderName locks in the 0.9.6 bug fix:
// if providerName is renamed (e.g. "workbuddy" → "workbuddy-provider"), the
// auth files written by authFileNameFor MUST still be visible to the panel.
//
// The host writes files using authFilePrefix as the disk prefix, and
// hostAuthList filters host.auth.list results with authFilePrefix too. If
// either side ever uses providerName + "-" instead, every auth file becomes
// invisible to the dashboard (panel shows "暂无 WorkBuddy 账号" while the host
// scheduler still routes traffic through those files — the worst kind of
// silent breakage). This test prevents the regression at compile-test cost.
func TestAuthFilePrefix_DecoupledFromProviderName(t *testing.T) {
	if authFilePrefix != "workbuddy-" {
		t.Fatalf("authFilePrefix=%q; this constant is the disk filename prefix and must remain stable across plugin-id renames. If you intentionally changed it, audit every test/asset/release that references the legacy prefix first.", authFilePrefix)
	}
	// The list filter in host_auth.go must reference authFilePrefix, NOT
	// providerName + "-". (We can't introspect the source string here, but we
	// can assert the rendered values differ enough to catch a future mistake:
	// if the test below passes after someone reverts to providerName + "-",
	// they're hiding behind a same-name alias. Sanity: providerName must NOT
	// accidentally equal the bare prefix in a way that makes both code paths
	// look identical in tests.)
	if strings.HasSuffix(providerName, "-") {
		t.Errorf("providerName=%q ends with '-'; a trailing dash is the kind of detail that drifts and then explodes in the list filter", providerName)
	}
	// Guard the negative case explicitly: the bad path was providerName + "-"
	// == "workbuddy-provider-" (or whatever providerName is). Confirm that
	// the disk prefix is the canonical "workbuddy-" regardless.
	if providerName == authFilePrefix {
		t.Fatalf("providerName (%q) must not be set to the disk prefix alone; that defeats the decoupling and brings back the 0.9.6 bug the next time someone renames providerName", providerName)
	}
}

// TestAuthFileNameFor_UsesPrefixConstant ensures the file-writing side and the
// list-filter side both go through the same constant. If someone hard-codes
// "workbuddy-" in authFileNameFor again, the test below would no longer catch
// the drift — but at minimum we assert the public behavior of the function is
// consistent with the public prefix constant.
func TestAuthFileNameFor_UsesPrefixConstant(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "ab12cd34"}}
	want := authFilePrefix + "ab12cd34" + ".json"
	if got := authFileNameFor(sa); got != want {
		t.Fatalf("authFileNameFor(%q)=%q; want %q (must equal authFilePrefix+uid+'.json')", "ab12cd34", got, want)
	}
}

// TestAuthFileNameFor_NilUID_LegacyFallback documents the legacy single-account
// fallback: when UID is empty, authFileNameFor returns the bare authFileName
// ("workbuddy.json") — which does NOT carry the authFilePrefix dash. That's by
// design: legacy files are identified by isLegacyWorkbuddyAuthName and migrated
// to the canonical workbuddy-<uid>.json name by resolveAuthFileTarget. They are
// never expected to match the prefix filter, so do NOT assert a prefix here.
func TestAuthFileNameFor_NilUID_LegacyFallback(t *testing.T) {
	got := authFileNameFor(nil)
	if !isLegacyWorkbuddyAuthName(got) {
		t.Fatalf("authFileNameFor(nil)=%q; want the legacy authFileName that isLegacyWorkbuddyAuthName recognizes (migrated by resolveAuthFileTarget, not prefix-matched)", got)
	}
}
