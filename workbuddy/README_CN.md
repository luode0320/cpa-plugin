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
- **Token 用量 feed** — 每条请求的 token 消耗（输入/输出/推理/缓存）以
  NDJSON 单行追加写入共享 feed
  `<CLIProxyAPI root>/data/token-usage-feed.ndjson`。独立配套插件
  `token-usage-tracker`（同一 registry 安装）轮询该 feed 导入自己的 bbolt
  库并展示 dashboard（菜单 "Token 用量"，
  `/v0/resource/plugins/token-usage-tracker/usage`）：趋势、按模型/账号统计、
  请求明细与成本估算。这是 v0.8.8 合并版统计（已撤回）的替代方案——宿主
  `UsagePlugin` 广播对插件 executor 恒为空，且两个长驻进程无法共享同一
  bbolt 文件锁，文件 feed 是唯一干净的跨插件数据通道。
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

      # 保号池 — 积分看护。每隔 preserve_watchdog_interval（默认 10m，首轮
      # 立即执行）拉取全部账号真实积分；剩余积分低于 preserve_threshold
      # （默认 50）的账号划入保号态（auth 文件顶级 preserve: true）：
      # 不参与路由、驱逐已绑定会话，积分回血后自动释放。
      # preserve_watchdog_enabled: false 可保留已保号账号但不再新增。
      preserve_threshold: 50
      preserve_watchdog_interval: "10m"
      preserve_watchdog_enabled: true

      # CPAMP usage 上报。URL+key 都设置才会上报。
      # 未配置时 fallback 到 USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY 环境变量或 docker secret 文件。
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # 插件层 management 鉴权。设置后所有 /v0/management/plugins/workbuddy/*
      # 写端点要求该 Bearer token。空（默认）则只靠宿主 management middleware。
      # 也可从 WB_MANAGEMENT_KEY 环境变量读。
      management_key: ""

      # token-usage-tracker 插件共享的 usage feed（默认开启）。feed 失败只
      # 禁用上报，不影响 chat 与 CPAMP 转发。
      usage_feed_enabled: true
      # 可选 feed 路径（默认 <CLIProxyAPI root>/data/token-usage-feed.ndjson）。
      # 两侧都显式设置时需与 token-usage-tracker 的 usage_feed_path 一致。
      usage_feed_path: ""
      # 异步落盘间隔（1s-1h，默认 5s）。
      usage_flush_interval: "5s"
      # 缓冲记录数超过该值强制落盘（1-1000000，默认 100）。
      usage_flush_max_records: 100
```

模型 alias 和排除走 CPA 原生 `oauth-model-alias` 和 `oauth-excluded-models`
配置，无需插件侧重复。

## 保号池（积分看护）

保号池是账号唯一的区分维度：账号要么**正常**（正常参与路由），要么**保号**
（因剩余积分跌破阈值被暂时屏蔽，让路由停止消耗其最后一点积分，等用户充值
回血）。标记由 watchdog 自动翻转——v0.12.0 起已移除手动三池选择
（v0.10.x 的优先/默认/兜底池），不存在用户手动归属可被覆盖的问题。

工作机制：

1. **定时看护** — 每隔 `preserve_watchdog_interval`（默认 `10m`，插件启动
   首轮立即执行）经共享 singleflight 通道拉取全部 workbuddy 账号真实积分。
2. **进入保号** — 当 `total_remain < preserve_threshold`（默认 `50`，严格
   小于）时，auth 文件写入 `preserve: true`（宿主 watcher 自动接管、重启
   不丢），并**立即驱逐**所有绑定到该账号的会话 binding——正在使用的对话
   下一次请求自动改走健康账号。
3. **不参与路由** — 保号账号在 `scheduler.pick` 中整体剔除（与 disabled
   同级过滤，先于 failover cooldown）。仅当**全部** workbuddy 账号都保号
   时保留全列表回落到当前 pin，避免全库保号把路由锁死。
4. **自动恢复** — 积分回到 `>= threshold` 后 watchdog 自动清除标记
   （删除 `preserve` 键），账号恢复正常路由。刻意不提供手动开关：
   保号是健康闸门，不是用户偏好。

配置项：`preserve_threshold`（int）、`preserve_watchdog_interval`
（时长字符串）、`preserve_watchdog_enabled`（bool），见上方配置示例。
面板对保号账号显示**保号**徽标，汇总栏显示 `保号 N` 计数。

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
