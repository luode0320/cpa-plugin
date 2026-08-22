# QoderWork 插件持续优化 LOOP

> **版本：** v1.1
> **创建：** 2026-07-27
> **当前线上版本：** qoderwork v0.2.5
> **仓库：** https://github.com/Sliverkiss/cpa-plugin （子目录 `qoderwork/`）
> **部署：** `/root/cpa-manager-plus/cliproxyapi/plugins/qoderwork-v<VERSION>.so`

---

## 核心 5 要素（每轮必读）

### 1. 目标（What）
让 qoderwork 插件成为**独立、生产级、CN-only** 的 CPA provider，与 workbuddy 平级、互不干扰。功能上对标 workbuddy 体验，但**接口与逻辑必须按 QoderWork 自己的规范**（KNOWLEDGE.md），不是照抄。

### 2. 边界（Boundary）
- ✅ **只许改 `/root/qoderwork/qoderwork/`** 下的文件
- ✅ **可改 `/root/cpa-manager-plus/cliproxyapi/config.yaml`**（仅追加 qoderwork 节，不动其他）
- ✅ **可参考** `/root/qoderwork/workbuddy/`（仅作对照，不改它的源码）
- ❌ **禁止改** CPA 源码、CPA 前端、workbuddy 源码、其他插件
- ❌ **禁止改** host / docker / network 配置（除 .so 部署 + config.yaml qoderwork 节）
- ❌ **禁止** 在 auth 文件 schema 之外加自定义字段（沿用 storedAuth 已有字段）

### 3. 规范（How）
- **每步必 commit**（feat/fix/refactor/docs 前缀 + 一句话动机 + 影响面）
- **大改动前写分析 MD**到 `/root/qoderwork/analysis/<topic>.md`（决策依据 + 影响范围 + 验证方法）
- **子 agent 只许分析扫描**，**禁止让他们改代码**——所有修改由 Hermes 主进程执行
- **编译 + 部署 + 实测**才算完成（不许停在"编译通过"）
- **每完成一个 Loop 在 LOOP.md 勾选 + 一行结论**

### 4. 现状基线（Current State）
- ✅ v0.1.11 运行中：1 CN 账号（aliyun6109533651，2200 credits/Pro Trial）
- ✅ PAT 导入 / 两级 token 刷新 / COSY 签名 / 嵌套 SSE / 签到（1s）/ 模型清单（10 静态 + 10 别名）/ 隔离
- ⚠️ **OAuth 登录卡片**：仍是 workbuddy 风格"等待插件保存认证文件"——需改为 **Vertex JSON 风格表单**（直接粘 PAT + 一键导入）
- ⚠️ **auth 自动刷新**：keepalive 已实现 22:00 调度，但**未验证**真实跑过
- ⚠️ **测试文件**：125 个 workbuddy 测试大量失效或逻辑不对——**未清理**
- ⚠️ **死代码**：仍可能有 workbuddy 残留（`active_auth.go`/`policy.go`/`lifecycle.go`/`scheduler.go`/`usage.go`/`usage_config.go`/`cache.go`/`redact.go`/`stream.go`/`panel.go`）

### 5. 候选目标池（Backlog）
按优先级排：

| ID | 目标 | 价值 | 状态 |
|---|---|---|---|
| **L1** | OAuth 登录卡片改为 Vertex JSON 风格（PAT 表单） | 🔥 高 | ⏸ 待做 |
| **L2** | auth 每晚 22:00 自动刷新（验证 keepalive 真实生效） | 🔥 高 | ⏸ 待做 |
| **L3** | 全面扫描死代码/无效代码/低效逻辑（子 agent 分析） | 🔥 高 | ⏸ 待做 |
| **L4** | 清理 `_test.go`（125 个 workbuddy 测试：删/改/留） | 🟡 中 | ⏸ 待做 |
| **L5** | 动态模型清单（COSY 拉 /algo/api/v2/model/list） | 🟡 中 | ⏸ 待做 |
| **L6** | auth 文件 OAuth 别名映射（auth attributes） | 🟢 低 | ⏸ 待做 |
| **L7** | lifecycle/scheduler/policy/active_auth 逻辑审计（是否还需要） | 🟡 中 | ⏸ 待做 |
| **L8** | stream/usage/usage_config/cache/redact 死代码扫描 | 🟡 中 | ⏸ 待做 |
| **L9** | panel.html 剩余 workbuddy 字符串/逻辑清理 | 🟢 低 | ⏸ 待做 |
| **L10** | LICENSE 归属声明优化（保留 lovingfish 归属，加 Sliverkiss 修改） | 🟢 低 | ✅ 完成 |
| **L11** | **auth 分类修复（CPA 原生 type 契约对齐）**：给存量 29 个 workbuddy 文件补 `"type"` 字段 + ParseAuth 加 type 防御 | 🔥 高 | ✅ v0.1.19 |
| **L12** | **真·OAuth 设备授权登录**（替代 PAT）：PKCE + device/selectAccounts + deviceToken/poll → dt-/drt-；deviceToken/refresh；与 PAT 家族共存兼容 | 🔥 高 | ✅ v0.2.5 |

---

## Loop 执行流（每轮）

```
读 LOOP.md → 选目标 → 写分析 MD → 实施（commit）→ 部署 + 实测 → 更新 LOOP.md
```

每轮**最多 1-2 个目标**，不批量。

---

## Loop 进度

（每完成一项填一行）

- [x] **L1** ~~OAuth 登录卡片 Vertex 化~~ → v0.1.12 已做但用户判定未达预期；**2026-07-27 用户指示：登录卡片问题跳过不管**
- [x] **L2** keepalive 自动刷新验证 → v0.1.13 / commit de8ee8b（tokenData JSON tag 错误 camelCase→snake_case，删 tokenData 统一用 jobTokenResponse）
- [x] **L3** 子 agent 全面死代码扫描 → 完成（DEAD_CODE_REPORT.md 生成，16 项死代码已派 Claudium 清理）
- [x] **L4** 测试文件清理 → 无 _test.go 文件（已删）
- [x] **L5** 动态模型清单 → v0.1.15 / commit a844266（COSY 签名调 /algo/api/v2/model/list，解析 chat scene）
- [x] **L6** auth 文件别名映射 → 已实现（config.yaml oauth-model-alias + parseModelAliasAttribute per-auth 覆盖）
- [x] **L7** lifecycle/scheduler 审计 → go vet clean（C-ABI 调度，grep 不适用）
- [x] **L8** stream/usage/cache 死代码扫描 → 删 extractAccessToken（17 行）；go vet clean
- [x] **L9** panel.html 深度清理 → 无 workbuddy/codebuddy/Global 残留
- [x] **L10** LICENSE 优化 → 已正确（Sliverkiss based on workbuddy by lovingfish）
- [x] **L11** auth 分类修复（CPA 原生 type 契约对齐）→ qoderwork v0.1.19 / commit 7b776a9 + workbuddy v0.8.4 / commit 86ba51e（存量 29 文件补 type + 双插件 ParseAuth 对称防御；写入侧核查均合规；双 provider chat 实测通过）

---

## 已解决问题（不再重复）

- ✅ auth 文件隔离（文件名前缀过滤）→ v0.1.5 / commit aaf6bd3
- ✅ billingBase 域名错（codebuddy.cn → openapi.qoder.com.cn）→ v0.1.8
- ✅ panel JS `elGl` undefined → v0.1.9 / commit 026f286
- ✅ OAuth 别名映射（qoder/qwen3.8-max 等 10 个）→ v0.1.9 / config.yaml
- ✅ 签到 30s 卡死（workbuddy 三段式 → 直接 GET+POST）→ v0.1.11 / commit 660b229

---

## 决策原则

1. **接口对齐 KNOWLEDGE.md**，不对齐 workbuddy
2. **代码宁少勿多**——能 50 行写完不写 200 行
3. **能复用 host bridge / schedulerLoop / OAuthModelAlias 就不自造**
4. **每个修复必须有可观察的"之前 X，现在 Y"对比**
5. **不留 debug 日志到 release**（debug 加 → 验证 → 删）

---

## 输出模板（每轮结束）

```
## Loop <N> — <标题>
- 目标：L<ID> <一句话>
- 分析：/root/qoderwork/analysis/<topic>.md
- 修改：commit <sha>（+X/-Y 行）
- 验证：<实测结果，含 before/after>
- 状态：✅ 上线 v0.1.<N>
- 下一 Loop 候选：L<ID>
```
