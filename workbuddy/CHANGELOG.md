# Changelog

## 0.13.1

### Feature — 请求明细展示会话 ID（session_key 替换 Tier 占位列）

token-usage-tracker 请求明细页的 `Tier` 列此前是空数据占位（workbuddy
从未写入上游 service tier）。现改为展示**会话 ID**（截取前 8 位），用于
回答"这条请求来自哪个会话、和上一条是不是同一个会话"——正常会话粘性
路由下，同一会话的所有请求应命中同一前缀，跨会话一眼可辨。

- `session_auth.go`：抽出 `extractSessionKeyFromSources(headers, metadata)`
  纯函数，`extractSessionKey(req)` 改为薄包装（复用同一份优先级逻辑：
  execution session metadata > 客户端 session 头 > derived session id）。
- `usage.go` / `usage_feed.go`：`publishUsage` / `recordUsageFeed` 末位
  追加 `sessionKey` 形参，NDJSON 记录新增 `session_key` 字段。
- `main.go` / `stream.go`：`handleExecExecute` / `handleExecStream` 入口
  各抓取一次会话键并透传全部调用点。
- tracker 侧：`Dimensions.ServiceTier` → `SessionKey`（json `session_key`），
  请求明细列 `Tier` → `会话`（前 8 位），定价查询不再误用该字段。
- 兼容：旧 NDJSON 行无 `session_key` → 空串 → 列表显示 `—`，零迁移。

### UI — 账号卡片对齐与筛选增强（panel.html）

- 卡片宽度与"用量汇总 · 全部账号"对齐：`.grid` 由 `repeat(3,1fr)`
  改为 `repeat(3, minmax(0, 1fr))`，避免卡片内容（长 uid/进度元信息）
  撑破列宽导致卡片超出容器。
- 移除卡片上"选用"按钮与"使用中" badge：路由已按会话粘性自动切换，
  手动选用不再需要（后端 `/select` API 与 `selectAuth` 保留）。
- 筛选栏新增"已禁用"、"保号"两个 tab：卡片增加 `data-disabled` /
  `data-preserve` 属性，`applyCardVisibility` / `accountsForFilter` /
  `updateFilterCounts` 同步支持新筛选与计数。

### 涉及文件

- `workbuddy/session_auth.go` / `usage.go` / `usage_feed.go` / `main.go` /
  `stream.go` / `usage_feed_test.go`
- `workbuddy/panel.html`
- `token-usage-tracker/usage_stats/`（feed_import.go / usage.go /
  aggregate.go / api.go / cost.go / dashboard.go / preferences.go /
  usage_record_test.go）+ `locales/{zh-CN,zh-TW,en,ru}.json`

## 0.13.0

### Feature — 40x 同请求切号重试（retry_on_4xx 预算）

账号级 40x（401/403/404/405）不再直接中断会话：同一请求在
`retry_on_4xx` 预算内（默认 3，范围 0-5）自动切换到下一个可用账号重建
请求重试，直到成功或预算耗尽。解决"坏号有积分但任何请求都 40x，会
话被立刻打断"的场景——坏号被快速跳过，会话不中断。

- 新建 `failover_retry.go`：同请求切号循环（`pickNextAuth` +
  `rebuildRequestWithSA` 重建请求重试）。
- 新建 `retry_config.go`：`retry_on_4xx` 配置加载与缓存（0 为 kill
  switch，全局中断恢复期可一键关闭）。
- `stream.go`：请求循环接入 40x 切号；预算耗尽或非账号级 4xx 才直返
  `streamEmitError`。400 业务错误仍直通不重试。
- `accountFailover.go`：40x 同时计入账号级故障，进入跨请求 cooldown
  阶梯退避（1/3/10 分钟），后续请求跳过坏号。
- `usage_config.go`：`retry_on_4xx` 配置项（缺省键保持当前值，kill
  switch 安全）。
- `accountFailover_test.go` / `failover_retry_test.go` /
  `retry_config_test.go`：新增表格驱动测试覆盖白名单、预算边界、配置
  缺省与 0 值开关。

### 涉及文件

- `workbuddy/failover_retry.go`（新增）
- `workbuddy/retry_config.go`（新增）
- `workbuddy/failover_retry_test.go`（新增）
- `workbuddy/retry_config_test.go`（新增）
- `workbuddy/stream.go` / `accountFailover.go` / `usage_config.go` /
  `accountFailover_test.go`
- `qoderwork/` 同构整批（逐函数适配，非整体覆盖）
- `qoderwork-patches/` 补丁备份

## 0.12.1

### Fix — 保号 watchdog 首 tick 与宿主初始化竞态

`init()` 启动的 watchdog goroutine 早于宿主调用 `cliproxy_plugin_init`
设置 `hostAPI`，导致首 tick 的 `hostAuthList()` 走到 `host API unavailable`
分支被吞、下一次要等满 `preserve_watchdog_interval`（默认 10m）。期间任意
跌破 `preserve_threshold`（默认 50）的账号都看不到保号 badge，会被路由
继续吃光积分。10 分钟后才被"补上"，体验割裂。

三道防线确保"插件生效即识别保号"：

1. **`watchdog.go` 首 tick 等宿主就绪** — 新增
   `hostReadyForWatchdog()` 探针（`hostBridgeAvailable()` +
   `hostAuthList()` 双信号），`preserveWatchdogLoop` 启动时先调用
   `waitHostReadyForWatchdog(15s, ...)` 轮询 250ms；首 tick 之前 drain
   一次 `preserveTickCh` 防止与 wait 期间排队的 trigger 立即双跑。
2. **`configure()` register/reconfigure 触发一次 tick** — 在
   `setPreserveConfig` 之后调用 `requestPreserveTick()`，保证新阈值/间隔
   立即生效，也保证首次注册时无须再等 10 分钟。`preserveTickCh` 是
   buffered cap 1，reconfigure 风暴自动合并为单次。
3. **面板强制刷新同步 reconcile** — `buildDashboardEx(force=true)` 用
   本次响应已拉到的 `credits`（无需再打上游）调
   `preserveReconcileFromAccounts(out)`，紧接重镜像一次
   `refreshPreserveSetFromDisk()`，让本次响应的 badge 与磁盘完全一致。
   用户主动点"刷新"看到的 badge 永远正确。

### 涉及文件

- `workbuddy/watchdog.go` — `hostReadyForWatchdog` /
  `waitHostReadyForWatchdog` / `preserveTickCh` / `requestPreserveTick` /
  `preserveFlipDecision` / `preserveFlipsNeeded` / `preserveApplyFlips` /
  `preserveReconcileFromAccounts`；`preserveWatchdogLoop` 重写为
  `timer + chan select`。
- `workbuddy/usage_config.go` — `configure()` 末尾
  `requestPreserveTick()`。
- `workbuddy/panel.go` — `buildDashboardEx` 在 `force=true` 时
  `preserveReconcileFromAccounts(out)` + 重镜像。
- `workbuddy/watchdog_test.go` — 新增 3 个测试：
  `TestWaitHostReadyForWatchdog`（4 case 覆盖立即 true / 第二次 true /
  maxWait=0 假 / 超时 false）、`TestRequestPreserveTickCoalesces`（3 发
  合并为 1）、`TestPreserveFlipsNeeded`（5 case 覆盖进入/退出/不动/无 credits）。

## 0.12.0


### Breaking Change — 移除三池路由（priority / default / fallback），只留保号池

v0.10.0 引入的三池路由（面板三态按钮 + `POST /pool` + auth 文件 `pool`
字段 + 三桶级联）在 v0.12.0 中**整体移除**。实测反馈：手动归池需要
逐账号点击维护，远不如保号池"自动扫描归池"省心。路由收敛为两种状态：

- **正常**：未保号账号按既有 session/credits 逻辑正常参与路由（等同旧
  default 池行为）。
- **保号**：watchdog 每 `preserve_watchdog_interval`（默认 10m）刷新全部
  账号积分，剩余 < `preserve_threshold`（默认 50）自动保号、不参与路由；
  恢复 ≥ 阈值自动解除——**0.11.0 引入的保号机制完整保留，无任何行为
  变化**。

移除明细：

- `pool.go` / `pool_test.go` 整套删除（池内存镜像、三桶级联、12 个测试）。
- 面板三态按钮（默认→优先→兜底循环）与优先/兜底 badge 删除，只保留
  **保号** badge 与汇总计数。
- `POST /plugins/workbuddy-provider/pool` 端点删除；`management.go` 路由表、
  mutating 白名单同步清理。
- `scheduler.pick` 不再分桶：候选链路收敛为「收集 → 剔除 disabled →
  剔除保号（全保号保留全量防锁死）→ 剔除冷却（全冷却保留全量保 pin）→
  session/credits 选择」。
- 旧错误函数 `errAuthIndexRequired` / `errAuthMissing` 内联进 `preserve.go`
  （原本定义在 pool.go 但被 preserve.go 复用）；测试辅助 `storeCredits`
  迁入 `watchdog_test.go`。

存量数据说明：auth 文件上遗留的 `pool` / `priority` 字段**不再解析**
（忽略式读取，零风险），也不做批量写盘清理——字段本身无害，保留原样。

### 涉及文件

- 删除：`pool.go`、`pool_test.go`
- `preserve.go`：内联错误函数；`watchdog_test.go`：迁移 `storeCredits`、
  清理 `resetAuthPool` 引用、`pool` 字段反例改为"不影响保号解析"语义
- `scheduler.go`：删三桶级联与 `anyCandidateUsable`
- `credits_handler.go`：删 `/pool` 端点与单卡 `pool` 字段
- `management.go`：删 `/pool` 路由注册与 mutating 白名单条目
- `lifecycle.go`：删两处 `clearPoolFor`
- `authfile.go`：删 `parsePoolFromAuthJSON`（含 legacy priority 迁移）
- `panel.go` / `panel.html`：删 Pool 字段、pool_sizes、三态按钮与 badge
- `README.md` / `README_CN.md`：删三池章节与配置注释，更新保号章节措辞

## 0.11.0

### Feature — 凭证导出 + 账号搜索 + 按积分排序

面板工具栏新增三项能力，配合既有的一键导入形成凭证备份/恢复闭环：

1. **导出凭证**（`导入凭证` 右侧新按钮）：新增 `GET /plugins/workbuddy-provider/export`
   端点，遍历宿主全部 workbuddy 账号，返回
   `{version, exported_at, count, accounts:[{name, auth_index, uid, nickname, region, credential}]}`
   —— 其中 `credential` 是每个账号的**原始物理文件 JSON**（nested 形式），
   面板一键下载为 `workbuddy-credentials-YYYY-MM-DD.json`。单账号加载/解析
   失败不影响整批（内联 `load_error` / `parse_error` 标记）。该端点纳入
   `mutatingManagementPath`，配置了 management key 时同样要求 Bearer 认证
   并受速率限制（返回敏感凭证，不应与 /accounts 同级透传）。
2. **搜索**（`全部领取` 右侧搜索框）：按 **nickname 模糊匹配**（大小写不敏感
   子串），同时匹配 label / 文件名 / UID（昵称为空时按 UID 找号更实用）。
   输入即过滤，与区域过滤（全部/CN/Global/耗尽）叠加生效，无需重新加载。
3. **排序**（搜索框右侧 `积分 ↕` 按钮）：按可用积分 `total_remain` 三态循环
   切换：关闭 → 升序（剩余少→多）→ 降序（剩余多→少）→ 关闭。未知积分的
   账号视为 -1（升序排最前、降序排最后）。排序开启时，卡片积分懒加载完成
   会触发整格重排，保证顺序实时正确。
4. **批量导入兼容**：导入弹窗（粘贴或选文件）现在自动识别
   「导出凭证」文件（`accounts[].credential` 包装）、纯 JSON 数组、以及单个
   nested/flat 凭证，统一展开为逐条凭证走既有 `/import` 管道——导出的文件
   可直接拖回导入框完成恢复（含 7s 限流重试）。

### 行为说明

- 搜索/排序为纯前端状态（`currentSearch` / `currentSort`），不新增后端
  查询参数；排序仅影响展示顺序，不改动账号卡原始顺序（关闭后还原）。
- `filterRegion` 与 `applyCardVisibility` 合并：区域过滤 + 搜索统一走
  卡片可见性切换，避免两套 display 逻辑互相覆盖。

### Feature — 保号池（积分阈值看护，自动暂停低积分账号路由）

系统此前只在请求发生时读取账号积分缓存（全事件驱动，无定时刷新）：
账号积分在两次请求之间悄悄跌破阈值时，路由依旧会把它当作可用账号继续
分发请求，直到下一次真实请求才触发耗尽处理——此时剩余积分往往已被
耗尽。本次新增**保号池**机制，把"健康看护"从请求路径中剥离出来：

1. **定时刷新**：新增后台 watchdog（默认每 10 分钟，可配置），遍历全部
   workbuddy 账号，经既有 singleflight 通道拉取真实积分
   （`/v2/billing/meter/get-user-resource`），首轮立即执行（插件启动即
   同步一次，无需等待一个完整周期）。
2. **阈值保号**：积分剩余 < `preserve_threshold`（默认 50）的账号被标记
   为**保号状态**（写物理 auth 文件顶级 `preserve: true`，宿主 watcher
   自动接管、重启不丢），并**立即驱逐**所有绑定到该账号的会话
   （`evictSessionBindingsForAuth`）——正在使用该账号的对话，下一次请求
   自动路由到其他健康账号。
3. **不参与路由**：保号账号在 `scheduler.pick` 中被整体剔除（与 disabled
   同级过滤，先于冷却过滤），仅在**全部账号都保号**时保留全列表回落到
   当前 pin，避免全库保号把路由锁死。
4. **自动恢复**：积分恢复 ≥ 阈值后，watchdog 自动清除保号标记
   （删除 `preserve` 字段），账号回到正常池继续参与路由——保号是运行时
   健康闸门，与账号的池归属完全解耦（v0.12.0 起池归属仅剩"正常"一态）。

### 保号池配置

| 配置键 | 默认值 | 说明 |
| --- | --- | --- |
| `preserve_threshold` | `50` | 剩余积分低于该值即保号（严格小于） |
| `preserve_watchdog_interval` | `10m` | watchdog 刷新间隔（首轮立即执行） |
| `preserve_watchdog_enabled` | `true` | 总开关；关闭时不再新增保号成员 |

面板账号卡片新增**保号**徽标（积分不足被看护的账号），汇总栏同步显示
`保号 N` 计数。

### 涉及文件（保号池）

- `preserve.go`：保号集合（内存镜像）+ 物理文件 `preserve` 字段读写 +
  配置 getter/setter
- `watchdog.go`：定时看护循环 + 阈值翻转决策（`preserveShouldFlip`）
- `scheduler.go`：候选收集后剔除保号账号（全部保号时保留全列表回落）
- `session_auth.go`：`evictSessionBindingsForAuth` 会话驱逐
- `usage_config.go`：三个保号配置键解析
- `panel.go` / `panel.html` / `credits_handler.go`：保号徽标 + 汇总计数 +
  单卡 `preserve` 字段
- `watchdog_test.go`：决策表/配置/路由过滤/会话驱逐测试

### 涉及文件

- `credits_handler.go`：新增 `handleExportAuth` / `errString`
- `management.go`：注册 `/export` 路由 + 加入 mutating 名单
- `panel.html`：工具栏按钮/搜索框/排序按钮、`exportAuth`、`expandCredentials`、
  `queuedImportItems`、`sortedAccounts` / `renderGrid` / `applyCardVisibility`

## 0.10.1

### Fix — 三池按钮文案错 + 点击无反应

`panel.html` 的三池循环按钮实现有两处缺陷：

1. **文案错**：按钮显示 `设优先 / 设兜底 / 设默认`（动作描述），
   用户期望显示当前状态名 `默认 / 优先 / 兜底`。
2. **点击无反应**：`nextPool` / `poolNames` 映射被错误地声明在
   `card()` 函数内（const 块作用域），但 `togglePool()` 事件处理
   函数是模块级，访问不到 → `ReferenceError` → 被 try/catch 吞掉
   → 按钮看起来"无反应"，仅短暂闪烁"池设置失败: nextPool is not
   defined"toast。

修复：将四个映射 (`POOL_NAMES` / `POOL_NEXT` / `POOL_BTN_LABEL` /
`POOL_BTN_TITLE`) 提到模块作用域；按钮文案改为当前状态名；保持
点击切换目标 (`POOL_NEXT`) 不变。

### 后端零改动

## 0.10.0

### Feature — 三池路由（priority / default / fallback 级联）

会话路由（scheduler_mode=session / credits）此前只有"选用"（单选 sticky
active）一种偏好：面板选中哪个账号，路由就粘在哪个账号上。本次新增
**三池划分**（写 auth 文件顶级 `pool` 字段，宿主 watcher 自动接管，重启
不丢），按钮三态循环切换：

- **优先池（priority）**：路由第一优先级。**只要优先池里还存在可用账号
  （未禁用、未耗尽、未冷却），路由就只在优先池内选择**——默认/兜底池
  账号不会随机漏入，即使面板"选用"的是默认账号。
- **默认池（default）**：所有未标记账号的默认归属。优先池为空、或全部
  优先账号 disabled / exhausted / cooling-down 时，路由级联到默认池。
- **兜底池（fallback）**：最后防线。仅当优先池与默认池都没有可用账号
  时，路由才使用兜底池，保证不因池级耗尽而 4xx/5xx 级联。
- **三级回落**：优先池内仍按原有规则跳过 exhausted / cooling-down 成员；
  逐级回落（优先 → 默认 → 兜底），全部不可用才 defer 内置调度。
- **live 切换**：面板按钮三态循环（默认 → 优先 → 兜底 → 默认），即点即
  生效（`POST /plugins/workbuddy/pool` body `{auth_index, pool}`），无需
  重启；每次 /accounts 刷新从磁盘重建 pool，手工改 auth 文件同样生效。
- **兼容迁移**：v0.9.x 的旧 `priority: true` 布尔标记自动映射为优先池，
  写入时统一收敛为 `pool` 字段。
- **session 粘性联动**：会话已 pin 到默认账号时，一旦优先账号出现，下次
  pick 自动把 binding 迁移到优先池；优先池耗尽后再迁回默认池。
- **删除清理**：删除账号时自动从 pool 移除，不会残留幽灵路由。

### API

- 新增管理端点 `POST /plugins/workbuddy/pool`（幂等，body
  `{auth_index, pool: default|priority|fallback}`，返回 `pool` 与
  `pool_sizes`）。
- `/accounts` 响应每账号新增 `pool`（default|priority|fallback）与
  `pool_sizes`（{priority: N, fallback: N}）；单卡 `/credits` 响应新增
  `pool`。

## 0.9.9

### Feature — 账户级 Failover：429/耗尽自动切换（阶梯指数退避）

会话粘性路由（scheduler_mode=session / credits）此前只认"耗尽/禁用"两种
不可用状态：账户返回 429 等错误时被当作软限流直接忽略，同一会话的后续
请求仍粘在同一个故障账户上，连续失败无法自动换账户。

本次新增按账户的运行时失败计数 + cooldown（`accountFailover.go`）：

- **阶梯指数退避**（按账户连续失败次数，成功一次即清零）：
  - 1 次失败 → cooldown 1 分钟
  - 2 次失败 → cooldown 3 分钟
  - 3 次失败 → cooldown 10 分钟
  - 4 次及以上 → 保持 10 分钟（封顶）
- **计入失败**：HTTP 429、402、5xx、传输层错误（status 0）、body 含
  rate limit / insufficient credit 等标记；**4xx 业务错误不计**。
- **生效范围**：cooldown 期间，`scheduler.pick` 直接跳过该账户（任何会话
  的新请求都路由到健康账户）；正在推理中的请求不中断；全账户 cooldown
  时保留当前 pin（与全耗尽 fallback 语义一致），不 defer。
- **会话粘性联动**：账户进入 cooldown 时 `evictSessionBindingsForAuth`
  立即清除指向该账户的所有 session binding，同会话下一次 pick 自动重分配。
- **成功即恢复**：该账户任何一次上游成功立即清零计数并解除 cooldown。
- **不写 auth 文件**：cooldown 仅内存标记（进程重启即重置），与现有
  lifecycle 的 disable/delete（402 硬耗尽）互不干扰。
- **开关**：plugin config `account_failover: false` 可整体关闭，恢复旧行为
  （默认开启）。
- 错误路径统一经 `noteAccountFailure`（`lifecycle.go`）记录并异步回填
  UID → auth.ID 规范键；`main.go` / `stream.go` 的传输层错误与同步流
  错误分支均已接入。

## 0.9.8

### Fix — 插件面板"暂无 WorkBuddy 账号"（账号列表为空）

v0.9.6 重命名 `providerName` 为 `workbuddy-provider` 后，`host_auth.go` 的
列表过滤使用 `providerName + "-"` → `"workbuddy-provider-"`；但
`authFileNameFor` 写入的文件名是硬编码 `"workbuddy-<uid>.json"`，**前缀
对不上**——`host.auth.list` 过滤后是空数组 → `/accounts` 返回
`accounts: []` → 面板渲染"暂无 WorkBuddy 账号"，且签到/领取/选择账号都
跟着失败。同时模型能正常用，因为 host 端调度器使用另一种读取路径，
不受 plugin 内部 prefix 过滤影响。

- **`authFilePrefix` 单一真相源**（`authfile.go:34`）：抽出文件名前缀为
  公共常量 `"workbuddy-"`。`authFileNameFor` 与 `host_auth.go` 都引用它，
  **解耦 plugin id 与文件前缀**——以后改名 providerName 不再撞。
- **`host_auth.go:54`**：列表过滤从 `prefix := providerName + "-"` 改为
  `prefix := authFilePrefix`。
- **测试守护**（`auth_prefix_test.go`）：锁死 prefix 常量值，断言
  `authFileNameFor` 输出 = `authFilePrefix + uid + ".json"`，断言 legacy
  无 UID 文件名也以同一 prefix 开头。

## 0.9.7

### Fix — 批量导入 UI 报错 `Failed to execute 'json' on 'Response'`

v0.9.6 起批量导入（多文件选择）面板在批量循环中常报
`Failed to execute 'json' on 'Response': Unexpected end of JSON input`，
6 个文件整批失败。两层根因，均已修：

**① 真根因（404）：panel API 常量未随 plugin id 更名**
0.9.6 plugin id 从 `workbuddy` 改为 `workbuddy-provider`，后端管理路由变为
`/v0/management/plugins/workbuddy-provider/*`，但 `panel.html:246` 的
`const API` 仍是旧路径 `/v0/management/plugins/workbuddy` → 面板**所有**
请求（导入/刷新/签到/领取/账号列表）全部 404，响应空 body / HTML 错误页，
前端解析必然失败。已改为 `workbuddy-provider`。

**② 放大（SyntaxError 吞真因）：`api()` 直接 `return r.json()`**
`api()` 在响应 body 为空或非 JSON 时抛 `SyntaxError`，把"哪个文件 / 哪次
请求失败 + HTTP 状态 + body 预览"吞成 JS 异常，单次坏响应拖垮整批。

- **`api()` 健壮化**（`panel.html:636-650`）：改为先 `await r.text()`，按
  `Content-Type` 分流——空 body / 非 JSON / JSON 解析失败均返回结构化
  `{error: "<HTTP 状态> (<content-type>): <body 前 200 字>"}`，不再 throw。
  401/403 仍按原语义 throw（认证/IP 封禁是终态，不该被批量循环吞掉）。
- **失败可观察**：批量导入的失败明细现在能告诉用户"第 3 个文件 HTTP 404
  empty response"这类真因，而不是千篇一律的 SyntaxError。
- 其他插件面板端点（accounts / credits / checkin / trial / select / toggle
  / keepalive / refresh）共享同一 `api()`，同步受益——任意端点拿到空或非
  JSON 响应时 UI 不再白屏。

## 0.9.6

### Rename — plugin `id` `workbuddy` → `workbuddy-provider`

Cpa 客户端按 `providerName` 常量显示插件侧栏名字；按 `<id>.so/.dylib/.dll`
加载插件身份。这次 id 改名后，老 plugin（如 `workbuddy.so`）仍可在侧栏共存，
但 `管理 → 安装` 列表里看到的是新插件 `WorkBuddy Provider`。

- `workbuddy/main.go: providerName` 由 `"workbuddy"` 改为 `"workbuddy-provider"`
- `var version` / `VERSION`：0.9.5 → **0.9.6**
- `registry.json`：plugin id 同步改为 `workbuddy-provider`
- build.yml 发布矩阵同步改为 `id: workbuddy-provider`
- 配套：`qoderwork` 0.2.7 → **0.2.8** （id `qoderwork-provider`）、`token-usage-tracker`
  0.1.6 → **0.1.7** （id `workbuddy-token-usage`）

## 0.9.5

### Rename — 插件显示名 `WorkBuddy` → `WorkBuddy Provider`

3 插件因 cpa-workbuddy-plugin 项目整体改名，registry 显示名一起区分避免
与旧同名插件混淆。`id` 保持 `workbuddy` 不变，所以已装的旧实例不会被新
release 替换（安装记录靠 `id` 寻址）。

- `registry.json`：`name` 字段 `WorkBuddy` → `WorkBuddy Provider`
- `var version`/`VERSION`：0.9.4 → 0.9.5
- 历史 release `workbuddy-v0.9.4` 已 delete，新 release `workbuddy-v0.9.5` 接管 registry

> 配套改动同 commit：`qoderwork` 0.2.6 → 0.2.7（显示名 `QoderWork Provider`）、
> `token-usage-tracker` 0.1.5 → 0.1.6（显示名 `WorkBuddy Token Usage`）。

## 0.9.4

### Fix — 共享 feed 中 `source` / `service_tier` 字段语义对调

`token-usage-tracker` dashboard 的"请求明细"在 0.8.9 拆出后出现了列值错
位：每条记录的「来源」列恒为字面量 `workbuddy`，而「Tier」列被填的是账
号 UID（`17625821743` 等）。根因是 workbuddy 在 feed 里把账号身份
（`sa.Account.Nickname`，兜底 `authUID`）误塞进了 `service_tier` 字段，
而 `source` 被硬编码成 `"workbuddy"`，导致 dashboard 把账号身份渲染到了
价格 tier 列里。

- **调整**（`workbuddy/usage_feed.go:165-189`）：
  - `source` 现在写入 `accountLabel`（`sa.Account.Nickname`，没设昵称
    时回退到 `authUID`）—— 此即用户在 dashboard「来源」列想要看到的
    "workbuddy 内的账号名"。
  - `service_tier` 写空串 —— workbuddy 当前调用链没有从上游 chat 接口
    解出真正的 tier，保留原"语义错位"反而误导用户。cost 侧在空
    `service_tier` 时直接走默认价表（`token-usage-tracker/usage_stats/cost.go:437-443`），
    不会因 tier 空值而报错。
- **形参重命名**（`publishUsage` / `pumpUpstreamStream` / `recordUsageFeed`
  最后位置参）：`serviceTier → accountLabel`，让"形参名"和它传递的真实含
  义（workbuddy 内的账号标签）对齐。调用方 `main.go:683, 755`、`stream.go:71`
  同步更新变量名 + 注释。
- **消费侧零改动**：`token-usage-tracker` 列绑定（`source → 来源`、
  `service_tier → Tier`）本就正确，UI 不需要任何修改。
- **存量数据**：本地 bbolt 里旧的 `service_tier` 字段不会被改写，dashboard
  上历史行仍按旧值（UID）显示；如需完全切到新列含义，可走
  `token-usage-tracker` 的"重置统计"。新增请求立即以新语义记录。
- **测试**：`usage_feed_test.go` 同步更新断言——`source` 期望账号标签、
  `service_tier` 期望空串。

### Feature — 多 JSON 凭证文件一键批量导入

「导入凭证」弹窗在保留原有粘贴 JSON 的基础上，新增多文件选择入口：

- **文件选择**：`📁 选择 JSON 凭证文件`（`<input type=file multiple>`），一次可
  选多个 `.json`，选完即展示待导入清单（文件名 + 大小），可一键清除。
- **批量语义**：每个文件独立走现有单凭证 `/import` 端点（逐条串行），单文件
  失败跳过继续，不因一个坏文件中断整批。
- **限流兼容**：插件层 per-IP token bucket（capacity 5 / refill 1/6s）对批量
  突发会返回 429，前端识别 `rate limit` 后自动退避 7s 重试（最多 3 次），不
  再把限流误报为导入失败。
- **结果报告**：全部成功自动关弹窗刷新面板；有失败时弹窗内展示
  `N 成功 / M 失败` 明细清单（文件名 + 原因），供修正后重试。
- **边界**：非 `.json` / 超 2MB / 空内容文件在选择阶段即跳过并提示；粘贴内容
  与文件可混用，按「先粘贴后文件」顺序导入。
- 纯前端改动（`panel.html`），后端 `/import` 契约不变。

## 0.8.9

### Change — 本地用量统计拆分为独立插件 token-usage-tracker(v0.1.0)

v0.8.8 把社区插件 `cap-token-usage-tracker` 合并进 workbuddy 的方向被撤回:
用量统计与 dashboard 由本项目**第三个插件** `token-usage-tracker` 独立提供
(与 workbuddy、qoderwork 并列),workbuddy 只负责产出数据。

- **移除**:`usage_stats/` 子包、`usage_stats_bridge.go`、`/usage` 页面与
  全部统计路由、`usage_stats_*` 配置项。`/v0/resource/plugins/workbuddy/*`
  只剩积分面板 `/panel`,此前的 dashboard API 404 不再出现。
- **新增共享 usage feed**(workbuddy → token-usage-tracker 的唯一数据通道):
  `publishUsage` 在转发 CPAMP 的同时,把每次请求的 `usage.Detail` 以
  NDJSON 追加写至 `<CLIProxyAPI root>/data/token-usage-feed.ndjson`(默认),
  打开即写即关(O_APPEND),超过 128MB 自动截断轮转。之所以不用共享 bbolt
  库:两个长驻进程无法同时持有 bbolt 排它文件锁。
- **新配置项**(`config_yaml`,均可选):
  - `usage_feed_enabled`(boolean,默认 true)
  - `usage_feed_path`(string,默认 `<CLIProxyAPI root>/data/token-usage-feed.ndjson`;需与 token-usage-tracker 的 `usage_feed_path` 一致)
- **配套插件**:安装 `token-usage-tracker` v0.1.0 后,在插件商店打开其
  dashboard("Token 用量" 菜单,`/v0/resource/plugins/token-usage-tracker/usage`)
  即可看到 workbuddy 账户的实时 token 消耗(轮询间隔默认 5s)。

## 0.8.8

### Feature — 本地 Token 用量统计(合并 cap-token-usage-tracker)

把社区插件 `cap-token-usage-tracker`(AITNR)合并进 workbuddy,解决"插件
executor 请求宿主 UsagePlugin 广播为空、独立 token 统计插件检测不到消耗"
的根因问题。

- **数据源改造**:统计不再依赖宿主 `UsagePlugin` 广播(插件 executor 适配器
  不发布 usage,广播队列恒为空),改为 workbuddy 执行链路内部采集——
  `usage.go` 的 `publishUsage`(非流式/流式全部请求的汇聚点)在转发 CPAMP
  的同时,把同一份 `usage.Detail` 写入本地 bbolt 库(`usage_stats` 子包,
  actor 异步落盘,256 缓冲,不阻塞热路径)。
- **新子包 `usage_stats/`**:移植 tracker 的存储/聚合/解码模块(usage、
  aggregate、persistence、cost、pricing、modelsdev、exchange_rate、config、
  preferences、dashboard、request_log、compression、handover + 4 语言
  locales)。裁剪:full-mode 管理密钥会话、API Key 加密/指纹、
  authRuntimeLookup 身份解析(改为直接用 auth_index=UID)。
- **新配置项**(`config_yaml`,均可选,默认开箱即用):
  - `usage_stats_enabled`(boolean,默认 true)
  - `usage_stats_path`(string,默认 `<CLIProxyAPI root>/data/usage-stats.db`)
  - `usage_retention_days`(integer,1-3650,默认 365)
  - `usage_flush_interval`(string,1s-1h,默认 5s)
  - `usage_flush_max_records`(integer,1-1000000,默认 100)
- **新页面**:`/v0/resource/plugins/workbuddy/usage`(菜单 "Token 用量"),
  与积分面板 `/panel` 并存。读接口(统计/趋势/分组/请求/成本/价格/偏好/
  汇率)走 resource 路由,写接口(价格、重置)走 management 路由,纳入既有
  `management_key` 鉴权/限流门。
- **降级语义**:统计库打开失败仅禁用本地统计,chat 与 CPAMP 上报不受影响。
- **测试**:新增 `usage_stats` 冒烟测试(bridge 调用链黑盒回归);
  主包与子包全量测试通过。

## 0.8.7

### Change — `session` is now the default scheduler mode

- `scheduler_mode` 默认值从 `off` 改为 `session`:多账户部署开箱即用按会话
  轮询(同一会话 1h 粘性同一账户,不同会话分散)。单账户无感知。
- `usage_config.go` — `configure()` 未配置 `scheduler_mode`(或值非法)时
  回落到 `session`;`scheduler.go` 全局初值同步。
- `main.go` — ConfigField 枚举顺序与描述更新(session 标注 DEFAULT)。
- 行为变化提示:默认不再 defer 给 CPA 内置调度。若想完全交给内置调度,
  需显式配置 `scheduler_mode: off`。

## 0.8.6

### Feature — per-conversation account routing (`scheduler_mode: session`)

多账户会话级轮询:同一会话 1 小时内粘性绑定同一账户,不同会话轮询分配
不同账户,避免所有流量压在一个面板选中账户上。

- `session_auth.go` (new) — 会话粘性路由核心:
  - `sessionKey → {AuthID, ExpiresAt}` 映射,默认 1h TTL,`RWMutex` 保护,
    后台 janitor 每 5 分钟清理过期绑定;
  - 会话键提取优先级(仅用 scheduler.pick 请求 Options 中宿主可见信号):
    `execution_session_id`(显式执行会话) > 客户端会话头
    (`X-Claude-Code-Session-Id` / `Session-Id` / `Session_id` /
    `X-Session-ID` / `X-Session-Affinity` / `X-Client-Request-Id`) >
    `derived_session_id`(宿主从会话根派生的稳定哈希);
  - 分配策略:未绑定账户优先 → 全绑定后轮询取模;绑定过期或账户被
    禁用/耗尽时自动重分配;所有账户不可用时保留现有 pin;
  - 无会话标识的请求回落面板选中账户(与 `credits` 模式行为一致)。
- `scheduler.go` — `handleSchedulerPick` 支持 `schedulerModeSession` 分支;
  新增 `schedulerModeSession = "session"` 常量。
- `usage_config.go` — `configure()` 解析 `scheduler_mode: session`。
- `main.go` — `scheduler_mode` ConfigField 枚举增加 `session` 并更新说明。
- `panel.go` — `/accounts` 响应新增 `scheduler_mode` 字段(面板可感知当前
  路由模式)。
- `session_auth_test.go` (new) — 12 个用例:会话键提取优先级、同会话粘性、
  异会话均匀分配、TTL 过期释放复用、账户耗尽重分配、无会话回落面板、
  全耗尽保 pin、完整 `handleSchedulerPick` 链路。

## 0.8.2

### Concurrency + lifecycle hardening

- `lifecycle.go` — P0-2: `reconcileOneAccount` now routes credits fetch
  through `cachedAccountDetails(force=true)` so singleflight serializes
  concurrent writers, eliminating a Load→Store race that could clobber
  newer plan/checkin values.
- `lifecycle.go` — P1-4: Global `lifecycleDelete` now requires a second
  `fetchUserResource` confirmation before deleting. Prevents transient 402
  from irreversibly removing an account.
- `checkin.go` — P1-5: after a successful checkin the credits cache is
  refreshed immediately (was only updating the checkin field). Panel now
  shows updated balance without waiting for the async reconcile pass.
- `cache.go` — P1-1 documented trade-off: force=true callers still join
  singleflight (skipping would re-introduce P0-2).
- `main.go` — P0-5: `scheduler_mode` ConfigField description now warns that
  `off + lifecycle_auto=false` leaves exhausted accounts routable.

## 0.8.1

### Bug fixes + compliance polish

- `keepalive.go` (new) — daily 22:00 access-token refresh to prevent Keycloak
  offline-session expiry; reuses `schedulerLoop`, routes via `host.http.do`,
  uses CPA native `disabled` field for session-dead auths.
- `models.go` — fix `filterExcludedModels` slice aliasing that corrupted
  `dynamicModelsCache` (P0).
- `billing.go` — route all billing API calls through `hostHTTPDo` (was missed
  in v0.7.0); improve "parse failed" error to include a redacted body snippet.
- `checkin.go` — avoid double `fetchCheckinStatus` in classify already-branch.
- `billing.go` — `performCheckinCall` now sets `success=true` as bool to avoid
  downstream type-mismatch when upstream returns a string.
- `host_auth.go` — fresh slice in `hostAuthList` to avoid aliasing RPC response.
- `oauth.go` — route `handleRefreshAuth` via `hostHTTPDo` (last path still on
  `sharedHTTPClient()`); make OAuth error messages actionable.

## 0.8.0

### Refactor — community-grade file layout

完成 v0.7.0 合规改造后的代码组织大重构，把两个超大主档拆成单一职责的
小文件，对齐 CPA 原生 plugin 案例的"一个能力一个文件"原则。

**File splits (main.go 2940 → 809, management.go 2263 → 349, lifecycle.go 980 → 535)：**

- `redact.go` (49) — redactSecrets + 4 个 regex + truncate
- `usage.go` (242) — handleUsage + publishUsage + forwardUsageToCPAMP + sseUsageCollector
- `payload.go` (469) — prepareUpstreamBody + 4 个 InPlace mutator + 4 个 legacy 包装
- `stream.go` (452) — streamEmit/Close + pumpUpstreamStream + collectUpstreamStream + aggregate*
- `models.go` (443) — callModelsAPI + fetchDynamicModels + resolveUpstreamModel + alias 反解
- `oauth.go` (240) — handleStartLogin/PollLogin/RefreshAuth + newLoginClient + doJSON
- `host_bridge.go` (388) — hostHTTPDo/DoStream/Read/Close + hostStreamReader + Direct fallbacks
- `billing.go` (486) — billing API + fetch* + perform* + JSON helpers
- `cache.go` (183) — accountCache + accountDetailFlight singleflight + prune
- `host_auth.go` (73) — hostAuthList/Get/GetBundle (host auth-store RPC)
- `usage_config.go` (202) — configure + resolveUsageReport + probe* + config vars
- `checkin.go` (515) — handleManualCheckin + runAutoCheckin + schedulerLoop + classify/execute/summarize
- `credits_handler.go` (285) — handleImportAuth/CheckinConfig/ClaimTrial/SelectAuth/CreditsQuery
- `panel.go` (266) — buildDashboardEx + summarizeCredits + servePanel + panelHTML
- `policy.go` (188) — lifecycleAction decisions + displayNote + labelForAuth
- `authfile.go` (299) — authFileNameFor/sanitizeUIDForFileName/hostAuthPersist/deleteAuth + path safety

**保留的小文件**：`scheduler.go` (138)、`active_auth.go` (158) — 本来就够小。

**文档（社区标准）：**

- `README.md` — 英文版，Features / Quickstart / Configuration / Lifecycle / Development / License
- `README_CN.md` — 中文版
- `LICENSE` — MIT
- `Makefile` — build / test / lint / clean / release / tag 目标
- `.gitignore` — 忽略 `*.so` / `*.h` / `bin/` / `dist/`
- `docs/architecture.md` — 模块图 + 数据流 + 关键设计决策 + 与 CPA 的集成点
- `docs/development.md` — 本地构建 / 测试 / 调试 / 发布流程
- `docs/definition-of-done.md` — v0.8.0 验收标准（量化可测）

### Lint / style

- `gofmt -l .` → 0 files
- `go vet ./...` → 0 issues
- `gocritic check ./...` → 0 issues（修复 policy.go 的 ifElseChain）
- `staticcheck` 真实代码问题 0（工具链版本噪音已过滤）

### Bug Fixes (carried over from v0.6.31 / v0.7.0)

本次重构完整保留了之前所有 bug 修复：
- UID 路径穿越白名单（authfile.go sanitizeUIDForFileName）
- refresh_token 不再泄露到 chat 上游（main.go backendHeaders）
- invalidateAccountCredits 数据竞争修复（值拷贝）
- handleManualCheckin early-already merge（不丢 credits/plan）
- configure 嵌套锁修复（parse-then-lock）
- scheduler_mode off 接通（handleSchedulerPick 读取配置）
- deleteAuth 调 clearActiveAuthIfMatch
- runAutoCheckin 串行改并发（sem=4）
- cachedAccountDetails singleflight
- panel.html XSS 修复（addEventListener + dataset）
- panel.html CSRF（fetch credentials:omit）
- redactSecrets 裸 JWT 兜底
- pumpUpstreamStream context cancel
- out[:0] 共享底层数组改新 slice
- 热路径 4 次 JSON 序列化合并为 1 次
- 冒泡排序改 sort.Slice
- usageReportConfigured/buildDashboard 死代码删除
- handleManualCheckin 三段拆分（classify/execute/summarize）
- management BasePath 缓存（register 时读取宿主注入）

### Tests

- 115/115 tests pass (`go test -race`)
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为

## 0.7.0

### Compliance — CPA native patterns
本次大版本把「自建通道」全部替换为 CPA 官方提供的 RPC / 能力接口，
对齐 `sdk/pluginapi` 的设计意图。生产路径 100% 走宿主桥接，插件不再
绕过宿主审计 / request-log / transport policy。

- **所有上游 HTTP 调用走 `host.http.do` / `host.http.do_stream`**：
  - `models API`、`billing API`、`usage 上报`、`chat completions`（流式 + 非流式）
    全部从 `sharedHTTPClient().Do` 切到 `hostHTTPDo` / `hostHTTPDoStream`。
  - 宿主 request-log 现在能捕获插件的出站请求和原始响应（之前完全看不到）。
  - 宿主 transport policy（proxy、超时、连接池）对插件上游调用生效。
  - `sharedHTTPClient` 降级为 fallback 专用：仅当宿主桥不可用（单元测试 /
    老版本 CPA）时使用。新代码直接调用 `sharedHTTPClient` 视为合规 bug。
- **`hostStreamReader` 适配层**：把宿主桥的 32KB 任意字节块适配为 `io.Reader`，
  `bufio.Scanner` 的 SSE 行切分逻辑不变，pump / collect / aggregate 全部透明迁移。
- **`UsagePlugin` 能力声明 + `handleUsage` RPC handler**：
  - 注册能力 `usage_plugin: true`，宿主每次请求完成后会把规范化的
    `pluginapi.UsageRecord` 推送给插件。
  - 插件在 `handleUsage` 里把 record 转发到 CPAMP，与宿主 `DefaultManager`
    的记录并行，不再重复也不遗漏。
  - 旧路径 `publishUsage` 保留向后兼容（老版本 CPA 没接 UsagePlugin 时仍可
    上报），新路径 `handleUsage` 同步触发，CPAMP 侧基于 (timestamp + auth +
    model + total_tokens) 幂等去重。
- **`reportUsageToCPAMP` 重命名为 `forwardUsageToCPAMP` 并走 host.http.do**：
  CPAMP 上报自身也走宿主桥，宿主能看到插件的运维流量。

### Architecture notes
- `hostBridgeAvailable()` 检查 `hostAPI.call` 是否为 nil，统一决定是否
  fallback。生产环境永远为 true，单元测试永远为 false（无宿主）。
- 所有 `*Direct` 函数仅服务测试；生产路径不经过。
- 宿主侧 `sanitizePluginRequest` 会把 `ExecutorRequest.HTTPClient` 置 nil
  （跨 c-shared 边界接口无法传输），所以**插件不可能用宿主注入的
  HTTPClient**——`host.http.*` RPC 是 c-shared 插件访问宿主 transport 的
  唯一合规方式，本版本全部采用。

## 0.6.31

### Security
- **UID 路径穿越修复**：`authFileNameFor` 新增 `sanitizeUIDForFileName` 白名单
  （`[^a-zA-Z0-9_-]+` → `_`、长度 ≤64、拒绝 `.`/`..`），导入凭证的
  `workbuddy-<uid>.json` 不再可能被 `../` 注入到任意路径。
- **refresh_token 停止泄露到 chat 上游**：`backendHeaders` 移除
  `X-Refresh-Token`。refresh_token 是长期凭证，只在 refresh 端点用；之前每次
  chat completion 都附带它，上游日志一旦记录请求头即等同账号被盗。
- **插件层 management 鉴权 + 限流**：`handleManagement` 入口对所有 POST /
  写端点新增插件层防护：constant-time Bearer 比对（`crypto/subtle`），
  per-IP token-bucket 限流（容量 5、每 6s 1 个）。配置方式：
  `config_yaml management_key:` 或 env `WB_MANAGEMENT_KEY`。空则保持
  历史行为（仅依赖宿主鉴权）。
- **panel.html XSS 修复**：4 处 `onclick="...('${esc(auth_index)}',this)"`
  改为 `data-action` + `data-auth-index` + `addEventListener`。`esc()` 只
  转义 HTML 不防 JS 字符串上下文注入。
- **panel.html CSRF 缓解**：`fetch` 显式 `credentials:'omit'`，面板纯靠
  Authorization Bearer，不再隐式带 cookie。
- **redactSecrets 兜底裸 JWT**：新增 `redactREJWTLoose` 正则，匹配不带
  `Bearer` 前缀、`access_token` key 的 `eyJ…` 两段/三段 JWT。

### Bug Fixes
- `invalidateAccountCredits` 数据竞争：直接改 sync.Map 共享 entry 的字段
  （`e.credits = nil`），并发 dashboard / reconcile / chat 后置 invalidate
  会拿到撕裂状态。改为 `fresh := *e; Store(&fresh)` 值拷贝，与其他 4 处
  写法一致。
- `handleManualCheckin` "early already" 路径丢 credits/plan：直接构造
  `accountCacheEntry{checkin: ci}` 覆盖整个 entry，签到后面板积分消失。
  改为 merge prev 的 credits/plan。
- `configure` 嵌套锁：在 `checkinAutoMu` 内嵌套获取 `lifecycleAutoMu` /
  `schedulerModeMu`，未来加反向获取路径即死锁。改为两阶段：无锁解析到
  局部变量，再分别单锁写入。
- `scheduler_mode: off` 配置断链：configure 解析但 `handleSchedulerPick`
  从不读取，"off" 实际表现为 "credits"。现在 off 正确 defer 给内置 scheduler。
- 删除 Global 账号后 `activeAuthID` 残留指向已删 ID：`deleteAuth` 两个成功
  路径现在都调 `clearActiveAuthIfMatch(authID)`。
- `runAutoCheckin` 重复 `fetchCheckinStatus` + 变量 shadow：原代码内层
  `ci` shadow 外层，且第二次调用与第一次状态可能不一致。改为单次调用，
  签到成功才 refresh。
- `out[:0]` 共享底层数组：`filtered := out[:0]` 复用底层数组在 range 中
  写入，改为 `make([]wbAccount, 0, len(out))`。
- `pumpUpstreamStream` 无 context：`http.NewRequest` 无 context，客户端
  断开后 goroutine 一直读到 120s 超时。改为 `NewRequestWithContext` +
  cancel 传入 pump，所有退出路径释放。

### Performance
- **热路径 4 次 JSON 序列化合并为 1 次**：新增 `prepareUpstreamBody` 统一
  `forceStreamBody` + `normalizeToolsForUpstream` + `rewriteSystemForUpstream`
  + `ensureSystemMessage` + `rewriteModelInBody`，单次 unmarshal + 单次
  marshal。每次 chat completion 省 4-5 个 JSON 往返。
- **`runAutoCheckin` 串行改并发**：抽出 `processAutoCheckinAccount`，主循环
  `sem=4` 并发。N 账号从 3N 串行 HTTP 降到并发 4 路。
- **`cachedAccountDetails` 加 singleflight**：per-authID `sync.Map` + done
  channel。并发 dashboard / reconcile 对同一账号只跑 1 次上游 fetch，
  其他 goroutine 等结果，消除 6x upstream QPS + last-writer-wins。
- **冒泡排序改 sort.Slice**：`pruneAccountCacheSoftCap` 从 O(n²) 降到 O(n log n)。

### Refactor
- **handleManualCheckin 273 行拆分**：`classifyCheckinTargets` /
  `executeCheckinBatch` / `summarizeCheckinResults` 三段独立函数，各自
  单一职责，便于单测。
- **management BasePath 不再硬编码**：register 时缓存宿主注入的 BasePath，
  handleManagement 用 cached 值。宿主未来版本化路径不会失效。
- 死代码清理：删 `upstreamBase` legacy 常量、`usageReportConfigured` 无人
  调用、`buildDashboard` 包装函数。

### Tests
- 新增 `TestSchedulerPick_OffMode_Defers` 覆盖 scheduler_mode=off 行为。
- 全套 115 tests + `-race` 通过。

## 0.6.29

### Fixed
- 修复签到后按钮不变"已签到"、套餐标记丢失的问题
  根因：handleManualCheckin/runAutoCheckin/handleClaimTrial 在签到/领取成功后
  accountCache.Delete(f.ID) 把 cache 清了，light load 时 checkin/plan 是 nil。
  handleCreditsQuery 的 cache merge 逻辑从 prev.plan（空）取值而不是用刚获取的
  fetchPaymentType(sa) 结果，导致 plan 在 light load 后丢失。
  修复：签到/领取成功后把 checkinSummary 存回 cache 而不是删除；
  handleCreditsQuery cache merge 用刚获取的 plan；runAutoCheckin/handleClaimTrial
  改为 invalidate credits（置 nil）而不是删除整个 cache entry。

## 0.6.28

### Fixed
- 修复面板选中卡片与实际路由账号不一致的根本问题
  根因：activeAuthID 存的是 auth.Index（运行时 SHA256 hash），但 scheduler
  的 SchedulerAuthCandidate.ID 是 auth.ID（持久化 UUID），两者永远不匹配，
  导致 pickActiveAuth 永远走 fallback 选第一个，面板显示选中第一个但实际
  路由到别的账号。同时 cachedCreditsScore 用 auth.ID 查 accountCache（key
  是 auth.Index）也查不到，exhausted 判断也坏了。
  修复：全链路统一用 auth.ID — activeAuthID、accountCache key、
  lifecycleState key、面板 selected 判断、/select API 返回值全部改用
  auth.ID。lifecycle 函数（reconcileOneAccount/disableAuth/reenableAuth/
  deleteAuth/syncAuthNote）加 authID 参数，resolveAuthIndex 改为
  resolveAuthIndexAndID 同时返回 index+ID。
- 修复首次加载面板时选中耗尽账号的问题
  首次 GET /accounts 不拉 credits（fetchCredits=false），所有卡片
  Exhausted=false，ensureDefaultActiveAuth 选第一个。lazyLoadCredits
  异步获取积分后发现第一个已耗尽，但选中状态不会更新。
  修复：lazyLoadCredits 全部完成后前端静默再拉一次 /accounts（此时
  cache 已有 credits，light load 能拿到正确 exhausted 和 selected），
  重新渲染卡片。

## 0.6.27

### Fixed
- ensureDefaultActiveAuth 也检查 Exhausted：面板刷新时选中账号已耗尽会同步切换
  修复 scheduler.pick 切了但面板 ensureDefaultActiveAuth 又选回去的 race
  现在 pickActiveAuth 和 ensureDefaultActiveAuth 用同一套规则，选中状态不会漂移

## 0.6.26

### Fixed
- 选中账号积分耗尽时自动切换到第一个可用账号，并同步更新选中状态
  全部耗尽时留在当前账号不 flip-flop
  修复 v0.6.25 过度 sticky 导致耗尽后一直报错的问题

## 0.6.25

### Fixed
- 选中账号 sticky：scheduler 不会因缓存过期/积分耗尽自动切换到别的账号
  只有 host 把选中账号从候选列表移除（disabled/deleted）才切换
  修复面板显示选中A但实际路由到B、静默消耗积分的问题

## 0.6.24

### Fixed
- model.static / model.for_auth 现在尊重 CPA 的 oauth-excluded-models 配置
  在 config.yaml 的 oauth-excluded-models.workbuddy 里列出的模型不再出现在 /models

## 0.6.23

### Fixed
- usage import URL 自动探测：先试 127.0.0.1:18317（裸机/Docker host），再试 Docker 服务名 cpa-manager-plus:18317
  不再写死 Docker 服务名，裸机安装也能自动找到 CPAMP

## 0.6.22

### Fixed
- ExecutorModelScope 改为 OAuth：插件只处理 workbuddy auth 绑定的模型
  不再拦截其他 openai-compatible 供应商的同名裸模型（如 deepseek-v4-flash、glm-5.2）
  修复启用 workbuddy 后自定义供应商模型请求不进监控的问题

## 0.6.21

### Fixed
- 积分懒加载改为并发：所有卡片同时请求，不再逐个排队

## 0.6.20

### Fixed
- 懒加载积分时同时拉取 plan（套餐类型），修复 plan 徽章显示「-」不更新

## 0.6.19

### Added
- 每张卡片新增「刷新」按钮：单独查询积分并即时更新该卡

## 0.6.18

### Added
- 积分懒加载：进页面先渲染骨架卡（加载中…），逐卡异步拉积分，失败自动重试一次
- 后端 `/accounts` 默认不再并发拉所有账号 credits（避免上游 500）
- `/credits?auth_index=` 单账号查询返回完整字段（region/exhausted/trial_claimed）

### Fixed
- 缓存有效时仍返回缓存的 credits，不再触发上游请求

## 0.6.17

### Fixed
- 流式路径也强制 `stream:true`：WorkBuddy API 现仅支持 stream 模式，`stream:false` 会报 "Non-stream chat request is currently not supported"

## 0.6.16

### Fixed
- 夜间模式：用量汇总卡与账号卡统一 `--card` 底色；内部指标格改用 `--surface`，避免汇总卡看起来更深/发黑

## 0.6.15

### Added
- 面板「选用」账号：默认第一张可用卡；选中卡决定 CN/Global 路由（读 domain，不解码 JWT）
- 选中账号耗尽/禁用/消失时随机切换下一张可用卡并记住

### Changed
- scheduler.pick 改为始终跟随 active 选中账号（不再依赖 credits 排行模式）

## 0.6.14

### Fixed
- Global 账号聊天 401/400 修复：JWT iss=workbuddy.ai 必须走 www.workbuddy.ai 端点（copilot.tencent.com 会对 Global token 返回 401）
- Global 请求自动注入 system message（www.workbuddy.ai 对 user-only 请求返回 code 11101）
- token 刷新和 models 发现也走域名感知端点

## 0.6.13

### Changed
- 请求监控 key 自动探测：config → env（CPAMP_ADMIN_KEY/USAGE_REPORT_KEY）→ docker secret `/run/secrets/cpamp_admin_key`，无需手写 usage_report_key


## 0.6.12

### Changed
- 删除无效 `usage.PublishRecord` 路径，请求监控仅走 CPAMP `/v0/management/usage/import`


## 0.6.11

### Fixed
- **请求监控**：c-shared 隔离导致 `usage.PublishRecord` 进不了宿主 redisqueue；改为异步 POST CPA-Manager-Plus `/v0/management/usage/import`（`usage_report_url`/`usage_report_key`）
- 补全 ExecutorType/AuthType/Source；配置字段暴露于管理面板


## 0.6.10

### Fixed
- **批量签到先过滤再操作**：Global 不参与；今日已签跳过；仅对 CN 未签账号调用 daily-checkin
- 返回 `summary{success,already,skipped_global,fail,eligible}`，面板文案不再把 Global/已签当失败
- 分类/签到并发（限流），降低「全部签到」卡到 502 context canceled

## 0.6.9

### Changed
- **Panel theme adaptive**: CSS variables now default to light (paper) theme; `[data-theme="white"]` and `[data-theme="dark"]` overrides align with CPA management panel tokens. Embedded iframe mirrors parent `data-theme` via MutationObserver; standalone page follows `prefers-color-scheme`. All hardcoded dark colors (toast, modal, input, buttons) replaced with theme-aware CSS variables.

## 0.6.3

### Fixed
- Auth identity: parse/refresh leave ID empty; regression tests (A-01)
- Stream pump: emit failure is failed usage; defer streamClose (A-06)
- No dual-write after host.auth.save (A-15)
- Scheduler skips host-disabled candidates (A-04)
- Global delete reconstructs path via peer auth dir (A-07)
- Panel IP ban wait parses upstream window (A-08)
- accountCache concurrent errs race + soft cap (A-02)
- Dashboard single host.auth.get per row (A-05)
- Instant check-in/trial button state (panel)


## 0.6.2

### Fixed
- **Credits look frozen after chat**: cache TTL 5m→45s; invalidate cache after successful chat (stream + non-stream)
- **Spend math**: package used = cycle size−remain; account total_size from package sizes; TotalDosage treated as capacity pool (not consumption)
- **Check-in packs inflate "available"**: UI labels 可用/已用/额度池 so grant vs spend is visible; note shows 余/已用/池

## 0.6.1

### Added
- WorkBuddy panel **用量汇总**：筛选范围内 剩余/已用/总量/占比 + 进度条；全部视图附 CN/Global 分项
- Dashboard API `summary` 字段：`total_remain` / `total_used` / 分区域统计

### Notes
- CPAMP Auth 页进度条仅支持内置 `codex/claude/kimi/xai/antigravity`（`QUOTA_PROVIDER_TYPES` 白名单）；workbuddy 无法靠 `note` 注入进度条，完整用量看插件面板

## 0.6.0

### Added
- **Credit lifecycle** (plugin-only, no CPA/CPAMP source changes):
  - CN exhausted → write auth file `disabled:true` (host skips scheduling)
  - Global exhausted → **delete** auth file (`os.Remove` on path from `host.auth.get`)
  - CN disabled + credits return (after check-in / refresh) → `disabled:false`
  - Executor hard credit errors → async reconcile; pure 429 does not delete Global
  - Unknown credits → no-op (safe default)
- Auth file **note** / **label** enrichment: `CN · 余 x · …` / `Global · …` / 已禁用
- Panel: CN/Global filter tags + counts; disabled badge; lifecycle toast on refresh
- Panel: management-key discipline to avoid CPA IP ban (no request without key; 401/403 backoff)
- Config field `lifecycle_auto` (default true)

### Changed
- Scheduled tick **no longer auto-claims Global trial** (one-shot; manual `/trial` / panel only)
- Tick = CN check-in (if `checkin_auto`) + lifecycle reconcile for all regions
- Import/save writes top-level `type`/`logo`/`note`/`disabled` with nested auth/account
- Force dashboard refresh runs lifecycle and may drop deleted Global rows

### Notes (CPAMP Auth page)
- Filter letter **「W」** / brand typeBadge colors cannot be fixed from the plugin (frontend static icon table)
- Plugin sets `Metadata.logo` + registration Logo; Auth cards show **note** for region/credits summary
- Full UX: WorkBuddy side panel

## 0.5.0

### Added
- International (Global) WorkBuddy account support (`www.workbuddy.ai` domain)
- Domain-aware billing API routing: CN accounts → `codebuddy.cn`, Global → `workbuddy.ai`
- Expert trial pack claim API: `POST /plugins/workbuddy/trial` (Global only, one-time 250 credits / 14 days)
- Panel region badges: light green `CN` (daily checkin) + light orange `Global` (expert trial)
- "全部领取" batch claim button for Global accounts
- Auto-scheduler region branch: CN → daily checkin, Global → claim expert trial if unclaimed
- `wbAccount.region` and `wbAccount.trial_claimed` fields in accounts API response
- `hasTrialPack()` helper detects trial pack from `get-user-resource` packages

### Changed
- `billingBase` selection is now domain-driven via `billingBaseFor(sa)`
- `backendHeaders` Origin/Referer dynamically set per account domain via `originRefererFor(sa)`
- Panel card buttons: CN → 签到, Global → 领取专家加油包 / 已领取
- "全部签到" button only triggers CN accounts (Global accounts are skipped with a message)
- `runAutoCheckin` branches by region: CN daily checkin, Global trial claim

## 0.4.3

### Changed
- Panel import modal: white surface + dark text for readable contrast (was dark-on-dark)

## 0.4.2

### Changed
- Panel: credential import is a toolbar button (left of 刷新数据) opening a modal, instead of an always-visible card

## 0.4.1

### Added
- Panel **耗尽** badge + `exhausted` field on accounts API (shared with scheduler)
- Credential **import** API `POST /plugins/workbuddy/import` + panel paste UI
- Per-account check-in lock (multi-tab safe)
- `executor.count_tokens` stub (`input_tokens:0` — upstream has no API)
- LICENSE (MIT), VERSION file, GitHub Actions multi-arch release workflow

### Changed
- SSE cleanChunk strips empty `extra_fields` / `refusal` / `reasoning_content`
- Scheduler credits mode prefers non-exhausted accounts first

## 0.4.0

### Added
- CPA **Scheduler** capability with `scheduler_mode`: `off` (default) | `credits`
- Credits-aware multi-account pick using panel credit cache

## 0.3.18

### Fixed
- ConfigFields use SDK `ConfigFieldType*` constants

## 0.3.17

### Fixed
- `FrontendAuthProvider` set false; remove dead frontend-auth handlers

## 0.3.16

### Fixed
- Panel refresh toast + busy feedback

## 0.3.15

### Fixed
- Normalize OpenAI object `tool_choice` for CodeBuddy upstream
