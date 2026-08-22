// host_auth.go wraps the host's auth-store RPC (host.auth.list / get /
// get_bundle). These are the only paths the plugin uses to read auth files;
// writes go through persistAuthDirect (physical file) so the host's file
// watcher — not host.auth.save — syncs disabled state into the scheduler.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all workbuddy credentials known to the host.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	// Fresh slice — resp.Files[:0] would alias the RPC response's backing
	// array (P1-3: fragile pattern, safe today but could break if resp is
	// ever cached/reused).
	//
	// Filter by filename prefix, NOT by Type/Provider: many existing auth
	// files on disk don't carry a "type"/"provider" field (they were written
	// before that convention), and EqualFold("", providerName) returns false
	// for them — meaning we'd incorrectly exclude files that have the
	// authFilePrefix prefix but no type field. Filename prefix is the only
	// reliable cross-version discriminator.
	//
	// The prefix MUST match authFilePrefix from authfile.go — never use
	// providerName + "-" here. Decoupling the two prevents a plugin-id
	// rename from silently stranding every auth file on disk.
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	prefix := authFilePrefix
	for _, f := range resp.Files {
		if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			out = append(out, f)
		}
	}
	return out, nil
}

// hostAuthGet fetches the credential JSON for one auth index.
func hostAuthGet(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// hostAuthGetBundle is one host.auth.get for both storage and physical metadata
// (avoids the previous double-RPC in dashboard: get + getPhysical).
func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}

// waitAuthDisabledState polls host.auth.list until the account's in-memory
// disabled state matches want. The host applies direct file writes through its
// async file watcher, so a toggle must confirm the new state before the panel
// reloads — otherwise the dashboard reads the stale pre-write memory state.
// Returns true when confirmed, false on timeout.
func waitAuthDisabledState(authIndex string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		files, err := hostAuthList()
		if err == nil {
			for _, f := range files {
				if f.AuthIndex != authIndex {
					continue
				}
				if f.Disabled == want {
					return true
				}
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
