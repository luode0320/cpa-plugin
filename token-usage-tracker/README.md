# token-usage-tracker

本项目（`cpa-workbuddy-plugin`）的**第三个插件**（与 `workbuddy`、`qoderwork` 并列）：
记录并可视化 **workbuddy 账户的真实 token 消耗**（实盘数据）。

## 数据链路

```
workbuddy 插件                        token-usage-tracker 插件
┌─────────────────────────┐          ┌──────────────────────────┐
│ 每次请求完成后          │  NDJSON  │ 轮询读取（默认 5s）        │
│ publishUsage 汇聚点 ─────┼──append──▶ 共享 feed 文件            │
│ 追加一行到共享 feed      │  O_APPEND│ 导入自己的 bbolt 库        │
└─────────────────────────┘          │ dashboard 展示（菜单      │
                                     │  "Token 用量"）            │
                                     └──────────────────────────┘
```

- **共享 feed**：`<CLIProxyAPI root>/data/token-usage-feed.ndjson`（默认），
  与 workbuddy 的 `usage_feed_path` 保持一致即可互相发现。
- **为什么是文件 feed**：CPA 宿主的 `UsagePlugin` 广播对插件 executor 恒为
  空；bbolt 排它文件锁不允许两个长驻进程共享同一个数据库文件。追加写
  NDJSON 文件是唯一干净的跨插件数据通道（无锁、可回放、可轮转）。
- **写入方只有 workbuddy，读取方只有本插件**：本插件是唯一打开 bbolt 库
  的进程，不会出现文件锁冲突。
- 统计核心（`usage_stats/` 子包）移植自社区插件
  [AITNR/cap-token-usage-tracker](https://github.com/AITNR/cap-token-usage-tracker)，
  感谢原作者。

## 安装

从插件商店（registry 源
`https://raw.githubusercontent.com/luode0320/cpa-workbuddy-plugin/main/registry.json`）
搜索 **token-usage-tracker** 安装即可。与 workbuddy 同时安装后开箱即用：
dashboard 读接口与写接口全部在注册表中显式声明，不会出现 404。

## 配置（config_yaml，均可选）

```yaml
# 写接口（价格/重置/备份/恢复）的 Bearer token。留空则只依赖宿主中间件。
# 也可用环境变量 TOKEN_USAGE_TRACKER_MANAGEMENT_KEY。
management_key: ""

# 是否轮询共享 feed（默认 true）。
usage_feed_enabled: true
# 共享 feed 路径（默认 <CLIProxyAPI root>/data/token-usage-feed.ndjson）。
# 与 workbuddy 的 usage_feed_path 一致。
usage_feed_path: ""
# 数据库路径（默认 <CLIProxyAPI root>/data/token-usage-tracker.db）。
usage_db_path: ""
# 保留天数（1-3650，默认 365）。
usage_retention_days: 365
# 落盘间隔（1s-1h，默认 5s）。
usage_flush_interval: 5s
# 强制落盘的缓冲条数（1-1000000，默认 100）。
usage_flush_max_records: 100
# feed 轮询间隔（1s-1h，默认 5s）。
usage_poll_interval: 5s
```

## Dashboard

- 页面：`/v0/resource/plugins/token-usage-tracker/usage`（菜单 "Token 用量"）
- 读接口（resource 路由，均已注册）：
  `/stats`、`/stats/initial`、`/stats/trends`、`/stats/groups`、
  `/requests`、`/costs`、`/prices`、`/preferences`、`/exchange-rate`
- 写接口（management 路由，已注册，受 `management_key` 门保护）：
  `PUT /prices`、`POST /prices/sync`、`POST /reset`、`GET /backup`、
  `POST /restore`

## 本地开发验证

本机 Windows 无 C 工具链，用 cgo 桩方式验证：

```bash
python scripts/cgo-shim-build.py token-usage-tracker
```

CI（Linux/macOS）直接 `go test ./...`（真实 cgo 环境）。
