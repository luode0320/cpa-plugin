# 项目记忆

## 核心记忆

### 仓库与发布

- 仓库：`luode0320/cpa-workbuddy-plugin`（原 cpa-plugin，2026-08-22 改名）；物理目录 F:\cpa-plugin
- 三插件：workbuddy-provider（主，腾讯 CodeBuddy CN+Global）、qoderwork-provider（QoderWork CN）、workbuddy-token-usage（用量 dashboard）
- 发布链路（不可跳步）：bump VERSION+main.go → commit → push main → dispatch CI（plugin=xxx version=yyy）→ 下载 8 assets → **git add assets + push（0.9.7 教训）** → publish-assets.py → commit registry + push → 远端验证 raw URL 200
- git push 必带：`GIT_TERMINAL_PROMPT=0 GIT_ASKPASS='C:\Users\luode\.github\git-askpass.sh' git -c credential.helper= push https://...`（askpass 用完即删）；tag pattern：`workbuddy-provider-v*` 等
- registry.json：`plugins` 是 list，artifacts 在 `install.artifacts`

### 插件架构事实

- **workbuddy 与 qoderwork 并非完全同构**（2026-08-22 实测 HEAD 基线差异：stream.go 224 行 / main.go 420 行 / scheduler.go 123 行）
  - qoderwork 是旧版：publishUsage 8 参数（无 accountLabel/reasoningEffort）、无 preserve watchdog、无 session_auth（无会话粘性）、SSE 用嵌套解包（outer["body"]）、认证走 `applyCosyHeaders`（COSY 签名，endpointChat 常量）+ qoderEncode 编码体
  - workbuddy 是新版：publishUsage 带 reasoningEffort/accountLabel、有 preserve watchdog、session_auth、`backendHeaders`+`endpointChatFor(sa)`、stripDataPrefix SSE
  - **同步改动只能逐函数适配，不能整体覆盖文件**；accountFailover.go 等纯逻辑文件可整文件同步
- 插件是 c-shared（import "C"），Windows 无 gcc 时 main.go 被工具链忽略（undefined: storedAuth 是环境假象），验证一律走 `python scripts/cgo-shim-build.py <plugin>`
- 磁盘写路径：host.auth.save 会丢未知顶层字段 → 直写物理 auth 文件（writeAuthFileDirect + fsnotify）；auth 目录 `~/.antigravity_cockpit/<plugin>_accounts/`
- config_yaml 经 host RPC 传输时 []byte 走 base64；测试必须 `json.Marshal(map{"config_yaml": []byte(yaml)})`

### 关键设计决策

- 数据库/配置一律逻辑引用（无物理外键）
- failover：1/3/10 分钟阶梯退避，429/402/5xx/传输错误计入，业务 400 不计
- 40x 换号重试（2026-08-22）：401/403/404/405 计入账号级故障，`retry_on_4xx` 预算默认 3（0-5），**缺省键保持当前值**（kill switch 安全），400 直通不重试
- 路由（0.12.0 起）：移除三池只留保号池（watchdog 积分阈值自动归池，默认 10m 刷新、阈值 50）；存量 pool/priority 字段忽略式读取
- 版本三轨（qoderwork）：main.go 0.8.2 / VERSION 0.4.1 / registry 0.2.x 历史双轨，发版以 registry 为准
- 跨插件数据通道：NDJSON 文件 feed（token-usage-feed.ndjson，超 128MB 截断），不用共享 bbolt（排它锁冲突）

## 变更记录

- 2026-08-23: 由 `project-rule-file-bootstrap-rules` 的 `memory-bootstrap` 初始化双区骨架；核心记忆由项目分析沉淀
- 2026-07-03: 模板骨架初始化（模板原始记录）

## 机器索引区

```yaml
version: 1
entities: []
relations: []
evidence: []
contexts: []
lifecycle:
  active: []
  deprecated: []
  stale: []
  conflicted: []
  retired: []
retrieval_hints:
  aliases: {}
  scopes: {}
  sources: {}
extensions:
  external_refs: []
  retrieval_provider: ""
  vector_doc_id: ""
  graph_node_id: ""
usage_tracking:
  schema_version: 1
  counted_files:
    - PROJECT_MEMORY.md
```
