// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	// scheduler default: session (per-conversation round-robin). Explicit
	// scheduler_mode in config_yaml overrides it; unknown values fall back to
	// this default (NOT off) so multi-account deployments get spread routing
	// out of the box.
	nextSchedulerMode := schedulerModeSession
	nextKeepaliveAuto := true
	nextMgmtKey := ""
	// failover default: enabled. Explicit account_failover: false disables the
	// whole cooldown mechanism (pre-failover behavior).
	nextFailoverEnabled := true

	// Preserve watchdog defaults; overridden by config_yaml.
	nextPreserveThreshold := preserveThresholdDefault
	nextPreserveInterval := preserveWatchdogIntervalDefault
	nextPreserveEnabled := preserveWatchdogEnabledDefault

	// retry_on_4xx: per-request account-failover budget. Applied ONLY when
	// the key is present in config_yaml: a valid value is clamped to
	// [0, 5] and applied; an unparseable value resets to the default 3.
	// An absent key keeps the current budget so an unrelated reconfigure
	// never silently lifts a `retry_on_4xx: 0` kill switch.
	nextRetryOn4xx := retryOn4xxDefault
	retryOn4xxSeen := false

	cfgURL, cfgKey := "", ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "checkin_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "checkin_auto:"))
					nextCheckinAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "lifecycle_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "lifecycle_auto:"))
					v = strings.Trim(v, "\"'")
					nextLifecycleAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "account_failover:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "account_failover:"))
					v = strings.Trim(v, "\"'")
					nextFailoverEnabled = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "scheduler_mode:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "scheduler_mode:"))
					v = strings.Trim(v, "\"'")
					if v == schedulerModeCredits {
						nextSchedulerMode = schedulerModeCredits
					} else if v == schedulerModeSession {
						nextSchedulerMode = schedulerModeSession
					}
				}
				if strings.HasPrefix(line, "usage_report_url:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_url:"))
					cfgURL = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "usage_report_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_key:"))
					cfgKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "management_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "management_key:"))
					nextMgmtKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "token_keepalive:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "token_keepalive:"))
					v = strings.Trim(v, "\"'")
					nextKeepaliveAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				// Preserve watchdog knobs: a credit-balance threshold below
				// which an account is parked in the preserve set; the tick
				// interval (Go duration syntax, e.g. "10m"); and the
				// enable/disable switch.
				if strings.HasPrefix(line, "preserve_threshold:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "preserve_threshold:"))
					v = strings.Trim(v, "\"'")
					if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
						nextPreserveThreshold = n
					}
				}
				if strings.HasPrefix(line, "preserve_watchdog_interval:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "preserve_watchdog_interval:"))
					v = strings.Trim(v, "\"'")
					if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
						nextPreserveInterval = d
					}
				}
				if strings.HasPrefix(line, "preserve_watchdog_enabled:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "preserve_watchdog_enabled:"))
					v = strings.Trim(v, "\"'")
					nextPreserveEnabled = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "retry_on_4xx:") {
					retryOn4xxSeen = true
					if n, ok := parseRetryOn4xxLine(line); ok {
						nextRetryOn4xx = clampRetryOn4xx(n)
					}
				}
			}
		}
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	setFailoverEnabled(nextFailoverEnabled)

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	resolveUsageReport(cfgURL, cfgKey)
	ensureScheduler()

	// Preserve watchdog: apply threshold/interval/enabled under their own
	// locks. The watchdog loop reads these on every iteration, so changes
	// take effect at the next tick without restarting the goroutine.
	setPreserveConfig(nextPreserveThreshold, nextPreserveInterval, nextPreserveEnabled)

	// Drop a non-blocking tick request so the freshly (re)configured
	// threshold/interval apply immediately rather than after a full interval
	// (v0.12.1: closes the "register just changed something, why does the
	// badge still show the old state?" UX gap). Reconfigure storms collapse
	// onto one tick via requestPreserveTick's buffered chan cap 1.
	requestPreserveTick()

	// Per-request retry-on-4xx budget: applied under its own lock so
	// executor loops reading it (loadedRetryOn4xx) stay atomic. Only when
	// the key was present — absent keeps the current budget (kill-switch
	// safety).
	if retryOn4xxSeen {
		setRetryOn4xx(nextRetryOn4xx)
	}

	// Shared usage feed for the standalone token-usage-tracker plugin.
	// Parses usage_feed_* fields from the same config_yaml. Non-fatal by
	// design: a failure only disables the feed, never chat.
	configureUsageFeed(raw)
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
	if url == "" {
		url = probeUsageReportURL()
	}
	key := firstNonEmpty(
		strings.TrimSpace(cfgKey),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_KEY")),
		strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		strings.TrimSpace(os.Getenv("CPA_MANAGER_ADMIN_KEY")),
		readSecretFile(os.Getenv("USAGE_REPORT_KEY_FILE")),
		readSecretFile(os.Getenv("CPAMP_ADMIN_KEY_FILE")),
		readSecretFile(os.Getenv("CPA_MANAGER_ADMIN_KEY_FILE")),
		// docker compose secrets default path
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
		// optional bind-mounts used on this host
		readSecretFile("/CLIProxyAPI/secrets/cpamp-admin-key"),
		readSecretFile("/CLIProxyAPI/secrets/cpamp_admin_key"),
	)
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
}

// probeUsageReportURL tries localhost first (bare-metal + Docker host-network),
// then Docker compose service name. Returns whichever responds; defaults to
// localhost if both fail (better to try localhost than an unreachable hostname).
func probeUsageReportURL() string {
	for _, candidate := range []string{defaultUsageReportURL, fallbackUsageReportURL} {
		if probeURL(candidate, 2*time.Second) {
			return candidate
		}
	}
	return defaultUsageReportURL
}

// probeURL does a quick HEAD/GET to check if the endpoint is reachable.
func probeURL(target string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any HTTP response (even 401) means the endpoint is reachable;
	// connection refused / DNS failure means not reachable.
	return resp.StatusCode > 0
}

func readSecretFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
