# WorkBuddy 插件（CLIProxyAPI）

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 **腾讯 CodeBuddy**
（国内版 `copilot.tencent.com` + 国际版 `workbuddy.ai`）原生 OAuth 提供商插件：
动态模型发现、流式执行器、积分感知调度、每日自动签到、内置管理面板。

[English → README.md](README.md)

## 功能

- **OAuth 登录** — 通过宿主 auth store 管理多账号 `workbuddy-<uid>.json`，
  CN 和 Global 共用一个插件、一份配置。
- **动态模型** — 上游 models API 实时拉取 + 5 分钟缓存 + 静态 fallback。
  宿主侧 `oauth-model-alias` / `oauth-excluded-models` 配置直接生效。
- **执行器** — OpenAI 兼容 chat completions，流式（真 SSE，走 `host.stream.emit`）
  和非流式（SSE 折叠成单个 completion）都支持。内置 `tool_choice` 归一、
  Claude Code 模板清洗、按区域注入 system message。
- **积分生命周期** — CN 账号耗尽自动 `disabled`，签到回血后自动恢复；
  Global 账号耗尽**删除** auth 文件（一次性 trial 额度）。Executor 遇到硬
  积分错误立即触发 reconcile。
- **每日签到** — CN 账号每天 09:00 和 21:00 自动签到（可配置）。面板可手动
  全部签到。Per-account 互斥锁防止多浏览器标签并发重复签到。
- **Trial 领取** — Global 账号可在面板领取一次性 250 积分专家加油包。
- **积分面板** — 内嵌面板 `/v0/resource/plugins/workbuddy/panel`，含积分
  进度条、套餐徽章、耗尽/禁用标记、CN/Global 筛选、凭证导入。
- **调度器**（可选） — `scheduler_mode` 默认 **`session`**：按会话轮询多账户
  （同一会话 1 小时内固定同一账号，不同会话分散到不同账号）；`credits`
  选中面板账号；`off` 完全交给 CPA 内置调度。
- **Usage 上报** — 实现 `UsagePlugin` 能力，每条请求的 usage record 转发到
  可配置的 CPAMP 端点。未配置 URL+key 时不上报。

## 快速开始

### 1. 安装插件

把编译好的 `workbuddy.so` 放到 CPA 插件目录：

```bash
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

多架构部署可用平台子目录约定：

```
plugins/
  linux/amd64/workbuddy.so
  linux/arm64/workbuddy.so
  darwin/arm64/workbuddy.so
```

### 2. 启用配置

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. 登录

从 CPA 侧边栏打开 WorkBuddy 面板（或直接访问
`/v0/resource/plugins/workbuddy/panel`），点 **登录** 走 OAuth 流程。
每个账号登录一次，插件会把 `workbuddy-<uid>.json` 写入 auth store。

### 4. 调用

用任何映射到 workbuddy 模型的 alias 调 OpenAI 兼容端点：

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

## 配置项

全部字段可选，位于 `plugins.configs.workbuddy` 下。

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # CN 账号每日自动签到（默认 true），09:00 和 21:00 本地时间。
      checkin_auto: true

      # 积分生命周期：CN 耗尽禁用 / Global 耗尽删除 / CN 回血恢复（默认 true）。
      lifecycle_auto: true

      # 调度行为（默认 "session"）：
      #   session → 按会话轮询：同一会话 1 小时内固定同一账号，不同会话分散
      #             到不同账号；无会话标识的请求回落面板选中账号
      #   credits → 插件选中面板选中的账号（耗尽/禁用时回退）
      #   off     → 完全交给 CPA 内置调度
      scheduler_mode: "session"

      # CPAMP usage 上报。URL+key 都设置才会上报。
      # 未配置时 fallback 到 USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY 环境变量或 docker secret 文件。
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # 插件层 management 鉴权。设置后所有 /v0/management/plugins/workbuddy/*
      # 写端点要求该 Bearer token。空（默认）则只靠宿主 management middleware。
      # 也可从 WB_MANAGEMENT_KEY 环境变量读。
      management_key: ""
```

模型 alias 和排除走 CPA 原生 `oauth-model-alias` 和 `oauth-excluded-models`
配置，无需插件侧重复。

## 生命周期

| 状态 | CN 账号 | Global 账号 |
|---|---|---|
| 积分 > 0 | active | active |
| 积分 = 0 | `disabled: true`（auth 文件保留） | auth 文件**删除** |
| 签到回血 | 自动恢复 | n/a（已删） |
| Trial 可领 | n/a | 每账号一次 |
| 积分未知 | 不动（永不误杀） | 不动 |

Executor 遇到硬积分错误（402、"insufficient credits"、"积分不足" 等）
会立即触发该账号的 reconcile。

## 开发

需要 Go 1.26+（与 CPA 一致）。

```bash
# 编译插件
go build -buildmode=c-shared -o workbuddy.so .

# 跑测试
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

插件所有上游调用走 CPA 宿主 HTTP 桥（`host.http.do` / `do_stream`），
request-log 可捕获出站流量并应用宿主 transport 策略。仅在桥不可用
（单元测试、v7.2.x 之前的宿主）时 fallback 到直连 HTTP client。

完整开发流程见 [docs/development.md](docs/development.md)，模块结构见
[docs/architecture.md](docs/architecture.md)。

## License

MIT — 见 [LICENSE](LICENSE)。
