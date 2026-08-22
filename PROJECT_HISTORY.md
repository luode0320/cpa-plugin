# 项目历史事件

> 本文件追加关键历史事件并只保留最近 20 条（按日期倒序、新事件置顶、追加后自动裁剪）；普通启动默认不读取，只有历史追问、当前状态不足或真实卡点时才窄检索。

## 事件

- 2026-08-22：workbuddy-provider v0.12.0 发布，移除三池路由只留保号池（提交链 f64f35a→2cdd179→fec796e）
- 2026-08-22：40x 账号级换号重试（401/403/404/405，retry_on_4xx 预算默认 3），workbuddy+qoderwork 对称，未发版
- 2026-08-22：项目改名 cpa-plugin → cpa-workbuddy-plugin，registry/build.yml/go.mod 全链路同步
- 2026-08-22：workbuddy-provider v0.9.9 + qoderwork-provider v0.2.9 账户 failover 阶梯退避发布
- 2026-08-22：token-usage-tracker v0.1.5 清零 envelope 修复落库失败
- 2026-08-22：workbuddy-provider v0.9.4 feed 字段语义对调 + 多凭证批量导入
- 2026-08-22：workbuddy-provider v0.9.3 toggle 直写物理 auth 文件（host.auth.save 硬编码 StatusActive 根因）
- 2026-08-22：三插件 id 改名（workbuddy→workbuddy-provider 等），build.yml matrix 拆 id/src
- 2026-08-22：registry 显示名改名（WorkBuddy Provider 等），版本 0.9.5/0.2.7/0.1.6
- 2026-08-19：token-usage-tracker 拆分决策（文件 feed 而非共享 bbolt）

## 计数锚点区

> 本区由 `memory-usage-tracking-rules` 收口闸门维护：HISTORY 仅窄读计入，会话启动不读不计；被裁剪事件的锚点随事件一起删除（不保留 retired）；本区计数仅作主题热度弱信号。锚点 key 用事件 `- YYYY-MM-DD：` 后的核心主题短语（约前 12 字符，可前缀匹配）。

```yaml
version: 1
anchors: []
```
