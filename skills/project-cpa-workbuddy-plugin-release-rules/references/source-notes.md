# Source Notes

## 2026-08-23 创建：project-cpa-workbuddy-plugin-release-rules

- **创建原因**：用户点名「把提交、push、构建、registry 等后续一整套发布的流程吸收到项目的 skill」
- **来源**：cpa-workbuddy-plugin 0.12.0 / 0.12.1 / 0.12.2 三次完整发布实操（对话 + .workbuddy/memory/2026-08-22.md + 2026-08-23.md）
- **覆盖**：版本 bump 三处、混合文件分离发布、HTTPS+askpass push、dispatch CI、下载 assets、publish registry、远端验证、12 条实测踩坑
- **关联 skill**：cgo-plugin-isolated-test（本地验证职责，本 skill 引用不重复）；luode-skills 仓库 `project-interface-release-execution-rules` 是接口测试门禁（不同职责域，无重叠）
- **边界**：只管发布执行链路；代码正确性验证、需求/编码规则在其他 skill
