# 方向修正：usage stats 从 workbuddy 拆出为第三个插件（v0.9.0 方案定型）

> 状态：已定案（2026-08-18）。替代 `merge-token-tracker-into-workbuddy.md` 的合并方向。

## 1. 用户反馈与需求

1. **404**：workbuddy 0.8.8 合并版 dashboard 页面能打开，但 `/stats/initial`、
   `/stats/trends`、`/stats/groups`、`/requests`、`/costs`、`/prices`、
   `/preferences`、`/exchange-rate` 全部 404。
2. **方向纠正**：tracker 不应并入 workbuddy，而是作为本项目**第三个插件**
   （与 workbuddy、qoderwork 并列），并且要能记录 **workbuddy 账户的真实
   token 消耗（实盘）**。

## 2. 根因（已逐文件确认）

- CPA 宿主资源路由是**精确匹配**：`Host.RegisterManagementRoutes`
  （`internal/pluginhost/management.go`）把插件声明路径规范化后存进
  `resourceRoutes` 表（key = `"GET " + 完整路径`），`ServeResourceHTTP`
  只查表转发；**未声明的前缀一律 404**（不会按 `/v0/resource/plugins/<id>/*`
  前缀透传）。
- workbuddy 0.8.8 只声明了 `/usage` 页面资源，dashboard 前端（由 tracker
  移植而来，URL 自适应 `location.pathname`）调用的 9 个读接口都没注册 →
  宿主层直接 404，`handleUsageStatsResource` 里的处理逻辑根本到不了。
- 管理路由同样精确匹配（`managementRouteKey(method, path)`），所以写接口
  （`/prices`、`/prices/sync`、`/reset`、`/backup`、`/restore`）也必须显式
  声明。

## 3. 为什么 0.8.8 的"合并进 workbuddy"走不通（数据通道的硬约束）

| 方案 | 结论 |
|---|---|
| 宿主 `UsagePlugin` 广播 → tracker | 插件 executor 适配器不 publish usage，广播恒为空（0.8.8 调研已确认） |
| workbuddy 写 bbolt、tracker 读同一 bbolt | **不可行**：bbolt 打开即持排它 flock（`bolt.Open` 写模式 `LOCK_EX`），两个长驻进程无法同时持有；读模式 `LOCK_SH` 也与写锁互斥 |
| tracker 轮询文件 feed（NDJSON 追加写） | **可行**：workbuddy 是唯一生产者（O_APPEND 逐行写），tracker 是唯一消费者（维护偏移量导入自己独占的 bbolt 库），无锁冲突、可回放、可轮转 |

**定案：共享 NDJSON feed 文件**。

## 4. 架构（0.8.9 + 0.1.0）

```
workbuddy 0.8.9（生产者）              token-usage-tracker v0.1.0（消费者）
┌──────────────────────────────┐      ┌──────────────────────────────────┐
│ publishUsage 汇聚点（所有请求） │      │ 轮询（默认 5s）+ 查询前即时同步      │
│   ├─ forwardUsageToCPAMP      │      │ 读取 <root>/data/                │
│   └─ recordUsageFeed          │      │   token-usage-feed.ndjson        │
│      O_APPEND 追加一行 NDJSON ─┼─────▶│ 导入 usagestats.Store（bbolt）    │
└──────────────────────────────┘      │ dashboard：全部路由显式注册        │
                                      └──────────────────────────────────┘
```

### workbuddy 0.8.9 改动

- 删除：`usage_stats/` 子包、`usage_stats_bridge.go`、`/usage` 资源、
  `handleUsageStatsResource/Management`、`usageStatsManagementAuthRequired`、
  `usage_stats_*` 配置项。
- 新增：`usage_feed.go` —— `recordUsageFeed`（配置 `usage_feed_enabled` /
  `usage_feed_path`，默认 `<root>/data/token-usage-feed.ndjson`，>128MB 截断
  轮转，逐行 O_APPEND 写）。
- 数据源挂点：`publishUsage` 的异步 goroutine（与 CPAMP 转发同处）。
  ⚠️ 只在 `publishUsage` 写（新宿主还会再调 `handleUsage`，两处都写会重复；
  走 executor 的请求必然经过 `publishUsage`，单点写入即 exactly-once）。

### token-usage-tracker v0.1.0（第三个插件）

- 能力：仅 `ManagementAPI`（无 model/auth/executor/scheduler/usage 能力）。
- 内核：`usage_stats/` 子包（复用 0.8.8 移植版，包名 `usagestats`，bbolt+
  yaml 依赖），新增 `usage_stats/feed_import.go`：
  `Store.RecordFeedNDJSON(line)`（feed 行格式契约归消费者所有）。
- 路由注册（修复 404 类问题的关键——**全部显式声明**）：
  - Resources（GET）：`/usage`（页面）、`/stats`、`/stats/initial`、
    `/stats/trends`、`/stats/groups`、`/requests`、`/costs`、`/prices`、
    `/preferences`、`/exchange-rate`
  - Routes（写）：`PUT /prices`、`POST /prices/sync`、`POST /reset`、
    `GET /backup`、`POST /restore`
  - 写接口统一走 `management_key` Bearer 门（dashboard 的 askManagementKey
    提示与此对应）。
- 导入器：`feed_ingest.go` —— 偏移量（内存 + `<feed>.offset` 侧车文件），
  轮转检测（size < offset → 归 0），尾部半行留给下轮，坏行跳过但推进偏移，
  行大小上限 1MB。轮询 + 每次查询前即时同步（dashboard 近实时）。
- 配置：`management_key`、`usage_feed_enabled`、`usage_feed_path`、
  `usage_db_path`、`usage_retention_days`、`usage_flush_interval`、
  `usage_flush_max_records`、`usage_poll_interval`。

## 5. 发布

- workbuddy 0.8.9：VERSION/main.go 版本 + CHANGELOG/README/README_CN/
  docs/architecture 同步，CI 发布，registry 更新。
- token-usage-tracker v0.1.0：CI 工作流加 test/build/cross 矩阵 +
  dispatch 选项 + tag 前缀 + release 判定；registry.json 加第 3 条目
  （7 平台 direct 资产）。
- 资产托管：`release-assets/token-usage-tracker-0.1.0/`（与既有插件同款
  流程：下载 release zip → sha256 交叉校验 → 入仓 → registry 更新）。

## 6. 风险与注意

- feed 与 tracker 的默认路径都必须解析到同一个 `<CLIProxyAPI root>/data`，
  根目录发现逻辑两侧一致（找 `plugins/` 的祖先目录）；显式配置时两侧
  `usage_feed_path` 必须一致。
- `usage_stats/handover.go` 的锁交接针对单进程多实例；新架构下 tracker 是
  bbolt 唯一持有者，workbuddy 永不触碰 DB 文件。
- 若用户曾安装 0.8.8：升级 0.8.9 后 `/usage` 菜单消失（积分面板不受影响），
  需另行安装 token-usage-tracker 才能看到统计；旧 `usage-stats.db` 不再
  使用（保留不删，可手动清理）。
