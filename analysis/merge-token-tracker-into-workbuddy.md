# 合并 cap-token-usage-tracker 进 workbuddy — 方案

日期:2026-08-19
状态:已定稿,进入实施

## 1. 背景与根因

CPA 宿主只在**原生 executor**(codex/claude/gemini/kimi 等)执行后由执行器内部
`reporter.Publish` 产生 usage 记录并广播给所有 UsagePlugin;插件 executor
(`internal/pluginhost/adapters_executors.go` 的 `executorAdapter.Execute/ExecuteStream`)
执行后**不发布任何 usage**。因此 workbuddy(插件 executor)处理的请求,宿主 usage
管理器收不到记录,`cap-token-usage-tracker`(纯 UsagePlugin 消费者)永远收不到数据。

workbuddy 自己的统计不依赖宿主:执行链路内自解析 usage(`sseUsageCollector` /
`usageDetailFromCompletion`),直连 CPAMP `usage/import` 上报(插件独立进程,
宿主进程内的 `usage.DefaultManager` 对插件不可用)。

**结论**:两个插件天然无法通过宿主联动 → 按用户决策,把 token 统计能力合并进 workbuddy,
数据源改为 workbuddy 执行链路内部采集。

## 2. 合并范围

### 2.1 保留(核心统计能力,迁移到子包 `workbuddy/usage_stats/`)

| 源文件(token 插件) | 行数 | 说明 |
|---|---|---|
| usage.go | 375 | normalizedUsage/Dimensions/Counters 结构与解码 |
| aggregate.go | 803 | 分钟级聚合、趋势、分组、响应构建 |
| persistence.go | 2685 | bbolt Store(actor 模型,异步串行) |
| cost.go | 481 | 费用估算(输入/输出/推理/缓存) |
| pricing.go | 379 | 模型价格表 |
| modelsdev.go | 439 | models.dev 价格同步 |
| exchange_rate.go | 202 | USD/CNY 汇率 |
| config.go | 122 | 存储配置(路径/保留期/flush) |
| preferences.go | 134 | dashboard 偏好 |
| plugin_path.go | 2485* | 插件目录发现 → 默认数据路径 |
| dashboard.go | 662 | 普通模式 dashboard 前端模板(268KB 含全量) |
| management.go | 1004 | 查询处理(stats/trends/groups/requests/costs/prices/preferences) |
| auth_identity.go | 198 | 认证账号显示名映射 |

*plugin_path.go 实为 2.5KB(2485 字节),含多平台实现。

### 2.2 裁剪(OAuth 场景无关)

| 源文件 | 理由 |
|---|---|
| full_mode.go | 管理密钥 + 短时 session,workbuddy 已有 management_key 鉴权体系 |
| sensitive.go | API Key 明文/指纹脱敏,workbuddy 无 API key(用 auth_index/UID) |
| handover.go | 数据库版本迁移(旧版→新版),新库不需要 |
| api_key_* 相关逻辑 | OAuth 场景无 API key |
| 响应压缩 compression.go | 保留(轻量,提升 dashboard 性能) |

### 2.3 数据源改造

workbuddy 现有 `publishUsage(...)`(usage.go:68)在 4 个采集点被调用(非流式
handleExecExecute、流式同步 collectUpstreamStream、流式异步 pumpUpstreamStream),
全部已携带 `usage.Detail`。在 `publishUsage` 内追加一行本地统计写入:

```
recordLocalUsage(alias, model, authUID, started, detail, failed, statusCode, errBody)
```

- 构造 normalizedUsage:Provider=workbuddy、ExecutorType=workbuddy、AuthType=oauth、
  Source=codebuddy.cn、AuthIndex=UID、Model、Alias、Failed、Detail
- `Store.Record` 是 actor 异步写入,不阻塞执行器热路径
- 与 CPAMP 上报并行,互不影响

### 2.4 配置(workbuddy config_yaml 新增)

| 字段 | 默认 | 说明 |
|---|---|---|
| usage_stats_enabled | true | 本地 token 统计开关 |
| usage_stats_path | 自动(插件目录旁 usage-stats.db) | bbolt 数据库路径 |
| usage_retention_days | 365 | 保留天数 |
| usage_flush_interval | 5s | 批量 flush 间隔 |
| usage_flush_max_records | 100 | 批量 flush 上限 |

### 2.5 Management 路由(挂 workbuddy 前缀)

- Resource:`/usage`(Menu "Token 用量")→ dashboard 前端
- Management(GET 只读):`/usage/stats`、`/usage/initial`、`/usage/trends`、
  `/usage/groups`、`/usage/requests`、`/usage/costs`、`/usage/exchange-rate`、
  `/usage/prices`、`/usage/preferences`
- Management(写):`/usage/reset`(POST)、`/usage/prices`(PUT)、`/usage/prices/sync`(POST)、
  `/usage/backup`(GET)、`/usage/restore`(POST)

### 2.6 依赖变更

- workbuddy go.mod:`github.com/router-for-me/CLIProxyAPI/v7` v7.2.30 → v7.2.129
- 新增 `go.etcd.io/bbolt v1.4.3`

## 3. 实施步骤

1. **Phase 1**:创建 `workbuddy/usage_stats/` 子包,搬运保留文件,包名 main → usagestats,
   裁剪 full_mode/sensitive/api-key,清理编译错误
2. **Phase 2**:子包导出 API:`Open(config) (*Store, error)`、`(*Store).Record`、
   查询方法、`DashboardHTML()`
3. **Phase 3**:workbuddy main 集成——配置解析、store 生命周期(init 打开/shutdown 关闭)、
   `recordLocalUsage`、management 路由挂载
4. **Phase 4**:测试——子包单测(从 token 插件测试移植核心用例)+ 隔离 shim 环境全量回归
5. **Phase 5**:文档(CHANGELOG/README)+ 版本提升 + 发布

## 4. 风险

- dashboard.go 268KB 搬运后二进制体积 +~1.5MB(可接受)
- SDK v7.2.30 → v7.2.129 升级需回归 workbuddy 现有功能(仅 go vet/test 验证,
  CI 有完整 CGO 环境)
- bbolt 首次打开需创建目录,失败时降级禁用统计(不阻塞 chat)
