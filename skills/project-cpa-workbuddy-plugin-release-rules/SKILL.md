---
name: project-cpa-workbuddy-plugin-release-rules
description: 当需要发布 cpa-workbuddy-plugin 仓库任意插件新版本（workbuddy-provider / qoderwork-provider / workbuddy-token-usage）时触发：版本 bump、commit、HTTPS push、dispatch CI、下载 assets、更新 registry、远端验证的一整套发布链路。负责发布执行与门禁；本地逻辑验证用 cgo-plugin-isolated-test（cgo-shim-build.py），本 skill 只引用它，不重复实现。
---

# cpa-workbuddy-plugin 插件发布链路

## Skill 作用与适用场景

- 仓库三插件：`workbuddy-provider`（主，版本 0.12.x）、`qoderwork-provider`（0.2.x，老版独立演进）、`workbuddy-token-usage`（0.1.x）
- 覆盖从「bump 版本」到「远端验证 ALL PASS」的完整发布链路（0.12.0/0.12.1/0.12.2 三次跑通）
- 本地验证（cgo-shim / 单测）职责在 `cgo-plugin-isolated-test`，本 skill 只规定「何时、以什么环境验证」
- 发布前代码逻辑正确性由其他规则保证；本 skill 管「怎么把已验证的代码发出去」

## 自动触发信号

- 用户说「发布」「发版」「升级到 X.Y.Z」「发一个新版本」
- 改动已就绪（fix/feat 已本地验证）需要走完 commit→push→CI→assets→registry 全链
- 新版本号已 bump 但远端 registry 还是旧版，需要补齐剩余链路

## 进入后先做什么

1. **摸清工作树**：`git status --short` 全量看；区分「本次要发布的改动」与「并行/无关改动」（本项目常见并行 40x 重试等未提交改动，**严禁混入发布**）
2. **确认远端基线**：`git log --oneline -5`，记住远端 main 的 HEAD（push 成功后 diff 起点）
3. **确定版本号**：patch 级（bugfix/UX 微调）→ 0.12.x 递增；minor（行为/API 变更）→ 0.13.0。CHANGELOG 顶部是历史 Breaking Change 记录，格式 `## X.Y.Z`
4. **识别混合文件**：`git diff <file>` 检查 main.go / usage_config.go 等是否同时含并行改动 → 决定走「正常发布」还是「分离发布」

## 默认执行流程（12 步，不可跳步）

### Step 1 · bump 版本三处（漏一处 = 面板/产物/registry 不一致）

- `workbuddy/VERSION`（或其他插件目录 VERSION）：`0.12.2` → 新版本
- `workbuddy/main.go`：`var version = "0.12.2"` → 新版本（**注意：main.go 常是混合文件，见 Step 3**）
- `registry.json`：对应插件条目 `"version": "0.12.2"` → 新版本（registry.json 的 `plugins` 是 **list**，用 `id` 匹配条目）

### Step 2 · CHANGELOG 条目

`workbuddy/CHANGELOG.md` 顶部新增 `## X.Y.Z` 章节：Fix/Feat 标题 + 3-5 条要点 + 涉及文件清单。有 Breaking Change 用显式章节（参考 0.12.0 写法）。

### Step 3 · 混合文件分离（关键！）

当 main.go / usage_config.go / stream.go 等同时含并行改动（其他 AI 工具遗留）时，**不能整体 git add**。流程：

```bash
mkdir -p /tmp/cpa-0xxx
# ① 备份：混合文件（tracked 改动）+ 所有并行 untracked 文件
cp workbuddy/main.go workbuddy/usage_config.go workbuddy/accountFailover.go \
   workbuddy/accountFailover_test.go workbuddy/stream.go /tmp/cpa-0xxx/
mv workbuddy/failover_retry.go workbuddy/failover_retry_test.go \
   workbuddy/retry_config.go workbuddy/retry_config_test.go /tmp/cpa-0xxx/
# ② 恢复 HEAD（tracked 的用 checkout，untracked 的已被 mv 走）
git checkout -- workbuddy/main.go workbuddy/usage_config.go workbuddy/accountFailover.go \
   workbuddy/accountFailover_test.go workbuddy/stream.go
# ③ 重做自己的改动（version 行、requestPreserveTick 等）
# ④ 确认工作区只剩自己要发布的文件
git status --short workbuddy/
```

⚠️ **并行改动横跨多文件是整体**：只 checkout 部分文件会让剩余 untracked 测试引用缺失符号 → cgo-shim FAIL（实测：TestRetryOn4xxConfig_LoadsViaConfigure）。untracked 的并行文件必须一起 mv 走。

### Step 4 · 验证（模拟 CI 构建环境）

CI 构建的是 **push 的 commit**（= HEAD + 本次发布的文件），不是工作树。所以分离发布时必须先移走所有并行文件再验证：

```bash
python scripts/cgo-shim-build.py workbuddy   # 必须全绿
```

- 全绿 ≠ 新测试进编译 → 用哨兵法确认（见 cgo-plugin-isolated-test）
- 成功后 shim 目录会被自动删（残留空壳 `rmdir` 逐个删）

### Step 5 · 精确 add + 提交

```bash
git add registry.json workbuddy/VERSION workbuddy/main.go workbuddy/panel.html ...  # 显式列自己文件
git diff --cached --name-only
# 反向确认无并行文件混入：
git diff --cached --name-only | grep -iE "failover|retry|stream|account" || echo "CLEAN"
git commit -m "fix(watchdog): ..."   # 或 feat / chore(release)
```

### Step 6 · 恢复并行改动 + 验证

```bash
cp /tmp/cpa-0xxx/main.go /tmp/cpa-0xxx/usage_config.go ... workbuddy/
mv /tmp/cpa-0xxx/failover_retry.go ... workbuddy/
git status --short | grep -cE "^ M|^\?\?"   # 数量应等于发布前的并行改动条数
git diff workbuddy/main.go workbuddy/usage_config.go | grep -v "^warning:"  # 应只剩并行内容
```

### Step 7 · push（HTTPS + askpass）

```bash
# 写 askpass（用完即删；PAT 在 C:/Users/luode/.github/token）
printf '#!/bin/sh\necho "$(cat /c/Users/luode/.github/token | tr -d '"'"'\\r\\n'"'"')"\n' > "C:/Users/luode/.github/git-askpass.sh"
GIT_TERMINAL_PROMPT=0 GIT_ASKPASS='C:\Users\luode\.github\git-askpass.sh' \
  git -c credential.helper= push https://github.com/luode0320/cpa-workbuddy-plugin.git main
```

⚠️ GIT_ASKPASS 必须 **Windows 风格路径**（`C:\...`），Git 是 Windows 原生程序，`/c/...` 报 cannot spawn。push 成功后 `rm -f "C:/Users/luode/.github/git-askpass.sh"`。

### Step 8 · dispatch CI + 轮询

```python
import json, time, urllib.request, urllib.error, pathlib
TOKEN = pathlib.Path('C:/Users/luode/.github/token').read_text().strip()
API = 'https://api.github.com/repos/luode0320/cpa-workbuddy-plugin'
def call(method, url, body=None, tries=5):
    for i in range(tries):
        try:
            req = urllib.request.Request(url, method=method)
            req.add_header('Authorization', f'Bearer {TOKEN}')
            req.add_header('Accept', 'application/vnd.github+json')
            req.add_header('X-GitHub-Api-Version', '2022-11-28')
            data = json.dumps(body).encode() if body is not None else None
            if data: req.add_header('Content-Type', 'application/json')
            with urllib.request.urlopen(req, data=data, timeout=30) as r:
                return r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()
        except Exception as e:
            print(f'  retry {i+1}: {e}', flush=True); time.sleep(2 ** i)
    return -1, ''
st, body = call('POST', f'{API}/actions/workflows/build.yml/dispatches',
    {'ref':'main', 'inputs':{'plugin':'workbuddy-provider', 'version':'0.12.3'}})
print('dispatch:', st)                     # 期望 204
time.sleep(3)
st, body = call('GET', f'{API}/actions/runs?event=workflow_dispatch&per_page=1')
run = json.loads(body).get('workflow_runs', [{}])[0]
print('run:', run.get('id'), '| head:', (run.get('head_sha') or '')[:7], '| status:', run.get('status'))
# ⚠️ 必须确认 head == 自己刚 push 的 commit（避免触发旧 run）
```

轮询（11 分钟超时，构建一般 3.5-5.5 分钟）：

```python
RUN = 32585681475   # 上一步打印的 run id
deadline = time.time() + 660
while time.time() < deadline:
    run = call('GET', f'{API}/actions/runs/{RUN}')[1]
    run = json.loads(run)
    status, concl = run.get('status'), run.get('conclusion')
    print(f'{time.strftime("%H:%M:%S")} status={status} conclusion={concl}', flush=True)
    if status == 'completed': break
    time.sleep(20)
if status != 'completed': raise SystemExit('TIMEOUT')
assert concl == 'success', 'CI FAILED'
```

### Step 9 · 下载 assets + 校验

```bash
python scripts/download-release-assets.py 0.12.3    # 自带重试+断点缓存+自校验，7 zip checksums 全 OK
```

产物在 `release-assets/workbuddy-provider-0.12.3/`：7 个 zip（darwin amd64/arm64、freebsd amd64、linux amd64/arm64、windows amd64/arm64）+ checksums.txt。

### Step 10 · 提交 assets + push（0.9.7 教训：必须先 push assets 再 publish）

```bash
git add release-assets/workbuddy-provider-0.12.3/
git commit -m "chore(release): add workbuddy-provider 0.12.3 release assets (<描述>)"
# push（同 Step 7 的 askpass 命令）
```

### Step 11 · publish registry + 提交 + push

```bash
python scripts/publish-assets.py workbuddy-provider 0.12.3
git diff registry.json | grep -v "^warning:"   # 确认只改 workbuddy-provider 的 artifacts
git add registry.json
git commit -m "chore(registry): publish workbuddy-provider 0.12.3 (<描述>)"
# push（同 Step 7）
```

### Step 12 · 远端验证 + 清理

```python
import json, urllib.request, hashlib
RAW='https://raw.githubusercontent.com/luode0320/cpa-workbuddy-plugin/main/registry.json'
reg=json.loads(urllib.request.urlopen(RAW, timeout=30).read().decode())
e=next(p for p in reg['plugins'] if p['id']=='workbuddy-provider')
print('version:', e['version'])
arts=e['install']['artifacts']; ok=True
for a in arts:
    d=urllib.request.urlopen(a['url'], timeout=60).read()
    sha=hashlib.sha256(d).hexdigest()
    match=(len(d)==a['size']) and (sha==a['sha256'])
    print(f"  {a['goos']}/{a['goarch']}: size={len(d)}/{a['size']} sha={'OK' if sha==a['sha256'] else 'MISMATCH'}")
    ok=ok and match
raw=json.dumps(reg)
for old in ['0.12.2','0.12.1','0.11.0','0.10.1','0.10.0']:   # 除当前版本外的全部历史版本
    if old in raw: print(f'RESIDUE: {old}'); ok=False
print('FINAL:', 'ALL PASS' if ok else 'FAIL')
```

```bash
rm -f "C:/Users/luode/.github/git-askpass.sh"
rm -rf /tmp/cpa-0xxx cpa-shim-* 2>/dev/null
git log --oneline -5 && git status --short | grep -cE "^ M|^\?\?"   # 并行改动条数应不变
```

## 关键命令速查

| 操作 | 命令 |
|---|---|
| 版本 bump | VERSION + main.go `var version` + registry.json `"version"` 三处 |
| 本地验证 | `python scripts/cgo-shim-build.py <plugin>`（需先移走并行文件模拟 CI） |
| push | `GIT_TERMINAL_PROMPT=0 GIT_ASKPASS='C:\Users\luode\.github\git-askpass.sh' git -c credential.helper= push https://github.com/luode0320/cpa-workbuddy-plugin.git main` |
| 下载资产 | `python scripts/download-release-assets.py <VER>` |
| 发布 registry | `python scripts/publish-assets.py <PLUGIN> <VER>` |
| registry 校验 | `python scripts/validate-registry.py` |

## 踩坑清单（全部实测）

1. **GIT_ASKPASS 路径**：必须 Windows 风格 `C:\...`；`/c/...` 报 cannot spawn（git.exe 不认 POSIX 路径）。askpass 用完即删。
2. **0.9.7 教训**：assets commit **必须 push** 后才能 publish-assets.py——publish 后 registry 指向 raw.githubusercontent 的 URL，不 push 则远端 404。
3. **分离发布**：并行改动横跨多文件是整体；tracked 用 checkout 恢复、untracked 必须 mv 走；验证要在「纯 HEAD+自己文件」环境跑，否则混入并行测试符号导致 FAIL。
4. **版本三处漏一处**：VERSION/main.go/registry.json 任一遗漏 → 面板版本号、构建产物版本、registry 版本不一致。
5. **dispatch 偶发 SSL EOF / exit 35**：api.github.com 瞬时窗口，curl/urllib 都可能失败 → 指数退避重试（tries=5）。
6. **registry.json 结构**：`plugins` 是 **list**（不是 dict），条目用 `id` 匹配；artifacts 在 `install.artifacts`。
7. **零残留**：旧版本号字符串不得出现在 registry.json（历史 artifacts 会被 publish 替换）。
8. **CI 构建的是 push 的 commit**，不是工作树 → 本地验证必须模拟 CI 环境（移走并行文件），否则「本地绿、CI 红」。
9. **dispatch 后核对 run head_sha** == 自己 push 的 commit。
10. **cgo-shim 成功即删 shim 目录**：想跑 `go test -v` 哨兵需手动建持久 shim；残留空壳 `rmdir` 逐个删（rm -rf 可能卡 3 分钟+）。
11. **gofmt CRLF 假象**：历史 tracked 文件 CRLF 时 `gofmt -l` 全量列出；只确认自己新写/新改的文件不在列表。
12. **qoderwork 是老版**：publishUsage 8 参数（无 accountLabel/reasoningEffort）、无 preserve watchdog、无 session_auth——同步改动只能逐函数适配，不能整体覆盖。
13. **Windows rm -rf cpa-shim-\***：go 子进程/杀软持句柄会挂起，`ls -d cpa-shim-*` 确认后 rmdir 逐删。
14. **qoderwork 下载脚本硬编码**：`download-release-assets.py` 的 PLUGIN 是写死的 `workbuddy-provider`，qoderwork 需用等价 python 内联（release API + 下载 + checksums 校验同逻辑）。
15. **验证一律前台跑**：cgo-shim 验证用 `run_in_background` + `| tail` 时 qoderwork 曾挂 23 分钟无输出（后台 bash 环境/残留 shim 目录问题），前台跑 ~6s 秒过；挂起先 TaskStop 再前台重跑，不要死等。
16. **GitHub API 列表缓存延迟**：push/dispatch 后 `actions/runs` 与 `releases` 列表可能仍显示旧条目，须按 `head_sha` / `releases/tags/<tag>` 精确查询为准。
17. **分离发布边界**：仅当「工作树存在与发布内容无关的并行 go 改动」时才需要移走分离；纯规则文件（AGENTS.md/PROJECT_*/skills 等非 go 文件）不参与编译，无需分离，直接验证即可（0.13.0 发布内容即并行批本身，未分离一次通过）。
18. **并行会话竞争（0.13.1 实测）**：发布期间其他 AI 会话可能同时改工作树——盘点后、commit 前、dispatch 前各核对一次 `VERSION` / `main.go version` / `git status` 与上次差异；发现版本号/CHANGELOG 被预写、他人已 commit push 时：a) `git show HEAD:<file>` 验证对方 commit 是否包含你的改动（9854d22 带走我 Edit 的 CHANGELOG 但漏了 panel.html）b) dispatch 前查 `actions/runs?head_sha=<你的commit>`，发现他人 run 竞争同一 tag 时先 `POST /actions/runs/<id>/cancel` 取消对方（产物必须是你的 superset 才可取消），保留自己的 run。
19. **rm askpass 路径必须 POSIX**：`rm -f 'C:\Users\...'` 在 Git Bash 中反斜杠被当相对路径 → safe-delete 拦截报错、文件残留；一律 `rm -f /c/Users/luode/.github/git-askpass.sh`。
20. **registry 版本号预写中间态**：他人可能先改 registry 的 version 字段（如 token-usage 0.1.7→0.1.8）但 release/artifacts 未发——`validate-registry.py` 只校验版本格式不校验 URL 与版本匹配；发布前识别此类中间态并告知用户，勿替用户补发未授权插件。

## 权责边界与不负责事项

- 只负责「发布执行」：版本 bump、提交、push、CI、assets、registry、远端验证
- 不负责代码正确性验证本身（那是 cgo-plugin-isolated-test + 各开发规则）
- 不负责替代用户决策（版本号语义、是否发布）
- 不把并行/无关改动混入发布（用户明确要求另发时除外，须显式列出文件）
- 发布完成后提醒用户重启宿主（c-shared 插件需 CLIProxyAPI 重启才生效）

## 执行通过 / 驳回标准

- 通过：三处版本一致、commit 链清晰（feat/fix → assets → registry）、远端验证 ALL PASS、askpass 已删、并行改动完好
- 通过：发布内容与工作树其他改动严格隔离（staged 列表人工可核）
- 驳回：跳过任一步骤（尤其 push assets 或远端验证）
- 驳回：版本号三处不一致、registry 有旧版本残留、并行改动丢失
