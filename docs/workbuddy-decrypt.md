# WorkBuddy 桌面凭据导出（Windows）

WorkBuddy 桌面版 5.6 起本地凭据文件是**加密信封**（`{$wbEncrypted:1, envelope:...}`），
janus 只接受明文。本文档说明如何用 `janus workbuddy-decrypt` 子命令在**装了并登录过
WorkBuddy 桌面版的 Windows 机器上**把凭据解成明文，供 janus 的 `WORKBUDDY_AUTH_FILE`
使用。

> 原理：密钥封在 WorkBuddy 改版 Electron 的私有 `workbuddyStorage` 绑定里，必须用
> **它自己的二进制**以 `ELECTRON_RUN_AS_NODE=1` 跑一行脚本解封。因此解密必须在装有
> 桌面版的机器上执行；Linux 服务器上做不了。

## 前置条件

- 一台装了 **WorkBuddy（国内版）** 或 **WorkBuddyAI（国际版）** 桌面版、且**已登录**的
  Windows 机器（Win10/11，x64）
- `janus_windows_amd64.exe`（v0.3.55+；从 [Releases](https://github.com/qist/janus/releases)
  下载，或本机 `make dist-windows` 自建）

## 步骤

### 1. 导出明文凭据

PowerShell 里执行（exe 所在目录下）：

```powershell
.\janus_windows_amd64.exe workbuddy-decrypt -o out.json
```

正常情况下会自动完成三件事：

| 自动发现项 | 探测位置 |
|---|---|
| 桌面版可执行文件 | `%LOCALAPPDATA%\Programs\WorkBuddy\WorkBuddy.exe`（国内版）<br>`%LOCALAPPDATA%\Programs\WorkBuddyAI\WorkBuddyAI.exe`（国际版） |
| 加密凭据文件 | `%LOCALAPPDATA%` 与 `%APPDATA%`（Roaming）下的<br>`CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info`（国内版）<br>`CodeBuddyExtension\Data\Public\auth\workbuddy-desktop-ai.info`（国际版） |
| 解封密钥 | 用桌面版自己的 exe 跑一行脚本（`ELECTRON_RUN_AS_NODE=1`） |

成功输出形如：

```
reading C:\Users\you\AppData\Local\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop-ai.info
resolving at-rest key via C:\Users\you\AppData\Local\Programs\WorkBuddyAI\WorkBuddyAI.exe
done. wrote out.json — 把它放到 janus 主机并配 WORKBUDDY_AUTH_FILE 指向它即可。
```

`out.json` 即明文凭据（含 `accessToken` / `refreshToken`）。

### 2. 放到 janus 主机

把 `out.json` 拷到 janus 服务器，按 `janus.env` 里 `WORKBUDDY_AUTH_FILE` 指向的路径放置
（默认模板建议 `/etc/janus/workbuddy-auth.json`），权限收紧到 0600：

```bash
scp out.json root@<janus主机>:/etc/janus/workbuddy-auth.json
ssh root@<janus主机> 'chmod 600 /etc/janus/workbuddy-auth.json && systemctl restart janus'
```

重启后日志出现 `workbuddy provider enabled: region=... models=N` 即激活成功；
`/v1/models` 里会出现 `workbuddy/<id>` 系列模型。

### 3. 验证

- janus 主机：`curl -s http://127.0.0.1:2810/healthz`，再打开 `/ui` 面板切到
  **WorkBuddy** 渠道——能看到积分余额即凭据有效。
- 明文里的 `refreshToken` 可无限续期，**导一次长期可用**；accessToken 过期后 janus
  自动续期并回写凭据文件，不需要再来桌面机导出。

## 常用参数

| 参数 | 作用 | 什么时候用 |
|---|---|---|
| `-o out.json` | 写文件（0600） | 推荐；不加则打到 stdout |
| `-bin <exe路径>` | 指定桌面版可执行文件 | 自动发现失败（自定义安装路径）时 |
| `-in <info路径>` | 指定加密凭据文件 | 凭据不在默认位置时 |
| `-variant cn\|ai` | 只探国内版 / 国际版 | 两个产品都装了、想明确导哪个 |

## 故障排查

| 报错 | 原因与处理 |
|---|---|
| `没有自动找到 WorkBuddy 桌面版可执行文件` | 安装位置自定义了，用 `-bin` 指向桌面版 exe |
| `没有找到桌面凭据文件` | 这台机器没装/没登录过桌面版；或用 `-in` 直接指定 `.info` 文件 |
| `文件是空的（桌面版可能还没登录）` | 先打开 WorkBuddy 登录一次再导 |
| `本机 WorkBuddy 的当前密钥（keyId ...）与凭据信封（...）不匹配` | 凭据是**另一处安装**封的（比如重装过系统）。`-in` 指向的凭据文件要在封存它的那台机器上解 |
| `不是合法 JSON` / `无法识别的形态` | 凭据文件已损坏或版本过旧，重新登录桌面版刷新凭据后再导 |

## 安全注意

- `out.json` 等于账号本身：**不要提交进 git、不要发群里**；导完尽快拷走并删除本地副本。
- janus 侧凭据文件保持 `0600`，只有运行账号可读。
- 若怀疑泄露：在 WorkBuddy 客户端退出登录（作废 refresh token），重新登录后重新导出。
