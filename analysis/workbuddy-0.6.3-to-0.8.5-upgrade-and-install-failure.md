# WorkBuddy 插件 0.6.3 → 0.8.5 升级对比 与 安装失败根因分析

> 分析日期：2026-08-18
> 涉及代码：
> - `F:\cpa-plugin\workbuddy` — 插件源码（当前 VERSION=0.8.5），对应 GitHub `Sliverkiss/cpa-plugin` / 本地 fork `luode0320/cpa-plugin`
> - `F:\CLIProxyAPI` — 线上 CPA 主程序代码（fork，`v7.2.135-6-g8ca63236`）

---

## 一、代码库关系澄清

| 代码库 | 身份 | 版本 | 说明 |
|---|---|---|---|
| `F:\cpa-plugin\workbuddy` | WorkBuddy 插件源码（clean-room 重构版） | **0.8.5** | 49 个文件：17 个单职责 Go 文件 + 15 个测试 + 文档 |
| `Sliverkiss/cpa-plugin` | 插件发布仓库（上游） | workbuddy **0.8.5** release 已发布 | 0.6.x 早期 release 用 `v0.6.x` tag；0.8.x 改用 `workbuddy-v0.8.x` tag |
| `luode0320/cpa-plugin` | 本地 fork（remote origin） | registry.json 写 0.8.5 | **没有任何 release**（404） |
| `F:\CLIProxyAPI` | 线上 CPA 主程序 | **v7.2.135** | 用户 fork，Docker 部署（debian:bookworm 运行镜像） |

关键事实：**0.8.5 的 release 资产本身是完整、有效、可下载的**（2026-07-26 发布，10 个资产，含 checksums.txt；本次实测下载 linux/amd64 zip，SHA256 校验通过，解压出合法 ELF64 x86-64 `workbuddy.so` 7.7MB）。问题不在构建产物，而在"商店无法解析这个 release"。

---

## 二、0.6.3 → 0.8.5 的改进与升级

### 2.1 规模变化（git diff fe9355f(0.6.3) → HEAD(0.8.5)）

| 指标 | 0.6.3 | 0.8.5 | 变化 |
|---|---|---|---|
| 文件数 | 13 | 49 | +36 |
| 代码净增 | — | — | **+8011 / -3499 行** |
| main.go | 超长主档 | 2940 → 809 行 | 拆分为 17 个单职责文件 |
| management.go | 超长主档 | 2263 → 349 行 | 同上 |
| 测试 | 少量 | 115 个（`go test -race` 全过） | 每个模块独立测试 |
| 文档 | 无 | README / README_CN / docs/* | 社区级 |

### 2.2 按主题分类的改进清单

#### 🔒 安全（0.6.31 集中爆发修复，最高优先级）
- **UID 路径穿越修复**：导入凭证文件名 `sanitizeUIDForFileName` 白名单，杜绝 `../` 写入任意路径
- **refresh_token 不再泄露**：chat 请求头移除 `X-Refresh-Token`（长期凭证只在 refresh 端点使用）
- **插件层 management 鉴权 + 限流**：constant-time Bearer 比对（crypto/subtle）+ per-IP token-bucket（`management_key`）
- **panel.html XSS 修复**：`onclick="...('${idx}')"` 字符串注入 → `data-action` + `addEventListener`
- **panel.html CSRF 缓解**：`fetch` 显式 `credentials:'omit'`
- **redactSecrets 裸 JWT 兜底**：无 `Bearer` 前缀的 `eyJ…` 也会被脱敏

#### 🐛 稳定性 / Bug 修复（0.6.28~0.6.31 为主）
- **选中账号与实际路由不一致（P0 级）**：activeAuthID 存 auth.Index（SHA256）而 scheduler 用 auth.ID（UUID），永不匹配 → 全链路统一为 auth.ID
- **签到后按钮不更新 / 套餐标记丢失**：cache 用 merge 语义替代 delete
- **数据竞争**：`invalidateAccountCredits` 值拷贝（`fresh := *e; Store(&fresh)`）
- **configure 嵌套锁死锁风险**：两阶段"无锁解析 + 分锁写入"
- **`out[:0]` 共享底层数组** → `make([]byte, 0, len)`
- **pumpUpstreamStream 无 context**：客户端断开后 goroutine 泄漏至 120s 超时 → `NewRequestWithContext` + cancel
- **`scheduler_mode: off` 配置断链**：configure 解析了但 handler 不读取 → 修复后 off 正确 defer 内置 scheduler
- **Global 账号聊天 401/400**：JWT iss=workbuddy.ai 必须走 www.workbuddy.ai 端点（0.6.14）
- **流式强制 stream:true**：上游不再支持非流式（0.6.17）

#### ⚡ 性能
- **热路径 JSON 序列化 4 次 → 1 次**：`prepareUpstreamBody` 统一 4 个 mutator（每次 chat 省 4-5 次 JSON 往返）
- **批量签到串行 → 并发**（sem=4），29 账号从 3N 串行 HTTP 降到并发 4 路
- **`cachedAccountDetails` singleflight**：并发 dashboard/reconcile 对同一账号只打 1 次上游
- **冒泡排序 → sort.Slice**（O(n²) → O(n log n)）
- **积分懒加载并发化**（0.6.21）

#### 🏗️ 合规改造（0.7.0，架构级）
- **所有上游 HTTP 调用从 `sharedHTTPClient` 切到宿主桥 `host.http.do` / `host.http.do_stream`**：
  - models / billing / usage 上报 / chat completions（流式+非流式）全部走宿主 RPC
  - 宿主 request-log 现在能捕获插件出站流量（之前完全不可见）
  - 宿主 transport policy（proxy/超时/连接池）对插件生效
  - 这是 CPA 官方 pluginapi 的设计意图，对齐 `sdk/pluginapi`
- **`UsagePlugin` 能力声明 + `handleUsage` RPC handler**：宿主每次请求完成推送 UsageRecord，插件转发 CPAMP，幂等去重
- **`hostStreamReader` 适配层**：宿主 32KB 任意字节块 → io.Reader，SSE 行切分透明迁移

#### ✨ 功能增强
- **面板**：CN/Global 筛选标签、骨架卡懒加载、单卡刷新、夜间模式主题自适应、用量汇总卡、plan 徽章、导入弹窗、选中卡路由（0.6.9~0.6.21 系列）
- **keepalive.go（0.8.1 新增）**：每日 22:00 access-token 刷新，防 Keycloak offline-session 过期
- **0.8.3~0.8.5**：hostAuthList 按文件名前缀过滤；对称 ParseAuth 所有权守卫；**手动签到链重写**——单账号直连路径（1 RPC + 2 HTTP ≈ 1-2s，替代原 4 RPC + 5 HTTP×重试 ≈ 10-30s），消除 30s context canceled 超时

#### 📦 工程化（0.8.0）
- 超大主档拆分为单一职责文件（对齐 CPA 原生插件案例"一个能力一个文件"）
- gofmt / go vet / gocritic / staticcheck 全绿
- README / README_CN / LICENSE(MIT) / Makefile / .gitignore / docs（architecture / development / definition-of-done）
- 双插件独立版本发布 CI（tag `workbuddy-v*` / `qoderwork-v*`）

### 2.3 升级价值小结

0.6.3 → 0.8.5 是**质变**：从"能用"到"生产级"。安全性（路径穿越、token 泄露、XSS/CSRF）和正确性（选中/路由漂移、缓存竞争）的修复直接影响线上账号安全与计费准确性；0.7.0 的宿主桥改造让插件流量进入宿主审计视野（合规关键）；0.8.0 的重构让插件可维护、可测试、可交接。

---

## 三、为什么 0.8.5 无法安装 —— 根因

### 3.1 现象

线上 0.6.3 一直正常；0.8.5（及 0.8.0~0.8.4、qoderwork 0.2.6）从 CPA 插件商店安装失败。

### 3.2 根因（确定）：release tag 格式与商店解析器不兼容

CPA 插件商店安装链路（`internal/pluginstore/`，宿主 v7.2.135）：

```
Client.Install / InstallVersion
  → FetchLatestRelease  GET api.github.com/repos/{owner}/{repo}/releases/latest
  → ReleaseVersion(release)   ← 在这里失败
      normalizeVersion:   只剥离 1 个前导 "v"/"V"
      validPluginVersion: 正则 ^[0-9][0-9A-Za-z.+-]*$ 要求【首字符必须是数字】
  → "invalid release tag ..."  → 安装失败
```

**Release 命名历史变化**：

| 版本 | release tag | ReleaseVersion 解析结果 | 商店能否安装 |
|---|---|---|---|
| 0.6.3 | `v0.6.3` | `0.6.3` ✅ valid | ✅ 能装（线上现状） |
| 0.6.29/0.6.30 | `v0.6.29` / `v0.6.30` | ✅ valid | ✅ 能装 |
| **0.8.5** | **`workbuddy-v0.8.5`** | normalize 后仍是 `workbuddy-v0.8.5`，首字符 'w' | ❌ **invalid release tag，装不上** |
| qoderwork 0.2.6 | `qoderwork-v0.2.6` | 同样 invalid | ❌ 同样装不上 |

> CI 在 0.7/0.8 时期为支持"双插件独立版本"把 tag 从 `v*` 改成了 `<plugin>-v*`（见 `.github/workflows/build.yml` 的 `tags: ["workbuddy-v*", "qoderwork-v*"]`），但 **CPA 插件的 `ReleaseVersion()` 解析器从未支持该前缀格式** —— 发布端和消费端契约不匹配。

### 3.3 实测证据

1. 用 CPA 真实逻辑（`normalizeVersion` + `validPluginVersion` + 真实正则）对 7 个 tag 跑测试：
   ```
   tag=v0.6.3            -> 0.6.3           valid=true
   tag=v0.6.30           -> 0.6.30          valid=true
   tag=workbuddy-v0.8.5  -> workbuddy-v0.8.5 valid=false   ← 失败
   tag=qoderwork-v0.2.6  -> qoderwork-v0.2.6 valid=false   ← 失败
   ```
2. GitHub API 实测：`Sliverkiss/cpa-plugin` 有 `v0.6.3` 和 `workbuddy-v0.8.5` release；`luode0320/cpa-plugin` **无任何 release**（若商店源配成自己的 fork，则是 404 报错，同样装不上）。
3. 0.8.5 release 资产实测：`workbuddy_0.8.5_linux_amd64.zip` 下载成功、SHA256 与 checksums.txt 匹配、zip 根目录就是 `workbuddy.so`（合法 ELF64 x86-64，Go 1.26 构建）—— **资产本身没有任何问题**。

### 3.4 次要因素 / 需排除项

| 因素 | 状态 | 说明 |
|---|---|---|
| 商店源指向自己的 fork | ⚠️ 需确认 | 若线上配置源是 `luode0320/cpa-plugin` → 404 release not found |
| 插件 SDK 版本（v7.2.30 vs 宿主 v7.2.135） | ✅ 不影响 | 0.6.3 与 0.8.5 go.mod 都 require v7.2.30；ABI=1 不变，宿主对老 schema 向后兼容 |
| Go 版本（1.26） | ✅ 匹配 | CI 与 Dockerfile 均 go 1.26 |
| 运行环境 glibc | ⚠️ 低风险 | CI 构建于 ubuntu-24.04(glibc 2.39)，运行镜像 debian:bookworm(2.36)；Go c-shared 对 glibc 依赖极少，但若报 `GLIBC_* not found` 即此问题 |
| 手动安装时平台错误 | ⚠️ 需注意 | Windows 开发机上 `go build -buildmode=c-shared` 产出的是 `.dll`；交叉编译 Linux .so 需 CGO 交叉工具链 |
| 0.8.x 早期 docker SIGSEGV | ✅ 已修复 | 0.8.5 `cliproxyPluginShutdown` 已改为 no-op（见代码注释，0.8.2 之前 docker 重启见过 SIGSEGV） |

---

## 四、解决方案（按优先级）

### 方案 A：补发一个 `v0.8.5` tag 的 release（最快，10 分钟）
在发布仓库打一个纯版本号 tag 并关联现有资产（或复用 CI 资产重发）：
```
git tag v0.8.5 <commit> && git push origin v0.8.5
# 用 GitHub UI / gh 创建 release，attach 现有 7 个 zip + checksums.txt
```
之后商店 `FetchLatestRelease` 取到 `v0.8.5` → 解析 `0.8.5` → 安装成功。
⚠️ 注意：`releases/latest` 只会指向最新的 release，所以需要确保 `v0.8.5` 的 created_at 是最新的（或临时把 `workbuddy-v0.8.5` 的 latest 状态让给 `v0.8.5`）。

### 方案 B：手动安装（已验证资产有效）
```bash
curl -LO https://github.com/Sliverkiss/cpa-plugin/releases/download/workbuddy-v0.8.5/workbuddy_0.8.5_linux_amd64.zip
unzip workbuddy_0.8.5_linux_amd64.zip   # 得到 workbuddy.so
cp workbuddy.so /path/to/cliproxyapi/plugins/workbuddy.so   # 或 plugins/linux/amd64/
# config.yaml 启用:
#   plugins: { enabled: true, dir: plugins, configs: { workbuddy: { enabled: true } } }
```

### 方案 C：registry 改 direct 安装类型（绕过 GitHub release 解析）
在 `registry.json` 的 workbuddy 条目加 `install.type: direct` + `artifacts`（带 URL + SHA256），商店走 `InstallDirect` 路径，不经过 `ReleaseVersion`。

### 方案 D：修 CPA 解析器（长期，若 fork 常驻）
在 `F:\CLIProxyAPI\internal\pluginstore\version.go` 的 `normalizeVersion` 增加 `<plugin>-v` 前缀剥离（如 `strings.TrimPrefix` 到最后一个 `-v`），并同步 `ReleaseVersion` 校验。改完需重新构建线上 CPA 镜像。

### 排查建议（若现象不同）
先看 CPA 日志中插件商店请求的实际报错：
- `invalid release tag "workbuddy-v0.8.5"` → 命中本报告根因（方案 A/D）
- `404` / `release asset ... not found` → 源指向无 release 的 fork（换源或方案 B）
- `GLIBC_2.xx not found` / `cannot open shared object` → 运行环境 glibc 太旧（方案 B 时在相同 base 镜像内构建插件）
- `plugin ABI version ... not supported` → 宿主过旧（需 v7.2.x+）

---

## 五、结论

1. **0.6.3 → 0.8.5 是值得升级的**：安全、稳定性、性能、合规、工程化五个维度全面质变，且 0.7.0 起流量走宿主桥，是 CPA 官方合规路径。
2. **0.8.5 装不上的根因是 release tag 命名契约不匹配**：`workbuddy-v0.8.5` 前缀格式无法被 CPA 插件商店 `ReleaseVersion()` 解析（要求首字符为数字的 `vX.Y.Z`），与插件代码、构建产物、Go/SDK 版本均无关。
3. **最快修复**：补发 `v0.8.5` 格式的 release 或手动解压放置（资产已实测有效）；长期建议同步修复 CPA 解析器以支持 `<plugin>-v` 前缀。
