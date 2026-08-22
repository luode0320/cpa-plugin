package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Regression: CPA upsert key is auth.ID (or path-derived ID when empty).
// Returning UID as ID while the host registered with filename-ID creates
// duplicate in-memory auth records for the same on-disk file.

func sampleNestedAuthJSON(uid string) []byte {
	return []byte(fmt.Sprintf(`{
  "type": %q,
  "auth": {
    "accessToken": "at",
    "refreshToken": "rt",
    "domain": "www.codebuddy.cn",
    "expiresAt": 9999999999
  },
  "account": {
    "uid": "` + uid + `",
    "nickname": "nick"
  }
}`, providerName))
}

func decodeParseAuth(t *testing.T, raw []byte) pluginapi.AuthParseResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
	var resp pluginapi.AuthParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	return resp
}

func TestHandleParseAuth_EchoesFileNameAndEmptyID(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/root/.cli-proxy-api/workbuddy-" + uid + ".json",
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  sampleNestedAuthJSON(uid),
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("expected handled")
	}
	if resp.Auth.FileName != req.FileName {
		t.Fatalf("FileName=%q want host %q", resp.Auth.FileName, req.FileName)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("ID=%q want empty so host uses path-based id", resp.Auth.ID)
	}
	if resp.Auth.Provider != providerName {
		t.Fatalf("provider=%q", resp.Auth.Provider)
	}
}

func TestHandleParseAuth_LegacyWorkbuddyJSON_KeepsHostFileName(t *testing.T) {
	// Even when storage has a UID, parse must not rename to workbuddy-<uid>.json.
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/root/.cli-proxy-api/workbuddy.json",
		FileName: "workbuddy.json",
		RawJSON:  sampleNestedAuthJSON(uid),
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if resp.Auth.FileName != "workbuddy.json" {
		t.Fatalf("FileName=%q want workbuddy.json (no rename)", resp.Auth.FileName)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("ID must be empty, got %q", resp.Auth.ID)
	}
}

func TestHandleParseAuth_UnhandledForNonWorkbuddy(t *testing.T) {
	req := pluginapi.AuthParseRequest{
		Provider: "codex",
		FileName: "codex.json",
		RawJSON:  []byte(`{"type":"codex","token":"x"}`),
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if resp.Handled {
		t.Fatal("non-workbuddy must not be handled")
	}
}

func TestToAuthDataForRefresh_EmptyFileNameAndID(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "a", RefreshToken: "r", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u-1", Nickname: "n"},
	}
	ad := toAuthDataForRefresh(sa)
	if ad.FileName != "" {
		t.Fatalf("FileName=%q want empty", ad.FileName)
	}
	if ad.ID != "" {
		t.Fatalf("ID=%q want empty", ad.ID)
	}
	// Storage still carries uid for runtime use
	var nested map[string]any
	if err := json.Unmarshal(ad.StorageJSON, &nested); err != nil {
		t.Fatal(err)
	}
}

func TestToAuthData_LoginImportStillSetsUIDFileName(t *testing.T) {
	// New accounts (login/import) intentionally use workbuddy-<uid>.json
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "a", RefreshToken: "r", Domain: "www.workbuddy.ai"},
		Account: storedAccount{UID: "abc-uid", Nickname: "bob"},
	}
	ad := toAuthData(sa)
	if ad.ID != "abc-uid" {
		t.Fatalf("login ID=%q want uid", ad.ID)
	}
	if ad.FileName != "workbuddy-abc-uid.json" {
		t.Fatalf("login FileName=%q", ad.FileName)
	}
}
