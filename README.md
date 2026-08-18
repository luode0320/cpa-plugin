# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供 **WorkBuddy / CodeBuddy** 与 **QoderWork (CN)** 两个 OAuth Provider。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | Tencent CodeBuddy OAuth、动态模型、executor、CN 每日签到、Global 专家包、积分面板、可选积分调度 | [workbuddy/](workbuddy/) |
| `qoderwork` | QoderWork CN（qoder.com.cn）：OAuth 设备授权 + PAT 双登录（可共存）、COSY 签名推理、动态模型、每日签到、积分面板、token 保活 | [qoderwork/](qoderwork/) |

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

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `qoderwork-v0.2.6`）或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip qoderwork_0.2.6_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp qoderwork.so /path/to/cliproxyapi/plugins/qoderwork.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp qoderwork.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy:
      enabled: true
    qoderwork:
      enabled: true
```

## 远程更新（插件商店自定义源）

CPA 插件商店源添加：

```text
https://raw.githubusercontent.com/luode0320/cpa-plugin/main/registry.json
```

然后在商店 UI 安装/更新 **workbuddy** 和 **qoderwork**。
