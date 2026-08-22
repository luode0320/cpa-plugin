# cpa-workbuddy-plugin — CLIProxyAPI 插件集合

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Go 插件仓库，将多个 AI 服务封装为 **OpenAI 兼容 provider** 供 CPA 网关统一调度：多账号管理、动态模型、流式推理、每日签到、积分生命周期、token 用量统计。目前包含 **3 个并列插件**，每个插件独立版本、独立发布。

| 插件 | 服务 | 已发布 | 一句话 |
|---|---|---|---|
| [`workbuddy/`](workbuddy/) | 腾讯 CodeBuddy（CN + Global） | v0.9.3 | 原生 OAuth provider，积分生命周期 + 每日签到 + 用量 feed |
| [`qoderwork/`](qoderwork/) | QoderWork CN（qoder.com.cn） | v0.2.6 | 双登录 + COSY 签名推理，逆向产物封装（源码已迭代至 v0.4.x） |
| [`token-usage-tracker/`](token-usage-tracker/) | workbuddy 账户用量 | v0.1.5 | 真实 token 消耗 dashboard，经共享 feed 采集数据 |

---

## 插件总览

### workbuddy — 腾讯 CodeBuddy provider

覆盖国内版 `copilot.tencent.com` 与国际版 `workbuddy.ai`，CN/Global 共用一个插件：

- **OAuth 登录** — 多账号 `workbuddy-<uid>.json` 写入宿主 auth store
- **动态模型** — 上游 models API 实时拉取 + 5 分钟缓存 + 静态兜底；支持宿主侧 `oauth-model-alias` / `oauth-excluded-models` 配置
- **执行器** — OpenAI 兼容 chat completions，流式（真 SSE 走 `host.stream.emit`）与非流式；内置 `tool_choice` 归一、Claude Code 模板清洗
- **积分生命周期** — CN 账号耗尽自动禁用、签到回血自动恢复；Global 账号耗尽删除 auth（一次性 trial）；executor 遇硬积分错误立即触发 reconcile
- **每日签到** — 09:00 / 21:00 定时自动签到，面板可手动批量签；per-account 互斥锁防并发
- **Global trial 领取** — 面板一键领取一次性专家加油包
- **积分面板** — `/v0/resource/plugins/workbuddy/panel`，积分进度条、CN/Global 筛选、账号启停开关
- **调度器（可选）** — `session` 模式按会话轮询多账号，`credits` 选中面板账号
- **用量 feed** — 每条请求的 token 消耗以 NDJSON 追加写入共享 feed，供 token-usage-tracker 消费

### qoderwork — QoderWork CN provider

基于对 QoderWork 桌面客户端的逆向（见 [KNOWLEDGE.md](KNOWLEDGE.md)），纯软件实现其私有鉴权链路：

- **双登录共存** — ① OAuth 设备授权（PKCE，`dt-` 30 天 + `drt-` 1 年自动旋转）② PAT 导入（`pt-`，长期有效兜底），两家族可共存于同一 auth 文件
- **登录自动领包** — OAuth 登录成功后自动判断并领取一次性 Pro 升级包
- **COSY 签名推理** — RSA 包裹 AES 会话密钥 + MD5 请求签名（纯 Go 标准库实现），对接 `gateway.qoder.com.cn` SSE 流式，透传 OpenAI chunk
- **QoderEncoding** — 自定义 base64 字母表 + 三段重排的 body 编码
- **动态模型** — COSY 拉取 `/algo/api/v2/model/list`（chat scene：Qwen / DeepSeek / GLM / Kimi / MiniMax 等），10 静态模型兜底
- **每日签到** — 09:00 / 21:00 定时 + 面板手动（单账号/批量），签到后返回积分快照
- **token 保活** — 22:00 定时刷新，按 token 前缀路由（`drt-` → deviceToken、`jrt-` → jobToken），PAT 永不劫持 OAuth 刷新
- **auth 隔离** — 文件名前缀 `qoderwork-` 过滤，与其他插件互不干扰

### token-usage-tracker — 用量统计 dashboard

记录并可视化 **workbuddy 账户的真实 token 消耗**（实盘数据）：

```
workbuddy 插件                         token-usage-tracker 插件
┌────────────────────────┐   NDJSON   ┌─────────────────────────┐
│ 每次请求完成            │ append ▶  │ 轮询读取（默认 5s）      │
│ publishUsage 汇聚点 ────┼──────────▶│ 导入自身 bbolt 库        │
│ 追加一行到共享 feed      │  O_APPEND │ dashboard（"Token 用量"）│
└────────────────────────┘           └─────────────────────────┘
```

- **共享 feed**：`<CLIProxyAPI root>/data/token-usage-feed.ndjson`，与 workbuddy 的 `usage_feed_path` 一致即可互相发现
- **为什么是文件 feed**：宿主 `UsagePlugin` 广播对插件 executor 恒为空；bbolt 排它锁不允许两个长驻进程共享同一数据库。追加写 NDJSON 是唯一干净的跨插件数据通道（无锁、可回放、可轮转，超 128MB 自动截断）
- **统计核心** 移植自社区插件 [AITNR/cap-token-usage-tracker](https://github.com/AITNR/cap-token-usage-tracker)

---

## 技术亮点

- **QoderWork 逆向**：废弃 WASM 签名与 qodercli 子进程两条弯路后，确认签名是 JS 内重写的 RSA+AES+MD5，PAT → jobToken（`jt-` 24h / `jrt-` 48h）链路端到端实测 200 OK。完整知识库见 [KNOWLEDGE.md](KNOWLEDGE.md)
- **模型路由陷阱**：QoderWork 响应 `"model":"auto"` 是服务端硬编码，不代表路由失败；真实路由靠 `x-model-key` header + `model_config.key` body 字段双管齐下
- **CPA 原生 type 契约对齐**：存量 auth 文件补齐 `type` 字段 + 双插件 `ParseAuth` 对称防御（v0.1.19 / v0.8.4）
- **跨插件数据通道**：文件 feed 而非共享 bbolt，规避文件锁冲突（v0.8.9 拆分决策）

---

## 仓库结构

```text
cpa-workbuddy-plugin/
├── workbuddy/              # 插件 1：腾讯 CodeBuddy provider（含测试与 CHANGELOG）
├── qoderwork/              # 插件 2：QoderWork CN provider（含 baseprompt.json 模板）
├── token-usage-tracker/    # 插件 3：token 用量 dashboard（usage_stats/ 统计核心）
├── registry.json           # 插件商店源（schema v2，direct install，含 sha256/size）
├── release-assets/         # 各版本多架构 zip + checksums.txt 历史产物
├── .github/workflows/build.yml  # 多架构构建 + tag 触发独立版本发布
├── scripts/
│   ├── publish-assets.py       # 扫描 release-assets → 更新 registry.json → 校验
│   ├── validate-registry.py    # registry.json 格式/完整性校验（CI 中执行）
│   ├── cgo-shim-build.py       # 本地 cgo 编译验证 shim（产物 cpa-shim-*/，gitignore）
│   └── qoder_cn_pat_login.py   # QoderWork CN PAT 获取（阿里云 SSO 短信）
├── docs/cpamp-workbuddy-panel.png   # 面板截图
├── analysis/               # 关键决策分析记录（登录方案、拆分合并、升级排障等）
└── *.md                    # KNOWLEDGE / LOOP / plan / STATUS / CLAUDIUM_SPEC / DEAD_CODE_REPORT
```

---

## 多架构 Release

每个插件独立版本发 Release（tag `<id>-v*`），产物为 CPA 插件商店标准格式：

```text
<id>_<version>_linux_amd64.zip      # zip 根目录: <id>.so
<id>_<version>_linux_arm64.zip
<id>_<version>_darwin_amd64.zip     # <id>.dylib
<id>_<version>_darwin_arm64.zip
<id>_<version>_windows_amd64.zip    # <id>.dll
<id>_<version>_windows_arm64.zip
<id>_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`（见 CLIProxyAPI `internal/pluginstore`）。

**CI（GitHub Actions）：**

| 触发 | 行为 |
|---|---|
| push / PR | 全量构建 + `go test` + `go vet` + registry 校验（只出 artifacts，不发 Release） |
| tag `<id>-v*`（如 `qoderwork-v0.2.6`） | 该插件独立版本 Release |
| workflow_dispatch | 手动选插件 + 版本（空 = 插件 VERSION 文件） |

发布流程：改代码 → 编译多平台 zip 放 `release-assets/<id>-<version>/` → `scripts/publish-assets.py` 更新 registry → 提交 → tag 触发 Release。

---

## 安装

### 方式一：插件商店（推荐）

CPA 插件商店添加自定义源：

```text
https://raw.githubusercontent.com/luode0320/cpa-workbuddy-plugin/main/registry.json
```

然后在商店 UI 安装/更新 **workbuddy**、**qoderwork**、**token-usage-tracker**。

### 方式二：手动部署（linux/amd64 示例）

```bash
unzip qoderwork_0.2.6_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp qoderwork.so /path/to/cliproxyapi/plugins/qoderwork.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp qoderwork.so plugins/linux/amd64/
```

### config.yaml

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
    qoderwork:
      enabled: true
    token-usage-tracker:
      enabled: true
```

---

## 本地开发

```bash
# 构建 c-shared 插件（以 qoderwork 为例）
cd qoderwork
CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.version=0.2.6" -o qoderwork.so .

# 测试 + 静态检查（与 CI 一致）
go test ./...
go vet ./...

# registry 校验
python3 scripts/validate-registry.py registry.json
```

Windows 无 C 工具链时可先用 `scripts/cgo-shim-build.py` 生成 shim 验证 C ABI 与打包逻辑（产物在 `cpa-shim-*/`，已 gitignore）。

---

## 文档导航

| 文档 | 内容 |
|---|---|
| [KNOWLEDGE.md](KNOWLEDGE.md) | QoderWork CN 完整逆向知识库（凭证体系 / COSY / QoderEncoding / 模型路由真相） |
| [LOOP.md](LOOP.md) | QoderWork 持续优化循环（Backlog、已解决问题、决策原则） |
| [plan.md](plan.md) | QoderWork 实现计划 v4（认证链路 / Loop 拆分 / 模型清单） |
| [STATUS.md](STATUS.md) | 逆向与对接现状、实测记录、技术决策记录 |
| [CLAUDIUM_SPEC.md](CLAUDIUM_SPEC.md) | QoderWork 插件实施规格（已验证事实，禁止重新逆向） |
| [DEAD_CODE_REPORT.md](DEAD_CODE_REPORT.md) | qoderwork 死代码与 workbuddy 残留分析 |
| [analysis/](analysis/) | 关键决策分析：OAuth 方案、token-tracker 拆分/合并、升级排障等 |

各插件目录内另有独立 README（workbuddy 提供中英双语版）。
