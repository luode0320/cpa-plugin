# WorkBuddy Plugin Architecture

Module map and data flow for the workbuddy plugin. The plugin is a single
`package main` compiled as a c-shared `.so`, loaded by CPA at startup and
driven via the `pluginabi` RPC interface.

## Capability surface (declared in `wbRegistration`)

| Capability | Implementation file | What it does |
|---|---|---|
| `ModelProvider` | `models.go` | Static + dynamic model list, alias reverse-resolution, `oauth-excluded-models` filter |
| `AuthProvider` | `oauth.go`, `auth_parse.go` (in `authfile.go` / `main.go`) | OAuth login flow (CN + Global), token refresh, auth file parse |
| `Executor` | `executor.go`, `stream.go`, `payload.go` | Chat completions, streaming SSE pump, request body rewriting |
| `Scheduler` | `scheduler.go`, `active_auth.go` | Optional panel-selected account routing (`scheduler_mode: credits`) |
| `ManagementAPI` | `management.go`, `panel.go`, `checkin.go`, `credits_handler.go`, `billing.go`, `usage_config.go`, `host_auth.go` | Dashboard, manual check-in, credits query, import credential, config |
| `UsagePlugin` | `usage.go` | Forward every request's usage record to CPAMP |
| `Local usage stats` | `usage_stats/` (subpackage, merged from cap-token-usage-tracker), `usage_stats_bridge.go` | Per-request token usage persisted to local bbolt from the executor chain (`publishUsage`), dashboard page + read/write API under `/usage` |

## File map (by responsibility)

```
main.go           C ABI exports + handleMethod dispatch + registration
registration.go   (in main.go) wbRegistration + capabilities + ConfigField
envelope.go       (in main.go) envelope/okEnvelope/errorEnvelope helpers

host_call.go      hostCall + hostBridgeUnwrap (RPC to CPA host)
host_bridge.go    hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks

executor.go       handleExecExecute / handleExecStream
stream.go         streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
payload.go        prepareUpstreamBody + InPlace mutators (forceStream/normalizeTools/
                  rewriteSystem/ensureSystemMessage/rewriteModel) + legacy wrappers

models.go         callModelsAPI + fetchDynamicModels + cacheModelAliases +
                  resolveUpstreamModel + parseModelAliasAttribute + filterExcludedModels

oauth.go          handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
auth_parse.go     (in authfile.go / main.go) handleParseAuth + parseStored + toAuthData

usage.go          handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector

management.go     managementRegistration + handleManagement + auth/ratelimit
panel.go          buildDashboardEx + summarizeCredits + servePanel + panelHTML
checkin.go        schedulerLoop + runAutoCheckin + handleManualCheckin + 
                  classifyCheckinTargets/executeCheckinBatch/summarizeCheckinResults
credits_handler.go handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
billing.go        fetchCheckinStatus/fetchUserResource/fetchPaymentType/
                  performCheckinCall/performTrialCall + JSON helpers
usage_config.go   configure + resolveUsageReport + probe* + config vars
host_auth.go      hostAuthList/Get/GetBundle (host auth-store RPC)

lifecycle.go      reconcileOneAccount/AllAccounts/AfterExecutorError/ByUID +
                  applyExhaustedPolicy + lifecycleState
policy.go         lifecycleAction decisions (pure functions) + displayNote + labelForAuth
authfile.go       authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth +
                  path safety checks

scheduler.go      handleSchedulerPick + candidateDisabled + cachedCreditsScore
active_auth.go    activeAuthID sticky state + pickActiveAuth + clearActiveAuthIfMatch

cache.go          accountCache + accountDetailFlight singleflight + prune
redact.go         redactSecrets + 4 regex + truncateRedacted + truncate
headers.go        (in main.go / oauth.go) commonHeaders/backendHeaders/billingHeaders
stored.go         (in main.go / models.go) storedAuth/storedTokens/storedAccount
```

## Data flow

### Chat completion (streaming)

```
client → CPA → plugin.handleExecStream
  → parseStored(auth file)
  → resolveUpstreamModel(alias → upstream id)
  → prepareUpstreamBody (single JSON pass: forceStream + normalizeTools +
                          rewriteSystem + ensureSystemMessage + rewriteModel)
  → hostHTTPDoStream (via CPA host bridge → request-log captured)
  → pumpUpstreamStream (goroutine)
      → hostStreamReader → bufio.Scanner → SSE lines
      → cleanChunkJSON per line
      → streamEmit → CPA → client
      → sseUsageCollector collects terminal usage object
  → publishUsage → forwardUsageToCPAMP (async, via host bridge)
  → invalidateAccountCredits (async)
  → host calls UsagePlugin.HandleUsage → handleUsage → forwardUsageToCPAMP (sync)
```

### Daily check-in (CN, 09:00 / 21:00)

```
schedulerLoop → runAutoCheckin (sem=4 concurrent)
  → processAutoCheckinAccount per account
      → fetchCheckinStatus → performCheckinCall if needed
      → update accountCache (merge, not wipe)
      → reconcileOneAccount → applyExhaustedPolicy
          → policy.go decides: disable (CN) / delete (Global) / reenable (CN)
          → authfile.go applies: hostAuthPersist / deleteAuth
```

### Dashboard load

```
panel.html → /v0/management/plugins/workbuddy/accounts
  → handleManagement (auth + ratelimit)
  → buildDashboardEx (concurrent cachedAccountDetails per account, sem=4)
      → accountDetailFlight singleflight dedups concurrent fetches
      → accountCache hit → return cached
      → miss → 3 concurrent billing API calls (plan/checkin/credits)
  → summarizeCredits
```

## Key design decisions

1. **Host HTTP bridge for all upstream calls.** Every HTTP request to
   CodeBuddy / CPAMP goes through `host.http.do` / `host.http.do_stream` so
   CPA's request-log captures outbound traffic and host transport policy
   (proxy, timeout) applies. The plugin's own `sharedHTTPClient` is a
   fallback used only when the bridge is unavailable (unit tests, hosts
   older than v7.2.x).

2. **Single-flight per account for billing API.** `cachedAccountDetails`
   uses a `sync.Map` of in-flight calls so concurrent dashboard refreshes
   and reconcile ticks for the same account share one upstream fetch
   instead of stampeding the billing API.

3. **Cache merge, never wipe.** All cache writes merge with the previous
   entry (credits + plan + checkin) instead of replacing it. The "early
   already checked in" fast path used to wipe credits/plan; v0.6.31 fixed
   that by always merging.

4. **UID whitelist for auth file names.** `sanitizeUIDForFileName` strips
   any character outside `[a-zA-Z0-9_-]` and caps length at 64, preventing
   path traversal when importing credentials with attacker-controlled UIDs.

5. **Plugin-layer management auth is opt-in.** When `management_key` is
   unset the plugin defers entirely to CPA's management middleware
   (historical default). When set, mutating endpoints require a constant-time
   Bearer match plus a per-IP token bucket.

6. **Scheduler defers by default.** `scheduler_mode: off` (default) makes
   `handleSchedulerPick` always return `Handled: false` so CPA's built-in
   scheduler picks accounts. The plugin only routes when the operator
   explicitly opts in with `scheduler_mode: credits`.

7. **No goroutine leaks across hot-reload.** The scheduler loop uses a
   `schedulerStop` channel and is idempotent. The plugin's `Shutdown` is a
   deliberate no-op because c-shared runtime teardown races with Go sync
   primitives (SIGSEGV) — `dlclose` cleans up the whole runtime anyway.

## Integration points with CPA

- **Auth store**: `host.auth.list` / `host.auth.get` / `host.auth.save` —
  plugin never writes auth files directly to disk, always via host RPC.
- **Model registration**: `model.static` / `model.for_auth` RPC, plus
  `oauth-model-alias` / `oauth-excluded-models` from host config.
- **Streaming**: `host.stream.emit` / `host.stream.close` — async SSE
  chunks pushed to the client without blocking the executor return.
- **Usage**: `usage.handle` RPC — host calls `UsagePlugin.HandleUsage`
  after every request with a canonical `pluginapi.UsageRecord`.
- **Management**: `management.register` returns routes under
  `/v0/management/plugins/workbuddy/*` and panel resources under
  `/v0/resource/plugins/workbuddy/panel` (credits) and
  `/v0/resource/plugins/workbuddy/usage` (token usage statistics).
- **Local usage stats**: the merged tracker stack records each request inside
  `publishUsage` (`usage.go`) into a local bbolt DB via `usage_stats.Store`
  (actor channel, async flush — never blocks the executor). The dashboard
  page reads through the resource route (`/stats`, `/requests`, `/costs`,
  `/prices`, `/preferences`, `/exchange-rate`); write endpoints (`/prices`,
  `/reset`, `/backup`, `/restore`) are mounted on the management route and
  pass the same `management_key` gate.
- **Scheduler**: `scheduler.pick` RPC — plugin returns `Handled: true` with
  an `AuthID` only when `scheduler_mode: credits` and a valid candidate
  exists; otherwise defers.
