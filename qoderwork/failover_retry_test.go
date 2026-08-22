package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPickNextAuth_NoCurrent(t *testing.T) {
	// pickNextAuth always filters out its currentAuthID arg, so passing
	// the empty string sweeps the whole list — and we have no host RPC
	// available here, so the call must return ok=false without panicking.
	if _, _, ok := pickNextAuth(""); ok {
		t.Fatal("pickNextAuth must return ok=false when host RPC is unavailable")
	}
}

func TestReadAllUpstreamErr(t *testing.T) {
	cases := []struct {
		name string
		in   io.Reader
		want string
	}{
		{"nil reader", nil, ""},
		{"error reader", &errReader{}, ""},
		{"empty body", strings.NewReader(""), ""},
		{"plain text", strings.NewReader("Method Not Allowed"), "Method Not Allowed"},
		{"json body", strings.NewReader(`{"error":"forbidden"}`), `{"error":"forbidden"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readAllUpstreamErr(tc.in); got != tc.want {
				t.Fatalf("readAllUpstreamErr(%v) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestRebuildRequestWithQoderAuth_NilAndEmptyToken(t *testing.T) {
	if _, err := rebuildRequestWithQoderAuth(nil, "body", "model"); err == nil {
		t.Fatal("rebuildRequestWithQoderAuth(nil, ...) must return an error")
	}
	// Empty access token → cosy session build fails fast.
	empty := &storedAuth{Auth: storedTokens{AccessToken: ""}}
	if _, err := rebuildRequestWithQoderAuth(empty, "body", "model"); err == nil {
		t.Fatal("rebuildRequestWithQoderAuth with empty token must return an error")
	}
}

func TestRebuildRequestWithQoderAuth_BuildsFreshRequest(t *testing.T) {
	sa := &storedAuth{
		Auth: storedTokens{AccessToken: "jt-test-token", RefreshToken: "jrt-test-refresh"},
		Account: storedAccount{
			UID:      "acc-1",
			Nickname: "test account",
		},
	}
	req, err := rebuildRequestWithQoderAuth(sa, `{"q":1}`, "qoder/qmodel_preview")
	if err != nil {
		t.Fatalf("rebuildRequestWithQoderAuth failed: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.String() != endpointChat {
		t.Fatalf("url = %s, want endpointChat", req.URL.String())
	}
	if req.Header.Get("x-model-key") != "qoder/qmodel_preview" {
		t.Fatalf("x-model-key = %q, want qoder/qmodel_preview", req.Header.Get("x-model-key"))
	}
	if req.Header.Get("Content-Type") == "" {
		t.Fatal("content-type header must be set by COSY headers")
	}
}

type errReader struct{}

func (e *errReader) Read(_ []byte) (int, error) { return 0, errors.New("simulated read error") }
