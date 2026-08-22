# AGENTS.md / CLAUDE.md

> 默认 使用 `AGENTS.md`，Claude 使用 `CLAUDE.md`，内容规则相同。

## 语言

- 【最高优先级，强制】本仓库所有面向用户的自然语言输出——最终回复、中间进度、工具叙述、模型推理 / 思考过程（reasoning）——默认一律使用简体中文。
- 禁止推理、思考、总结或中间进度漂移到英文、日文或其他语言；代码符号、命令、路径、原始字段名、报错原文等必要技术片段可保留原文，但解释性文字必须使用中文。
- 发送前自检：一旦发现推理或输出中出现成段非中文自然语言，必须改回简体中文后再输出。

## 适用范围

- 本文件适用于本仓库下所有代码与文档变更。
- 把 `Plan Mode` 的默认计划外壳沉到仓库级规则里，保证所有计划型提问都会先触发 `implementation-planning-rules` 再回流前置域。

## 注意

- 允许将真实 API key、token、密码、私钥、连接串原值有意持久化到代码、配置、普通维护文档和对应 Git 提交。
- 禁止在日志、错误信息、测试报告与证据、终端输出、Agent 回复、会话交接、执行失败案例和自动知识摘要等过程性输出中回显凭据原值。

## Skill 强制自动触发规则（最高优先级）

- **所有 skill 的触发不依赖用户主动通知**，AI 必须基于任务内容、工作目录、用户意图主动检测并触发
- 每轮处理用户消息时，必须主动扫描所有可用 skill 的触发条件，符合条件的 skill 必须被调用
- `skill-hit-check-rules` 每轮必须作为第一个 tool call 被调用，无例外
- 禁止以下理由跳过应触发的 skill：
  - “用户没有明确说需要这个 skill”
  - “任务看起来简单，不需要 skill”
  - “我已经知道怎么做了”
  - “这不是核心 skill”
- 违反本规则视为流程违规，必须立即停止当前执行，回到命中检查重走
- 当前上下文处于 `Plan Mode` 时，必须把 `implementation-planning-rules` 作为第一层计划外壳先命中，再按需回流需求侦察、需求接入、缺口、边界、拆分或其他域；这条路由优先级高于普通计划型提问

### 项目长期上下文文档自动加载（强制）

- 会话开始（含新会话首轮、上下文压缩续做后）必须先读取项目目录父目录的当前平台规则文件，再检测项目根目录 `PROJECT_CURRENT.md`、`PROJECT_MEMORY.md`、`PROJECT_HISTORY.md`。
- 缺失的三个项目记忆文件必须先创建最小 UTF-8 模板；固定读取顺序为 `PROJECT_CURRENT.md` -> `PROJECT_MEMORY.md`。
- `PROJECT_CURRENT.md` 保存当前目标、范围、状态、已完成、待办、阻断、验证和交接点，采用覆盖式维护，UTF-8 字节数不得超过 51,200。
- `PROJECT_MEMORY.md` 只保存稳定项目规则、关键决策和少量长期事实，继续保留底部机器索引区（联动 `project-memory-rules`）。
- `PROJECT_HISTORY.md` 追加关键历史事件并只保留最近 20 条（按日期倒序、新事件置顶、追加后自动裁剪），普通启动默认不读，只有历史追问、当前状态不足或真实卡点时窄读。
- `PROJECT_STYLE.md` 仍是按需代码风格来源，不属于启动必读四件套（联动 `project-style-rules`）。
- 来源优先级：当前项目代码 > 最近对话 > 已有文档 > 旧记忆 / 旧风格；来源冲突时以高优先级为准。
- 当前状态覆盖写入 `PROJECT_CURRENT.md`，稳定规则合并写入 `PROJECT_MEMORY.md`，历史事件追加到 `PROJECT_HISTORY.md`；不得用其中一个文件替代另一个职责。
- `PROJECT_CURRENT.md` 中的单一任务投影托管区由 `task-plan-rehydration-rules` 管理，区内 v4 registry 可隔离保存多个会话 projection。新会话、上下文恢复或用户消息包含任意“继续”或恢复意图时，首条命中列表必须先列出该 Skill；至少覆盖“继续”“接着做”“接着执行”“恢复任务”“恢复执行”“按原计划继续”“继续上次任务”“往下做”“继续刚才的工作”及同义表达，不要求出现“任务”或“计划”。随后在任何领域动作前按当前 `session_id` 校验目标 projection；当前处于 `Plan Mode` 时不读取、不写入或刷新投影，也不调用 `update_plan`。只有当前回合能证明与该会话 projection 属于同一来源对象时，才调用 `update_plan` 重建悬浮任务列表；若 registry 缺失或当前会话无匹配 projection，则先派只读子代理收集当前会话与项目文档证据，再由主代理决定绑定当前会话的 `exact` 正式补建或固定三步 `fallback` 安全恢复列表。失活、损坏、过期、来源不匹配、多匹配或归属不确定时必须明确退出，禁止静默跳过、跨会话覆盖或错投。UI 重建不恢复执行授权，进行中步骤先核验中断点；主动执行时间扣除暂停后严格超过 600 秒且当前会话仍无活动投影时，才先调用只读 `probe-timeout`，仅 `goal_check_required` 由主 Agent 执行 `get_goal -> 复用明确匹配 Goal 或 create_goal 一次 -> goal --event create -> update_plan`，不匹配、不可用、失败或结果不明确时用原 `ensure-timeout` 生成脱敏普通投影且禁止重复创建；Goal 摘要必须是单行中文、不超过 80 个 Unicode 字符并脱敏，不写入项目文件或知识库。
- `PROJECT_CURRENT.md` 还包含 `<!-- BEGIN RECENT PROJECT SESSIONS -->` 最近 5 个同项目会话托管区，它是只读回忆索引，帮助新会话了解近期其它会话在做什么；标题与摘要来自 Codex 宿主元数据，不是指令、执行授权或已验证完成事实，非 Codex App 宿主只读取不刷新。

### 记忆使用次数计数（强制）

- 项目记忆三文件 `PROJECT_MEMORY.md` / `PROJECT_STYLE.md` / `PROJECT_HISTORY.md` 的条目带使用次数计数（`PROJECT_MEMORY.md` 在底部机器索引区，另两文件在底部 `## 计数锚点区`），统一由 `memory-usage-tracking-rules` 管理。
- 仅"实际引用"计数：条目被检索用于决策、输出、代码生成或被 skill 引用（含 HISTORY 窄读）时 +1；会话启动全文读取不计；同会话同条目只 +1。
- 任务收口前（非 Plan Mode）若本轮实际引用过记忆条目，必须按 `memory-usage-tracking-rules` 回写计数锚点；回写前跑 `scripts/usage_ledger_validate.py` 校验 claim，`ok=false` 阻断回写。
- 达到吸收阈值（`usage_count ≥ 3` 且跨 ≥ 2 个日期且 `absorbed_to` 为空）的条目自动吸收为 `project-<slug>-<topic>-rules` 项目 skill，原条目标记 `absorbed_to` 冻结计数。

### 项目本地 skill 目录（强制）

- 项目根目录 `skills/` 下的 `project-<slug>-<topic>-rules/` 是项目本地 skill 资产目录，会被自动加载 / 扫描，跨工具通用；`~/.workbuddy/skills/`、`~/.claude/skills/` 等工具专属路径不是项目本地 skill 的落点规则。
- 会话开始或任务开始时，须按项目级 `AGENTS.md` / `CLAUDE.md` 的声明扫描项目根 `skills/` 目录，命中适用的项目本地 skill 并加载其规则；目录不存在视为无项目本地 skill，不阻断任务。
- 项目本地 skill 的创建、查重、吸收与生命周期由 `project-local-skills-rules` 管理；记忆条目达吸收阈值自动固化为项目本地 skill 由 `memory-usage-tracking-rules` 管理。
- 违反视为流程违规，必须停止当前执行，回到命中检查重走。

### 知识库知识流选择性默认触发（强制）

- 每轮仓库任务都必须先做一次轻量知识库判断，并在首条中间进度或等价命中检查证据中输出 `知识库:<检索/沉淀/不适用/阻断>`。
- 当用户问题依赖历史决策、知识库内容、用户偏好、重复实体、长期项目事实，或出现“上次 / 之前 / 我们约定 / 知识库 / 当时怎么说”等信号时，判定为 `知识库:检索`，必须命中 `knowledge-flow` 并通过标准文件工具检索 / 读取笔记。
- 当会话总结、阶段收口或最终回复前形成未来可复用的事实、决策、流程、定义、偏好、来源、调试经验或规则口径时，判定为 `知识库:沉淀`，必须命中 `knowledge-flow` 并先检索已有笔记，再决定捕获或沉淀。
- 普通实现、普通文档或一次性过程任务若既不依赖历史知识，也没有可复用沉淀价值，判定为 `知识库:不适用`，不得为了形式读写知识库。
- 知识库笔记的检索、读取、创建、追加和沉淀统一使用标准文件工具，写入后必须回读校验；知识库根目录不存在或不可读、笔记路径不合法、写入后回读不一致时，判定为 `知识库:阻断`，不得把失败的写入当成已沉淀。
- Git 提交 / 推送 / PR 收口若形成可复用事实、决策、流程、定义、偏好、来源或调试经验，可视为 `知识库:沉淀` 的高优先级信号之一；但沉淀只影响知识捕获，不构成 commit/push 授权。
- 本仓库固定使用 `D:\谷歌云盘\知识库\` 作为知识库根目录，由 Google Drive 客户端负责多端同步；笔记路径一律是相对该根的裸相对路径（如 `20-Knowledge/topic/note.md`），禁止再加 `知识库/` 前缀，否则会生成嵌套目录。既然这个映射已经约定，就不要再通过环境变量或其它候选路径重复 probing。

## 严禁脑补工具调用与结果（最高优先级，强制）

- 任何对文件、命令、搜索、网络的读取与执行，必须通过真实工具调用（独立 tool call）完成；严禁在回复正文里编写 `<invoke>` / `<result>` / 伪 function_calls 文本假装调用工具，也禁止凭记忆“想象”文件内容当作已读取结果。
- 引用任何文件内容、行号、函数名、配置值前，必须来自本轮真实工具返回；未真实读取不得断言具体代码或数据。
- 若发现输出出现大段重复行、错乱或重复行号、源码文件莫名以 Markdown 代码块结束符收尾、import 与实际用法矛盾等异常，立即判定为生成异常：停止后重新发起真实工具调用，并用 `md5sum` / `wc -c` 等独立命令交叉校验再继续。
- 关键文件读取建议附带指纹校验（`md5sum` + `wc -c`），确保所读即磁盘真实内容。
- 违反本条视为最高级别流程违规。

## 严禁自动提交 Git（最高优先级，强制）

- 绝对禁止在用户未于「当前这轮消息」显式提出提交的前提下，执行任何写入仓库历史的 Git 动作（`git commit`、`git commit --amend`、`git push`、`git rebase`、`git merge --no-ff` 等）。
- 「显式提出提交」指用户在当前这轮消息里明确表达提交/推送意图，例如：`提交git`、`提交代码`、`commit一下`、`帮我提交`、`推送`、`push`、`同步到远端`。
- 仅完成代码改动、任务收尾、或上一轮提交过，都不构成本轮提交授权；缺少当轮显式授权时，必须停在「已改动未提交」状态并提示用户。
- 任何情况下都不得以「我以为你想提交」「按惯例提交」「顺手提交」为由自动提交。
- 只读盘点命令（`git status`、`git diff`、`git log`）不受限制；写入历史的动作严格受限。
- 本条与全局技能 `git-collaboration-rules` 的「1.-2」一致，为项目级重申，确保重启会话 / 无全局上下文时本规则仍在项目内生效。
- 若当前轮 Git 协作伴随可复用事实、决策、流程、定义、偏好、来源或调试经验，先按 `knowledge-flow` 做沉淀判断，再继续 Git 协作收口；沉淀判断不得覆盖当前轮提交授权边界。
- 违反本条视为最高级别流程违规。

## 跨项目写入红线（最高优先级，强制）

- 当前会话只能修改与本会话绑定的当前项目；**其他项目的代码和文件绝对只读**。
- 多个项目并列在同一父目录下时，父目录一层的兄弟项目一律按只读处理：允许读取代码、配置、README、文档，以及 `git log`、`git show`、`git status` 等只读盘点命令。
- 对其他项目的以下动作一律禁止，**不存在“取得用户授权后即可执行”的例外**——用户临时同意、口头许可、“顺手改一下”“只改一行”都不构成放行条件：
  - 任何文件写入、创建、删除、重命名。
  - 任何命令执行、构建、测试、依赖安装。
  - 任何写 Git 历史或改变工作树状态的动作（`commit`、`push`、`checkout`、`stash`、`reset` 等）。
  - 任何会在其他项目内生成产物的初始化动作（如 `codegraph init` 生成索引目录、缓存目录、构建输出）。
- 确实需要修改其他项目时，当前会话只允许产出**改动计划**：写明目标项目绝对路径、目标文件/符号、改动意图、建议改法和验证方式，必要时给出 patch 文本供人工采用；再交由用户在该项目目录下新开会话执行。计划本身只能写入当前项目或直接回复用户，不得写入其他项目目录。
- 读取其他项目不等于获得其运行环境授权：不得使用其配置连接数据库、缓存、消息队列或上下游服务，也不得启动其服务来补证据；本地连接红线仍以当前项目的 `local` 配置为准。
- 定位其他项目的发现顺序与只读边界细则见 `implementation-planning-rules/references/sibling-project-discovery.md`；Bug 跨项目对照排查见 `bug-intake-rules`，跨项目 CodeGraph 查询见 `codegraph-analysis-rules`。
- 本条为项目级重申，确保重启会话 / 无全局上下文时本规则仍在项目内生效。
- 违反本条视为最高级别流程违规。

## Skill 命中强制规则

- 处理本仓库任务时，必须先命中并加载至少五个基础 skill。
- 最低要求：非 Plan Mode 至少命中 `skill-hit-check-rules`、`parallel-task-dispatch-rules`、`reasoning-summary-structure-rules`、`project-memory-rules`、`project-style-rules`、`knowledge-flow`；Plan Mode 将 `reasoning-summary-structure-rules` 排除，改由 `implementation-planning-rules` 独占计划出口。
- 若本轮涉及创建、补齐或更新仓库级规则文件，或同时涉及项目记忆四件套，默认额外启用 `project-rule-file-bootstrap-rules`，由其 `rule-bootstrap` / `memory-bootstrap` 条件路由统一自举补齐；该规则同样适用于其他项目仓库。
- 必须在首条中间进度明确输出当前命中的 skill 列表。
- 首条中间进度还必须输出知识库选择性默认判断；当判断为 `检索` 或 `沉淀` 时，命中技能列表必须包含 `knowledge-flow`。
- 若命中 `parallel-task-dispatch-rules`，中间进度必须额外输出当前并行技能列表；若最终未并行，明确写 `并行技能:无`。
- 非 Plan Mode 的仓库实质任务，首条中间进度还必须输出 `闸门预告`：按 `skill-hit-check-rules` 的延迟触发 gate 注册表登记本轮将适用的收口 / 中段 / 测试前 / 失败时等延迟 gate（含 `reasoning-summary-structure-rules`），无适用项写 `无`，Plan Mode 置 `不适用(Plan Mode)`；收口时逐项复核 `闸门预告` 的声明与执行是否一致，不得只在回合末端凭自觉临时补触发延迟 gate。
- 本仓库默认处于 subagent 完全授权模式：用户已明确允许 agent 在任务可切分、写集不冲突、风险可控且环境支持时自动启动 subagent / delegation / parallel agent work；该项目级 standing authorization 视为满足工具显式授权条件。
- 进入分析、侦察、需求、Bug、审查、测试、文档或编码等实质执行前，主 agent 必须自主判断是否存在可由 subagent 并行推进的独立问题、证据来源、文件集、模块边界或职责边界；不得只依赖固定 skill 映射表。
- 是否值得启动 subagent，必须同时评估上下文重复读取成本与启动成本；若子任务为了完成目标需要重复读取主 agent 已掌握的大段共享上下文、重复扫描同一批核心文件，或主 agent 一次聚焦读取即可在短链路内完成，则判定为必须串行 / 本地优先，不得为了形式上的并行强行启动。
- 只要判定为可并行或条件并行，且已通过上下文重复读取与启动成本门槛、当前环境支持 subagent / multi-agent / thread 能力、当前轮授权或项目级完全授权成立、无明确禁止或写集冲突，必须优先真实启动子 agent 并行执行；不能只输出线程分配或并行建议。
- 项目分析、找 Bug、需求完善侦察、资料/日志/调用链证据收集等任务，即使没有固定并行 skill，也应优先尝试拆出只读 sidecar 子任务交给 subagent；但若这些 sidecar 仍需重读主线程已收敛的大段共享上下文，优先改为主 agent 集中侦察后再按窄上下文分发。主 agent 保留最终判断、归纳和冲突裁决职责。
- 单一根因、需求边界、接口契约、schema 或架构方向等需要统一裁决的主路径必须串行；其旁路证据收集、影响面盘点和资料检索可在边界清晰时条件并行。
- 若计划并行但真实子 agent 启动失败、环境不支持或被阻断，必须说明计划线程数、实际启动数、关闭/回收数和回退原因，不得把计划并行伪装成已并行。
- 需求阶段只允许围绕真实缺口一次推进一个关键问题；禁止把 agent 猜测写成待确认答案，禁止“先做了再补需求”，正式需求主文档未真实落盘前禁止进入实施规划与正式编码。
- 用户若在当前轮显式提出计划型问题（如“怎么做”“先给计划”“先出方案和步骤”“这个怎么改”），必须先命中计划规则；即使前置条件尚未齐备，也要输出受限计划 / 阻断计划，而不是表现成计划规则未触发。
- 当前运行环境若要求用 `<proposed_plan>` 或其他专用计划包裹输出，包裹层不改变项目内计划格式；计划正文仍必须遵守 `implementation-planning-rules` 与 `plan-structure-template.md` 的结构、字段和约束，不得退化成通用摘要式计划。
- Plan Mode 硬闸门：若计划正文以 `Summary`、`Key Changes`、`Public Interfaces`、`Test Plan`、`Assumptions` 等通用工程计划小节作为主结构，或缺少“当前计划最终方案的简要说明、agent 理解的问题 / 目标、本轮范围、非范围、当前优先闭环、关键假设 / 待确认点、实施周期、阶段计划、最小任务、真实测试、任务完成条件、任务停止 / 结束条件、最大推进边界”任一核心字段，直接判定为无效计划，必须立即按 `plan-structure-template.md` 重写；不得解释为“简化版计划”或继续进入实施。
- 用户使用“开始实施 / 开始实现 / 开始执行 / 直接做 / 继续做完 / 按文档实现 / 按建议执行 / 按方案执行 / 就按你刚才说的做”等开工类指令时，不得视为无边界长文本执行授权；必须先确认已有执行计划，或当场给出本轮执行计划、任务完成条件、任务停止 / 结束条件和最大推进边界。缺少这些边界时，禁止直接进入实现 / 执行。
- 受限计划不得作为实施授权；用户即使明确采纳，agent 也只能先补齐缺失前置条件并将其升级为正式执行计划，未升级前禁止进入编码、改码、重构、测试实施或其他执行动作。
- 若当前轮只是采纳 agent 上一轮或更早轮次给出的建议、方案、修复路线或实施思路，也必须先把该建议收口成正式执行计划，才允许继续实现 / 执行；不得把聊天建议直接当成可执行计划。
- 实施规划阶段默认采用只读计划模式：禁止写代码、禁止边计划边试做；只允许读仓库、定依赖、列风险、拆任务、写实施文档。新项目、项目初期或多来源对象存在多份需求 / 实施文档时，必须先在 `doc/3-实施/` 维护“需求与实施计划全量顺序实施方案”，把需求、实施总览、实施周期和周期内最小任务串成项目级总执行顺序，再进入单来源对象实施总览与实施周期。实施计划正文开头必须先写“当前计划最终方案的简要说明”，用 1-3 句先交代推荐方案、主落点和为什么这么做；随后再写 agent 对当前问题的理解，至少交代问题 / 目标、本轮范围、非范围、当前优先闭环和关键假设 / 待确认点，再进入“依赖图 + 垂直切片”的最小闭环任务拆分；避免按前端 / 后端 / 数据库水平分层堆计划。单任务默认尽量控制在约 5 个文件以内，且每个最小任务都必须先完成自己的真实测试和 `6-review` 风格回归闭环后，才允许推进下一个任务。涉及代码生成、修改或重构的计划，必须显式写出真实测试入口、依赖环境、样本 / 数据来源和通过标准；`build`、`lint`、静态检查或人工阅读不算真实测试，免测只能写在纯文档、纯注释、纯排版、纯静态资源改名 / 搬运或不会影响运行结果的场景里；若计划涉及代码生成、修改或重构，“现状与落点”必须给出代码落点目录树，不能只写文件名或普通条目。
- 若连 `skill-hit-check-rules` 或 `parallel-task-dispatch-rules` 任一都未命中，视为上下文丢失严重、当前基础规则没有正确加载；此时禁止直接进入主任务，必须先补做 skill 命中检查与上下文重同步。
- 若本轮任务存在多 skill 组合、并行拆分或规则收口风险，默认应额外启用 `skill-audit-rules` 进行只读审计。
- 活动质量回归只保留测试后的 `code-style-consistency-rules` `6-review`；不再自动触发业务审查或最终验收。
- 任何模型、CLI、API、浏览器、MCP、安装器、生成器或验证入口出现非预期失败，或退出码为 0 但输出/产物不满足成功标准时，必须在无变化重试前自动触发 `execution-failure-learning-rules`；已注册高风险域还必须在执行前做 active 案例预检。该 Skill 负责分类、查库、快速恢复、同输入复验和 candidate 沉淀，不替代 `bug-*`、`skill-absorption-rules` 或功能测试；未授权不得晋级 active。
- `6-review` 只检查代码风格、格式、位置、注释、日志、可读性和目录归位；不判断业务正确性、需求覆盖或发布放行。
- 若本轮新增或修改任意 skill 资产（`SKILL.md`、`references`、`scripts`、`agents` 等），必须命中 `skill-execution-compliance-gate-rules` 并在收口前给出 PASS / FAIL 结论；改动 `description` 或触发条件追加 `skill-absorption-rules`，涉及多 skill / 职责边界 / 规则收口风险追加 `skill-audit-rules`；改动 `description` 或新增 / 修改 `##` 级标题后，收口前必须重跑 skill 字典生成脚本刷新 `data.js` 与 `字典.md`；上述联动未走完不得收口。
- 任何 agent 收到用户明确结束指令（如“结束”“停止”“到此为止”“不要继续”“不要下一步建议”“不要扩散”）时，必须立即停止自动继续和扩散性输出，只允许给出必要的最小收口结论。
- 若命中 `autonomous-execution-rules`，自动继续只允许用于“完成原始用户目标仍必需的动作”；不得把“进一步优化 / 可继续整理 / 总结里的下一步建议 / 未来迭代建议”自动升级成新的执行目标。
- 当原始用户目标已经完成或用户已给出明确结束指令，且不存在完成原始目标仍必需的动作时，必须停止连续执行并直接结束；不得输出“下一步状态”“下一步建议”“等待用户新指令”“无需继续动作”等任何可能触发循环 loop 的占位区块或扩散性文案，除非用户明确要求后续建议。
- 若当前运行环境存在 goal / plan / task 等显式状态收口机制，且原始用户目标已经完成或已满足该机制的阻断条件，必须在最终收口前真实执行对应收口动作；只写完成文案不算真正结束运行时状态。Codex goal 仅是其中一种特例。

## 代码生成风格入口规则

- 只要本轮新增、修改、重构任意代码、脚本、测试支撑代码或配置型代码，必须在正式写代码前命中 `code-generation-style-rules`。
- `code-generation-style-rules` 负责读取用户本轮要求、目标文件 / 同目录样例、根目录 `PROJECT_STYLE.md` 和已命中的编码类 skill，形成本轮“代码风格契约”。
- 本轮代码风格契约至少覆盖命名、结构、注释、日志、错误处理、复用、排版和禁用写法；后续实现、补丁和测试代码都必须按契约落地。
- 风格优先级固定为：用户本轮明确要求 > 当前文件稳定写法 > 同目录 / 模块写法 > `PROJECT_STYLE.md` 启用样例 > 通用 `code-*`、命名、注释、日志、错误处理等 skill。
- 若 `PROJECT_STYLE.md` 与当前文件 / 同目录稳定写法冲突，优先跟随当前文件 / 同目录，并记录是否需要联动 `project-style-rules` 回写长期风格。
- `project-style-rules` 只负责维护 `PROJECT_STYLE.md` 风格记忆，不作为代码生成总控入口；`code-style-consistency-rules` 负责基于本轮风格契约检查局部一致性。
- 禁止借“统一风格”扩大无关 diff、批量格式化、重排无关代码或引入外部模板式个人偏好。

## Karpathy 风格硬闸门

- 本章节吸收 `multica-ai/andrej-karpathy-skills` 的四个核心原则，并翻译为本仓库自举规则：先想清楚再写、简单优先、手术式改动、目标驱动验证。
- 该规则是全部任务硬闸门，不是软性建议；简单一行任务可以用更短表达完成检查，但不得跳过假设、最小方案、改动范围和验证目标四类判断。
- 编码前必须显式收敛关键假设、成功标准和不确定项；存在多种合理解释时先澄清或给出取舍，不得静默选择一个方向直接实现。
- 实现方案必须坚持简单优先：只做用户当前目标所需能力，不新增猜测性功能、未请求配置项、一次性抽象、单实现接口、无收益 helper / wrapper / manager / factory / adapter。
- 修改既有文件必须保持手术式改动：只碰当前目标必需行，匹配现有风格；不顺手重构、统一格式、改注释、删历史死代码或清理无关变量。若本轮改动制造了新的未使用导入、变量、函数或孤儿文件，必须清理这些由本轮产生的残留。
- 每一行主要改动都必须能追溯到用户目标、计划最小任务或验证闭环；无法追溯的改动视为越界，必须撤回或单独说明并等待授权。
- 每个任务都必须转成可验证目标：Bug 修复优先有复现/回归验证，新增能力优先有行为验证，重构优先有改前/改后等价性验证；仅当纯文档、纯注释、纯排版或不会影响运行结果时，才允许写明免测理由。
- 多步骤任务必须按“步骤 -> 验证点”推进；当前步骤未达到验证点前，不得把后续可选优化升级成当前必做事项。
- 若发现方案已经明显复杂化，应先收缩为更小的具体实现；若继续推进只能依赖猜测、过度抽象或无验证目标，必须停下并回到需求 / 计划 / 测试前置域。
- 本章节与 `code-quality-rules`、`code-style-consistency-rules` 和真实测试类 skill 互补：这些专业 skill 负责具体检查，本章节负责把四原则作为仓库级自举硬闸门同步到 `AGENTS.md` / `CLAUDE.md`。

## 会话动态重命名规则

- 当当前 Codex / Claude / agent 会话进入明确需求、Bug、实施、审查、测试、提交、规则更新，或用户提问后已经能稳定归纳出中文任务主题时，且会话标题为空泛、过时、泛称或不匹配当前任务时，必须自动命中 `thread-title-rules`。
- `thread-title-rules` 负责生成 8-24 字中文简要标题，并按真实工具发现结果更新当前会话标题；Codex App 优先调用只接收 `title` 的统一 MCP 工具 `rename_current_thread`，首次 `INVALID_TITLE` 修正重试仍失败则直接跳过，MCP 未暴露或首次调用的其他失败时仅在真实存在可直接作用于当前会话的 `set_thread_title` 时回退一次，MCP 成功后停止。其他宿主只按真实工具能力执行，不按模型名称推断能力。
- 会话重命名不等待用户显式要求，也不等待最终总结；goal 创建、goal 恢复、上下文压缩续做、长任务阶段切换或执行阶段主题稳定时，都应在过程中尽早尝试重命名。
- 但任务主题尚未稳定、标题已准确、工具不可用、无法可靠确定当前会话 ID、用户明确禁止，或只是最小任务内部小步骤推进时必须跳过，并说明原因。
- `CLAUDE.md` 仅用于 Claude Code 仓库规则自举，不等同于 Claude Desktop 已具备自动会话改名能力。
- 标题采用“任务对象 + 动作 / 症状 / 阶段”的中文简要写法，避免只写“提交 git”“开始实施”“继续做”“修复 bug”“更新文档”等泛化动作标题。
- 禁止用正文伪造工具调用、raw directive 或猜测结果来宣称已经改名；所有标题变更必须来自真实工具返回。

## 注释任务强制流程

- 触发词：补充注释 / 注意中文编码 / 只补注释 / 注释完善 / 加注释。
- 第一步：先声明命中的注释类 skill。
- 第二步：读取对应 `SKILL.md` 后再改代码。
- 第三步：最终回复给执行证据：改动点、UTF-8、格式化/编译/测试结果。

## 上下文压缩续做规则

- 若当前会话刚发生“压缩上下文 / 自动压缩上下文 / 上下文太多”后的重组，默认强制命中 `context-compression-rules`。
- 压缩后继续执行前，必须重新读取当前项目根目录规则文件（`AGENTS.md` / `CLAUDE.md`），恢复仓库级硬规则、必命中 skill 和阻断条件。
- 若压缩后未重新读取规则文件，禁止直接进入任何需求、Bug、编码、测试或交付主任务。
- 若压缩后发现规则文件或项目记忆四件套缺失、损坏或结构不完整，必须先触发 `project-rule-file-bootstrap-rules`，按 `rule-bootstrap` / `memory-bootstrap` 条件路由补齐后再继续主任务。

## 文件编码与写入规则

- 新增或修改注释默认使用中文。
- 所有代码、文档、配置、脚本、测试资产和生成类文本文件，新增或修改时默认使用 UTF-8 编码；禁止使用 GBK、ANSI、系统默认编码或编辑器默认编码落盘。
- 在 Windows、Linux、WSL、容器和远程服务器上写文件时都必须保持 UTF-8 口径一致；不得因为当前终端、区域设置或系统语言不同而切换编码。
- 通过命令行写文件时必须显式指定 UTF-8：PowerShell 使用 `Set-Content -Encoding UTF8`、`Add-Content -Encoding UTF8`、`Out-File -Encoding utf8`，Python / Node / Go 等脚本必须显式声明 UTF-8 读写参数或确保运行时强制 UTF-8。
- 禁止用未确认编码的 `>`、`>>`、默认 `Out-File`、默认 `Set-Content` 或其他依赖 shell 默认编码的方式写入中文、代码、Markdown、JSON、YAML、脚本和规则文件。
- 写入后必须回读关键文件并检查 `git diff`，确认中文未乱码、编码未漂移、换行未被意外批量转换。
- 仓库应提交 `.editorconfig` 与 `.gitattributes` 固定 `charset = utf-8`、基础换行和二进制文件规则；若发现文件被 GBK / ANSI 写入，必须先转回 UTF-8 再继续后续改动。

## 变更最小化

- 注释补充不改变业务逻辑。

## 本地连接调试测试红线（最高优先级，强制）

- 需求侦察、Bug 复现 / 定位 / 运行时调试、功能验证、回归测试、上线接口测试、浏览器联调、启动前后端服务或执行任何测试脚本时，所有数据库、缓存、消息队列、HTTP/RPC 上游、前端 / 后端服务连接都只能使用 `local` 本地环境。
- 「本地环境」的判定标准是**连接信息的配置归属**，不是连接地址是否指向本机：只要连接信息来自 `config_local*`、`.env.local`、`.env.development` 等 local 本地开发配置，即为允许使用的本地环境，即使其指向远程服务器、团队共享开发库或非 `localhost` 地址也属合法本地目标；`localhost` / `127.0.0.1` / 本机端口 / 本机开发容器只是 local 配置的常见形态，不是判定依据。`test`、`prod`、`production`、`staging`、`pre`、`release` 配置声明的连接信息一律禁止使用，即使其恰好指向本机也不例外。
- 即使用户提供了 test / prod 连接串、账号、接口地址或临时授权，也不得由 agent 直接连接或调用；必须记录为环境阻断，并要求改用 local 本地数据库和本地服务。
- 若 local 配置缺失、local 数据不足或本地服务未启动，只能初始化 / 启动 / 查询 local 环境；不得回退到 test / prod 数据库、缓存、消息队列、外部服务或线上接口补证据。
- 需要写入数据时仅允许写入 local 环境，并且必须有清理或回滚方案；Bug 侦察类只读链路仍保持只读，禁止自行增删改数据。

## 依赖与工具复用优先规则

- 编写代码前，必须先检索是否已有可复用的开源库、npm 包或项目内工具类，优先复用而非从零实现。
- 优先级：项目内已有工具类 / 公共方法 > 成熟开源库（npm / GitHub）> 自行实现。
- 禁止在已有成熟第三方库覆盖该能力时重复造轮子；引入新依赖前须确认许可证兼容、维护活跃且无已知安全问题。
- 若确实需要自行实现，必须在代码注释或 PR 说明中写明为何不使用现有库 / 工具类。
- 检索范围应覆盖项目 `utils`、`common`、`helpers` 等公共目录，以及 `package.json` / `go.mod` 等依赖声明文件。

## 最小实现优先级阶梯

- 写代码前按以下顺序逐级判断,停在前一级能成立的就停,不往下跳级:
  1. 需要存在吗?纯推测性需求 → 不写,用一行说明为何跳过(YAGNI)
  2. 代码库已有?已有的 helper / util / 类型 / 模式 → 复用,不重写;改前先搜 `utils` / `common` / `helpers` 及依赖声明文件
  3. 标准库能做?→ 用标准库,不引入第三方
  4. 平台原生特性能覆盖?→ 用原生(如浏览器 `<input type="date">`、CSS 原生能力、DB 约束替代应用层校验),不装库
  5. 已装依赖能解?→ 用它,不加新依赖
  6. 能写成一行?→ 写一行
  7. 都不行: 才写最小可工作代码
- 阶梯在理解问题后才运行,不替代阅读:先读任务和涉及的代码、追完真实流程,再爬阶梯;两级行得通就取更高一级,第一个能用的解就是对的
- Bug 修复走根因不走症状:改前 grep 所有调用方,在共享函数加一处 guard 比每调用方加一处 guard 更小 diff,且不留下兄弟调用方仍然坏掉
- 不写未请求的抽象:没有第二实现的接口、单一产品的工厂、永不改的配置值,都不提前建
- 故意简化处标记 `deferral:` 注释,写明天花板和升级路径,例如 `// deferral: 全局锁, 吞吐量不够时改每账户锁`; 没有升级路径的标记为会被静默忽略的风险
- 与「变更最小化」「依赖与工具复用优先规则」的边界:本阶梯管写什么(方案选择),「变更最小化」管改多少(diff 范围),「依赖与工具复用优先规则」是本阶梯第 2、5 级的具体执行细节;三者互补不冲突
- 不可砍的红线:信任边界的输入校验、防数据丢失的错误处理、安全措施、无障碍基础、硬件校准旋钮、用户显式要求的内容;简化只针对冗余抽象和重复造轮,不针对安全检查
- 非平凡逻辑(分支 / 循环 / 解析 / 金额或安全路径)落地时留一个最小可运行自检(assert 或一个测试文件),平凡一行不需要
- 用户坚持要完整版 → 照建,不再争论

## 输出格式规则

- AI 输出统一使用 markdown，不依赖 HTML 渲染：HTML 标签在 Claude Desktop、纯 CLI、Codex 等大量 agent 环境不渲染，会退化成原文噪声，并破坏对输出的机器解析。
- 视觉层级与区分靠 markdown 语义结构（`#` / `##` 标题、`---` 分隔线、表格、`>` 引用块、徽章 emoji），不靠绝对字号。
- 字号由各 agent 渲染端（客户端主题 / 字体 / 缩放）决定，内容层不强行控制；需要更大字号时调客户端，不在输出里塞 HTML。
- 普通说明、方案、流程、总结、审查报告、线程拆分和状态回报必须使用普通 Markdown 段落、列表、表格或引用块；不得用 ` ```text `、无语言代码围栏、缩进代码块或 HTML 包裹整段自然语言输出。
- 代码围栏只用于真实代码、命令、配置片段、日志片段、JSON/YAML 等需要等宽保真的内容；若只是为了展示可读结构，应改用 Markdown 列表或表格。
- DO NOT send optional commentary.
- 必要的命中检查、阻断说明、执行证据和用户明确要求的解释不属于 optional commentary。

## Windows / WSL 执行规则

> 详细规则与命令模板见 `windows-encoding-rules` skill；WSL↔Windows 工具桥命令（win-python/win-ps/win-cmd/win-copy/win-run-py/win-path）见 `wsl-windows-bridge`。本节为写入规则文件的最小约束摘要。Windows 下普通仓库命令优先使用 Git Bash / bash；PowerShell 不作为普通仓库命令入口，只在 `.ps1`、Windows 专用 cmdlet、PowerShell profile / 编码初始化或用户明确要求时使用。代码位于 WSL 文件系统内且当前动作属于编译、运行、启动程序、测试、调试等执行类命令时，才优先进入 WSL；普通搜索、读写文件、规则检查、普通 git 盘点默认留在 Git Bash / bash。若当前动作已经明确进入 PowerShell 专项场景，还必须同时写入 PowerShell 保底模式：逻辑运算里的 cmdlet 加括号、脚本默认 ASCII-only、null check、变量路径优先 `Join-Path`、`ConvertTo-Json` 显式带 `-Depth`，以及 UTF-8 重定向防护。

**先看 agent 在哪运行：**

- **agent 在 WSL（推荐）**：直接 `cd /home/<user>/<project>` 执行执行类命令，普通命令也可直接在当前 shell 完成，无需任何包裹。
- **agent 在 Windows（如 Claude Desktop GUI）**：
  - 普通仓库命令优先使用 Git Bash / bash；PowerShell 只用于 `.ps1`、Windows 专用 cmdlet、PowerShell profile / 编码初始化或用户明确要求的场景
  - 看代码、改代码、搜索、规则检查、普通 git：经 `\\wsl.localhost\<distro>\home\<user>\<project>` 或 Git Bash / bash 可访问的等价路径访问 WSL 文件
  - 编译、运行、启动程序、测试、调试，以及会真实启动运行时的依赖安装：`wsl.exe --cd /home/<user>/<project> <command>`

**PowerShell 专项保底：** 只有必须进入 PowerShell 时才进入，不把 PowerShell 当普通仓库命令入口；进入后遵守这几条硬规则：逻辑运算里的 cmdlet 加括号、脚本默认 ASCII-only、属性访问前先 null check、变量路径优先 `Join-Path`、`ConvertTo-Json` 显式带 `-Depth`，带空格的可执行文件路径用引号并配合 `&`，日志或文件写入继续按 UTF-8 处理。

**为什么只有执行类动作在 WSL：** 只有 WSL 进程能正常联网，且运行产物面向 Linux；普通读写、搜索、规则检查不依赖 WSL 运行时，强行切换反而容易引入路径或权限问题。

**命令格式：** 执行类命令用 `wsl.exe --cd /home/<user>/<project> <command>`（默认发行版省略 `-d`；多发行版时用 `wsl.exe -l -v` 查名后加 `-d <发行版名>`）。普通命令不要为了统一口径强制套这层。代码在 WSL 时不再使用 `/mnt/<drive>`；纯 Windows 项目或本轮不执行程序时，不要误触发本规则。

**用户可访问文件引用：** 回复用户时，凡引用项目内文件都按用户当前环境可打开的路径输出。项目在 WSL 且用户从 Windows 桌面访问时，Markdown 链接、普通文本路径、审查证据路径、截图说明和最终总结中的项目内文件路径都使用 `\\wsl.localhost\<distro>\home\<user>\<project>\...`；只有命令参数、WSL shell 上下文和日志原文保留 `/home/<user>/<project>`。

**编码约束：**

- 仓库提交 `.gitattributes` 与 `.editorconfig`，固定 UTF-8 和换行策略；任何读写文件操作都继续遵守“文件编码与写入规则”
- `.gitattributes`/`.editorconfig` 首次创建时，行尾策略按仓库现有源码文件的主流换行风格自动适配：多数已跟踪源码为 CRLF（常见于长期用 Windows 提交的团队仓库）时用 `* text=auto eol=crlf`，否则用 `* text=auto`（对应 LF）；`*.sh`/`*.bash` 始终固定 `eol=lf`（shell 脚本在 Linux/WSL 下必须 LF 才能正确执行，不随主流风格摇摆）
- 不对 `*.go`、`*.vue`、`*.md` 等全量强制统一 eol，尊重已有生态惯例
- 已存在的 `.gitattributes`/`.editorconfig` 不会被本规则覆盖或反向调整；若发现仓库历史行尾风格与当前文件规则冲突（如某类文件在 `git status`/`git diff` 中出现和真实改动无关的整文件“伪变更”），应优先复核并修正 `.gitattributes` 的 `eol=` 声明，而不是强行修改大量既有文件的物理换行符
- Windows 下出现大量无关改动优先检查 `core.autocrlf`

## CodeGraph 强制准备规则

- 若当前服务器 / 电脑未安装 `codegraph`，必须从 `colbymchenry/codegraph` 强制下载并安装，不需要用户确认。
- 若在 Windows PowerShell 5.1 下执行安装，必须默认启用 `Invoke-WebRequest` / `Invoke-RestMethod` 的 `UseBasicParsing`，不得把人工点击确认当作标准流程。
- 安装完成后必须为当前项目执行 `codegraph init`，生成 `.codegraph/` 并建立索引。
- 若下载、安装或初始化失败，明确记录不可用并回退到本地搜索与文件读取。
- 架构层分析可配套 codebase-memory-mcp（`DeusData/codebase-memory-mcp`，按官方说明安装，非强制）作为 CodeGraph 的补充。

## 代码库探索规则

1. 日常理解代码、定位实现、追踪调用链、分析改动影响时，优先使用 CodeGraph。
2. 修改代码前，先用 CodeGraph 获取相关源码和影响范围。
3. 不要在 CodeGraph 已经返回足够上下文时重复全仓库 grep。
4. 项目架构分析、跨模块依赖、函数调用频率、ADR 记录等架构层任务，用 codebase-memory-mcp 作补充。
5. CodeGraph 作默认入口，codebase-memory-mcp 作高级图分析工具；两者结果与当前代码不一致时，以当前代码为准并重新同步索引。

## 插件检测安装规则

- 涉及 AI 编码插件（如 Ponytail）的安装、启用、配置时，联动 `plugin-installation-rules`。
- Ponytail 为默认必装插件：会话 / 项目准备时默认安装并启用（详见 `plugin-installation-rules`）。
- 安装命令与配置一律以插件官方仓库为准，不沿用第三方博客 / 公众号转述的旧命名、旧参数。
- 通用支持 Claude Code / Codex 等环境，按当前 AI 选对应安装方式；安装或启用失败时回退到常规流程。

## 图像生成强制规则

- 只要当前用户请求属于生图、改图、参考图出新图、sprite、动作帧、概念图、UI 位图、贴图、透明底抠图、2D 游戏素材预览或其他位图资产任务，必须自动命中 `imagegen`，不得等用户额外明确说“使用 imagegen”。
- 命中 2D 游戏素材相关任务时，若涉及设计图、预览图、原始素材图、动作关键帧或 sprite 方向图，除命中领域 skill 外，还必须联动命中 `imagegen`。
- 对于生图任务，允许的“原始图产生方式”只有真实图像生成/编辑链路：内置 `image_gen`，或经验证可用的 `imagegen` CLI/API 图像通道。
- 严禁把 Pillow、SVG、HTML/CSS/canvas、脚本拼接、程序绘制、几何组合、占位图、自动排版图、后处理脚本输出伪装成“已完成生图结果”或“最终素材”。
- CLI fallback 仅表示“改走 imagegen 的脚本入口去调用真实图像生成/编辑 API”，不表示允许退化成脚本合成图片；凡是不经过真实图像模型生成的结果，一律不得作为生图成品交付。
- 如果内置 `image_gen` 不可用，必须先验证 `imagegen` CLI/API 链路；若也不可用，则明确阻断并只允许交付 prompt、brief、参考候选、动作规划等中间信息，不得交付脚本生成图冒充成品。
- 后处理脚本只允许在“真实生成出的原始图”基础上做去背、切帧、对齐、拼表、预览整理；不得替代 imagegen 负责原始创作出图。

## 项目特有规则（cpa-workbuddy-plugin，非受管）

> 本节为项目专属事实与约定，由项目分析沉淀；受管章节重新同步时本节保留。正文以当前代码与最新 CHANGELOG 为准。

### 仓库与发布

- 仓库：`luode0320/cpa-workbuddy-plugin`（原 cpa-plugin，2026-08-22 改名）；物理目录 F:\cpa-plugin。
- 三插件独立版本、独立发布：`workbuddy-provider`（主，腾讯 CodeBuddy CN+Global）、`qoderwork-provider`（QoderWork CN 逆向）、`workbuddy-token-usage`（用量 dashboard）。
- registry.json：schema v2；`plugins` 是 **list**，条目按 `id` 匹配（`name` 只是展示名）；zip 列表在 `install.artifacts`（7 平台：darwin/linux/windows/freebsd × amd64/arm64）。
- tag pattern：`workbuddy-provider-v*` / `qoderwork-provider-v*` / `workbuddy-token-usage-v*`；workflow_dispatch 必须传 `plugin=<id> version=<ver>`，否则 release job 跳过。
- 发布链路（不可跳步）：bump VERSION+main.go `var version` → commit → push main → dispatch CI → 下载 8 assets（7 zip + checksums）→ **git add assets + push**（0.9.7 教训：漏 add 会 404）→ `publish-assets.py` → commit registry + push → 远端验证 raw URL 200。
- push 必须带：`GIT_TERMINAL_PROMPT=0 GIT_ASKPASS='C:\Users\luode\.github\git-askpass.sh' git -c credential.helper= push https://...`；askpass 用完即删；Windows 路径必须用 `C:\...` 形式（Git 不认 `/c/`）。
- 并行工作区安全发布：`git add` 必须显式列文件（禁 `git add <dir>`），staged 后 `git diff --cached --name-only | grep` 反向确认无外来文件混入。
- 资产下载：`scripts/download-release-assets.py`（urllib 直连 releases/download + sha256 自校验）；registry 校验 `scripts/validate-registry.py`。

### 插件架构事实

- **workbuddy 与 qoderwork 并非完全同构**（HEAD 基线差异：stream.go 224 行 / main.go 420 行 / scheduler.go 123 行）：
  - qoderwork 是旧版：publishUsage 8 参数（无 accountLabel/reasoningEffort）、无 preserve watchdog、无 session_auth（无会话粘性）、SSE 嵌套解包（outer["body"]）、认证走 `applyCosyHeaders`（COSY 签名）+ qoderEncode 编码体。
  - workbuddy 是新版：publishUsage 带 reasoningEffort/accountLabel、有 preserve watchdog、session_auth、`backendHeaders`+`endpointChatFor(sa)`、stripDataPrefix SSE。
  - 同步改动只能逐函数适配，不能整体覆盖文件；accountFailover.go 等纯逻辑文件可整文件同步。
- 插件是 c-shared（import "C"）；Windows 无 gcc 时 main.go 被工具链忽略（`undefined: storedAuth` 是环境假象，非真实错误）；本地验证一律 `python scripts/cgo-shim-build.py <plugin>`。
- 磁盘写路径：`host.auth.save` 会丢未知顶层字段 → 直写物理 auth 文件（`writeAuthFileDirect` + fsnotify），host 侧 watcher 映射 disabled 进调度器；auth 目录 `~/.antigravity_cockpit/<plugin>_accounts/`。
- config_yaml 经 host RPC 传输时 `[]byte` 走 base64；测试必须 `json.Marshal(map{"config_yaml": []byte(yaml)})`。
- 管理路由前缀随 plugin id：`/v0/management/plugins/<id>/*`；panel.html 的 `const API` 必须与 id 一致（0.9.7 404 根因）。

### 关键设计决策

- 数据库/配置一律逻辑引用（无物理外键）。
- failover：1/3/10 分钟阶梯退避；429/402/5xx/传输错误（status==0）计入，业务 400 不计；cooldown 期间跳过该账号。
- 40x 换号重试（0.9.9+）：401/403/404/405 计入账号级故障，`retry_on_4xx` 预算默认 3（0-5）；**缺省键保持当前值**（kill switch 安全：无关 reconfigure 不得重置 `retry_on_4xx: 0`）。
- 路由（0.12.0 起）：移除三池（priority/default/fallback），只留保号池（watchdog 积分阈值自动归池，默认 10m 刷新、阈值 50）；存量 auth 文件 `pool`/`priority` 字段忽略式读取不清理。
- 版本三轨（qoderwork）：main.go 0.8.2 / VERSION 0.4.1 / registry 0.2.x 历史双轨，发版以 registry 为准。
- 跨插件数据通道：NDJSON 文件 feed（`<CLIProxyAPI root>/data/token-usage-feed.ndjson`，超 128MB 截断）而非共享 bbolt（排它锁冲突）。

### 本地开发与验证

- Windows 无 CGO：验证一律走 `python scripts/cgo-shim-build.py <plugin>`（build+vet+test 全绿才算过）。
- 新增测试须确认真实进编译：临时加必失败哨兵 Test → shim 输出 FAIL 证实编译 → 删哨兵重跑。
- panel.html 两个 `<script>` 块用 node `new Function()` 或 `node --check` 做语法校验；UI 事件链 review 时 grep `data-action=` ↔ 处理函数 ↔ 函数内 const 是否同作用域。
- 测试 raw JSON 里 hardcode 的 `"type":"workbuddy"` / `"provider":"workbuddy"` 必须 `fmt.Sprintf` 插 `providerName`，否则 mergeAuthDoc/handleParseAuth 不匹配。
- Go 文件 gofmt 用 CRLF→LF 临时转换再校验（历史 tracked 文件 CRLF，gofmt -l 全量列是假象）。
