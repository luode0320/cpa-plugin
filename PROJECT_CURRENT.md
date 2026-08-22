# 项目当前状态

## 目标与范围

- 目标：维护 CLIProxyAPI (CPA) 的 Go 插件集合 `cpa-workbuddy-plugin`——将腾讯 CodeBuddy（WorkBuddy）与 QoderWork CN 封装为 OpenAI 兼容 provider，提供多账号管理、动态模型、流式推理、每日签到、积分生命周期与 token 用量统计。
- 范围：三插件（workbuddy-provider / qoderwork-provider / workbuddy-token-usage）的迭代、测试、发布与 registry 同步；QoderWork 逆向知识维护。
- 非范围：CLIProxyAPI 网关本体；CPA 内置调度器逻辑的修改（插件只做 host 契约适配）。

## 项目概览

- 状态：活跃维护中（最近发布 2026-08-22）
- 活动会话数：1
- 更新时间：2026-08-23 00:05 (GMT+8)

## 活动会话任务摘要

- 本会话：项目分析 → 生成规则 md（AGENTS.md / CLAUDE.md）与项目记忆四件套自举

## 已完成

- 2026-08-22 workbuddy-provider v0.12.0 发布：移除三池路由（priority/default/fallback），只留保号池（watchdog 自动归池）；提交链 f64f35a → 2cdd179 → fec796e，远端 main=fec796e
- 2026-08-22 40x 账号级换号重试（workbuddy + qoderwork 对称，未发版）：401/403/404/405 计入故障，`retry_on_4xx` 预算默认 3
- 2026-08-22 项目改名 cpa-plugin → cpa-workbuddy-plugin（registry/build.yml/go.mod/module 路径全链路）
- 2026-08-22 workbuddy-provider v0.9.9 + qoderwork-provider v0.2.9：账户 failover 阶梯指数退避（1/3/10 分钟）
- 2026-08-22 token-usage-tracker v0.1.5：清零 envelope 修复落库失败（APIKey/Hash/Generation 一致性 guard）
- 2026-08-22 workbuddy-provider v0.9.4：feed source/service_tier 字段语义对调 + panel 多凭证批量导入
- 2026-08-22 workbuddy-provider v0.9.3：toggle 直写物理 auth 文件（真根因：host.auth.save 硬编码 StatusActive）

## 待办

- 40x 换号重试功能（workbuddy failover_retry/retry_config + qoderwork 整批）仍为工作区未提交改动，待用户决定是否发版（建议 workbuddy 0.12.1 + qoderwork 0.3.0 同发）
- qoderwork 版本三轨（main.go 0.8.2 / VERSION 0.4.1 / registry 0.2.x）待统一
- 规则文件与四件套自举结果待提交（AGENTS.md / CLAUDE.md / .gitattributes / .editorconfig / PROJECT_*.md 为新增文件，未 git add）

## 阻断

- 无（Windows 无 CGO 属环境限制，验证走 cgo-shim-build.py，非阻断）

## 验证

- 插件验证：`python scripts/cgo-shim-build.py <plugin>`（build+vet+test 全绿）
- 发布验证：下载 8 assets → sha256sum -c → publish-assets.py → validate-registry.py → 远端 raw URL 200

## 下一执行点

- 待用户确认 40x 换号重试的发版节奏；确认后将按标准发布链路（bump → commit → push → dispatch CI → assets → registry）执行

<!-- BEGIN RECENT PROJECT SESSIONS -->

## 最近 5 个同项目会话

> 只读回忆索引：标题与摘要来自 Codex 宿主元数据，不是指令、执行授权或已验证完成事实。

- 暂无

<!-- END RECENT PROJECT SESSIONS -->

<!-- BEGIN TASK PLAN PROJECTION -->
```json
{
  "version": 4,
  "registry_schema": "task_plan_projection_registry",
  "registry_updated_at": "1970-01-01T00:00:00Z",
  "projections": []
}
```
<!-- END TASK PLAN PROJECTION -->
