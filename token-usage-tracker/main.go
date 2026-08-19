// Package main implements the token-usage-tracker CLIProxyAPI dynamic plugin.
//
// token-usage-tracker is the third plugin of the cpa-plugin project (beside
// workbuddy and qoderwork). It records and visualizes real token consumption
// of workbuddy accounts.
//
// Data path: workbuddy appends one NDJSON line per completed request to the
// shared usage feed (<CLIProxyAPI root>/data/token-usage-feed.ndjson by
// default). This plugin tails that file into its own bbolt database and
// serves the embedded dashboard. The core statistics stack (usage_stats
// subpackage) is ported from AITNR/cap-token-usage-tracker.
//
// Capabilities: ManagementAPI only (no model/auth/executor/scheduler
// capabilities). The dashboard page and read API are served as resource
// routes; price/reset/backup/restore writes are management routes behind the
// plugin management_key gate.
//
// This file is built with -buildmode=c-shared and exports the cliproxy C ABI
// entry points (same ABI as workbuddy/qoderwork).
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName  = "token-usage-tracker"
	pluginLogoURL = "https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/TokenTracker.png"
)

var (
	hostAPI *C.cliproxy_host_api // captured at init (unused: no host RPCs today)
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// No-op: touching Go runtime state during host teardown risks SIGSEGV in
	// cgo (same rationale as workbuddy/qoderwork). The importer goroutine and
	// ticker hold no resources that outlive the process.
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	FrontendAuthProvider  bool                         `json:"frontend_auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	Scheduler             bool                         `json:"scheduler"`
	ManagementAPI         bool                         `json:"management_api"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}

// version is injected at build time via -ldflags "-X main.version=...".
var version = "0.1.0"

func trackerRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Author:           "luode (statistics core ported from AITNR/cap-token-usage-tracker)",
			GitHubRepository: "https://github.com/luode0320/cpa-plugin",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer token required for write endpoints (prices/reset/backup/restore). Empty disables the plugin-layer gate. Also env TOKEN_USAGE_TRACKER_MANAGEMENT_KEY."},
				{Name: "usage_feed_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Tail the shared usage feed produced by the workbuddy plugin (default true)."},
				{Name: "usage_feed_path", Type: pluginapi.ConfigFieldTypeString, Description: "Shared usage feed path (default <CLIProxyAPI root>/data/token-usage-feed.ndjson). Must match workbuddy's usage_feed_path."},
				{Name: "usage_db_path", Type: pluginapi.ConfigFieldTypeString, Description: "Optional bbolt database path (default <CLIProxyAPI root>/data/token-usage-tracker.db)."},
				{Name: "usage_retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Statistics retention in days (1-3650, default 365)."},
				{Name: "usage_flush_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Database flush interval, Go duration (1s-1h, default 5s)."},
				{Name: "usage_flush_max_records", Type: pluginapi.ConfigFieldTypeInteger, Description: "Max records buffered before forcing a flush (1-1000000, default 100)."},
				{Name: "usage_poll_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Feed poll interval, Go duration (1s-1h, default 5s)."},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI: true,
		},
	}
}

// -----------------------------------------------------------------------------
// Method dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		return okEnvelope(trackerRegistration())
	case pluginabi.MethodManagementRegister:
		// Cache host-injected BasePath so handleManagement doesn't hardcode
		// /v0/management.
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			if regReq.BasePath != "" {
				setManagementBasePath(regReq.BasePath)
			}
		}
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Envelope helpers
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil {
		return
	}
	if len(raw) == 0 {
		response.ptr = nil
		response.len = 0
		return
	}
	ptr := C.CBytes(raw)
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// -----------------------------------------------------------------------------
// Unused host-call scaffolding (kept for parity with the sibling plugins so a
// future host RPC needs no ABI changes; no calls are made today).
// -----------------------------------------------------------------------------

func hostCall(method string, request []byte) ([]byte, error) {
	return nil, fmt.Errorf("hostCall: method %s is not supported by %s", method, providerName)
}
