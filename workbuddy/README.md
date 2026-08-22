# WorkBuddy Plugin for CLIProxyAPI

A [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) plugin that
provides **Tencent CodeBuddy** (`copilot.tencent.com` CN and `workbuddy.ai`
Global) as a native OAuth provider: dynamic model discovery, streaming executor,
credit-aware scheduling, daily check-in automation, and a built-in management
dashboard.

[中文文档 → README_CN.md](README_CN.md)

## Features

- **OAuth login** — multi-account `workbuddy-<uid>.json` auth files via the
  host's auth store. CN and Global realms share one plugin, one config block.
- **Dynamic models** — live model list from the upstream models API with a
  5-minute cache and a static fallback. Host-side `oauth-model-alias` /
  `oauth-excluded-models` config applies unchanged.
- **Executor** — OpenAI-compatible chat completions, both streaming (real SSE
  via `host.stream.emit`) and non-streaming (SSE folded into a single
  completion). `tool_choice` normalization, Claude Code template sanitization,
  and per-realm system-message injection are built in.
- **Credit lifecycle** — CN accounts auto-`disabled` when credits run out and
  re-enabled when a check-in restores them. Global accounts are deleted on
  exhaustion (one-shot trial quota). Hard credit errors from the executor
  trigger an immediate reconcile.
- **Daily check-in** — CN accounts are checked in at 09:00 and 21:00 local
  time (configurable). Manual "check in all" from the panel. Per-account
  mutex prevents duplicate claims from racing browser tabs.
- **Trial claim** — Global accounts can claim the one-time 250-credit expert
  trial pack from the panel.
- **Dashboard** — embedded panel at `/v0/resource/plugins/workbuddy/panel`
  with credits progress bars, plan badges, exhausted/disabled flags, region
  filter, and credential import.
- **Token usage feed** — every request's token consumption (input / output
  / reasoning / cache) is appended as one NDJSON line to a shared feed at
  `<CLIProxyAPI root>/data/token-usage-feed.ndjson`. The standalone companion
  plugin `token-usage-tracker` (install from the same registry) tails that
  feed into its own database and serves the dashboard (menu "Token 用量",
  `/v0/resource/plugins/token-usage-tracker/usage`) with trends,
  per-model/account breakdowns, request history and cost estimates. This is
  the replacement for the v0.8.8 in-plugin statistics, which was reverted:
  the host's `UsagePlugin` broadcast never fires for plugin executors and two
  long-lived processes cannot share one bbolt file, so a file feed is the
  only clean cross-plugin data path.
- **Scheduler** (optional) — `scheduler_mode` defaults to `session`: conversations
  spread across accounts (same conversation stays on one account for up to 1h).
  `credits` pins to the panel-selected account; `off` defers to CPA's built-in
  scheduler entirely.
- **Usage forwarding** — implements `UsagePlugin`; every request's usage
  record is forwarded to a configurable CPAMP endpoint. No record is sent
  unless a URL+key are configured.

## Quickstart

### 1. Install the plugin

Drop the compiled `workbuddy.so` into CPA's plugin directory:

```bash
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

For multi-arch deployments use the platform subdirectory convention:

```
plugins/
  linux/amd64/workbuddy.so
  linux/arm64/workbuddy.so
  darwin/arm64/workbuddy.so
```

### 2. Enable in `config.yaml`

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. Sign in

Open the WorkBuddy panel from CPA's sidebar (or hit
`/v0/resource/plugins/workbuddy/panel` directly) and click **登录** to start
the OAuth flow. Repeat for each account you want to add — the plugin writes
one `workbuddy-<uid>.json` per account to the auth store.

### 4. Use it

Call the OpenAI-compatible endpoint with any alias that maps to a workbuddy
model:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "point/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## Configuration

All fields are optional and live under `plugins.configs.workbuddy`.

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # Daily check-in automation for CN accounts (default true).
      # Runs at 09:00 and 21:00 local time.
      checkin_auto: true

      # Credit lifecycle: disable CN on exhaust, delete Global on exhaust,
      # re-enable CN after check-in restores credits (default true).
      lifecycle_auto: true

      # Scheduler behavior (default "session"):
      #   session → per-conversation round-robin: same conversation stays on
      #             one account for up to 1h; conversations spread across
      #             accounts; requests without a session identity fall back
      #             to the panel-selected account
      #   credits → plugin picks the panel-selected account (with fallback
      #             when that account is exhausted / disabled)
      #   off     → defer to CPA's built-in scheduler entirely
      scheduler_mode: "session"

      # Preserve pool — credit watchdog. Every interval (default 10m, first
      # tick immediate) fresh credits are pulled for every account; accounts
      # with remaining credits below preserve_threshold (default 50) are
      # parked in the preserve set (top-level `preserve: true` on the auth
      # file): excluded from routing, sessions evicted, and auto-released
      # when credits recover. Set preserve_watchdog_enabled: false to keep
      # existing marks but stop adding new ones.
      preserve_threshold: 50
      preserve_watchdog_interval: "10m"
      preserve_watchdog_enabled: true

      # CPAMP usage forwarding. Both must be set for any record to be sent.
      # Falls back to USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY env vars or docker secret files when unset here.
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # Per-request account-failover budget for 40x errors (default 3,
      # range 0-5). When a request hits an account-level 40x (401/403/
      # 404/405), the plugin retries the SAME request on a different
      # workbuddy account up to `retry_on_4xx` times before giving up.
      # Set to 0 to disable on-request account rotation (use as a kill
      # switch during global outage recovery). The failing account is
      # also recorded in the cooldown list so subsequent requests skip
      # it.
      retry_on_4xx: 3

      # Plugin-layer management auth. When set, all mutating endpoints under
      # /v0/management/plugins/workbuddy/* require this Bearer token.
      # When empty (default) the host's management middleware is the only
      # guard. Also readable from WB_MANAGEMENT_KEY env var.
      management_key: ""

      # Shared token-usage feed for the token-usage-tracker plugin
      # (default enabled). Failures only disable the feed; chat and CPAMP
      # forwarding are unaffected.
      usage_feed_enabled: true
      # Optional feed path (default <CLIProxyAPI root>/data/token-usage-feed.ndjson).
      # Must match token-usage-tracker's usage_feed_path when both are set.
      usage_feed_path: ""
      # Async flush interval (1s-1h, default 5s).
      usage_flush_interval: "5s"
      # Max records buffered before forcing a flush (1-1000000, default 100).
      usage_flush_max_records: 100
```

Model aliases and exclusions are handled natively by CPA's
`oauth-model-alias` and `oauth-excluded-models` config — no plugin-side
duplication needed.

## Preserve pool (保号池)

The preserve pool is the only account separation: accounts are either
**normal** (routed normally) or **preserved** (kept idle because their
remaining credits just dropped below a threshold, so routing stops burning
their last credits while the user recharges). The flag is toggled
automatically by a watchdog — there is no manual pool selection (the v0.10.x
priority/default/fallback pools were removed in v0.12.0).

How it works:

1. **Watchdog loop** — every `preserve_watchdog_interval` (default `10m`,
   first tick fires immediately at plugin start) the watchdog pulls fresh
   credits for every workbuddy account via the shared singleflight channel.
2. **Entering preserve** — when `total_remain < preserve_threshold` (default
   `50`, strictly less), the account gets `preserve: true` on the physical
   auth file (host watcher picks it up; survives restart) and all session
   bindings pinned to it are evicted, so in-flight conversations re-pick a
   healthy account on their next request.
3. **Routing** — preserved accounts are excluded from `scheduler.pick`
   (filtered together with disabled accounts, before failover cooldown).
   Only when EVERY workbuddy account is preserved does routing keep the full
   list and fall back to the current pin, so a fleet-wide credit reset never
   locks routing.
4. **Recovery** — when credits recover to `>= threshold`, the watchdog clears
   the flag (`preserve` key removed) and the account rejoins normal routing
   automatically. Manual toggling is intentionally not exposed: preserve
   is a health gate, not a user preference.

Config knobs: `preserve_threshold` (int), `preserve_watchdog_interval`
(duration string), `preserve_watchdog_enabled` (bool) — see the config sample
above. The panel shows a **保号** badge on parked accounts and a `保号 N`
counter in the summary line.

## Lifecycle

| State | CN account | Global account |
|---|---|---|
| Credits > 0 | active | active |
| Credits = 0 | `disabled: true` (auth file kept) | auth file **deleted** |
| Check-in restores credits | re-enabled | n/a (already deleted) |
| Trial available | n/a | claimable once per account |
| Unknown credits | untouched (never mis-kill) | untouched |

Hard credit errors from the executor (status 402, "insufficient credits",
"积分不足", etc.) trigger an immediate reconcile of the failing account.

## Development

Requires Go 1.26+ (matches CPA).

```bash
# Build the plugin
go build -buildmode=c-shared -o workbuddy.so .

# Run tests
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

The plugin uses CPA's host HTTP bridge (`host.http.do` / `do_stream`) for
all upstream calls so request-log captures outbound traffic and host
transport policy applies. A fallback direct HTTP client is used only when
the bridge is unavailable (unit tests, hosts older than v7.2.x).

See [docs/development.md](docs/development.md) for the full workflow and
[docs/architecture.md](docs/architecture.md) for the module map.

## License

MIT — see [LICENSE](LICENSE).
