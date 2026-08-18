# Changelog

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
