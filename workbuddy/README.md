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

      # Routing pools — per-account three-state panel button (default →
      # priority → fallback → default), or
      # POST /plugins/workbuddy/pool {auth_index, pool}. Every account is
      # "default" unless marked. Routing cascades strictly:
      #   priority bucket (≥1 usable account) → default bucket → fallback
      #   bucket. While a higher bucket has a usable account (not
      #   disabled/exhausted/cooling-down), ALL routed traffic stays inside
      #   it; lower buckets only see traffic when every higher account is
      #   unusable. Live toggle, persisted on the auth file (top-level
      #   `pool` field; legacy `priority: true` auto-migrates), no restart
      #   needed.

      # CPAMP usage forwarding. Both must be set for any record to be sent.
      # Falls back to USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY env vars or docker secret files when unset here.
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

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

## Routing pools (priority / default / fallback)

The panel's three-state pool button (default → priority → fallback → default)
splits routing candidates into three buckets on top of the existing
session/credits pickers. Every account is **default** unless marked:

- **Priority bucket** — accounts marked `pool: "priority"` (persisted on the
  physical auth file, top-level field). While at least one priority account
  is usable, `scheduler.pick` only ever returns priority accounts, even if
  the panel-selected "active" account is a default one.
- **Default bucket** — every unmarked account. Used when the priority bucket
  is empty or every priority account is disabled / exhausted / cooling-down.
- **Fallback bucket** — accounts marked `pool: "fallback"`. Last resort:
  used only when BOTH the priority and the default bucket have no usable
  account, so pool-level exhaustion never causes 4xx/5xx cascades.

Rules:

1. Usable = not disabled, not credit-exhausted, not in failover cooldown.
2. The cascade is strict: priority → default → fallback. Inside the winning
   bucket the existing rules still apply (skip exhausted members, session
   stickiness within the bucket). If NO bucket has a usable account, routing
   defers to the built-in scheduler.
3. Session bindings migrate to the priority bucket automatically when a
   priority account appears, and back down the cascade when it empties.
4. Toggle is live: no restart, no config change. Deleting an account also
   removes it from the pool. Legacy `priority: true` marks (v0.9.x) are
   auto-migrated to the priority pool on read.

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
