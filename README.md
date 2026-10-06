# Janus

把 **OpenCode server**（只有 `/api/*`）包装成 **OpenAI 兼容 API**（`/v1/*`），
让只认 OpenAI 协议的客户端（Trae、CodeBuddy、Cursor、OpenAI SDK、LangChain…）
直接使用 OpenCode 的模型与 agent 会话。

```
Trae / CodeBuddy / openai-sdk
        │  OpenAI 协议
        ▼
  Janus  :2810
        │  OpenCode HTTP API + SSE
        ▼
  opencode serve          :随机端口（桌面端每次重启都变）
```

## 为什么需要它

OpenCode 暴露了 116 个路径，**全部是 `/api/*`**，没有 `/v1/*`。
`/api/experimental/generate` 是唯一接近"无状态补全"的端点，但
`opencode/*` 的免费模型会直接拒绝：

```
"OpenCode's free tier can only be used from within OpenCode"
```

所以本桥的**主路径是 agent 会话**（`/api/session/{id}/prompt` + `/api/event`），
而不是无状态 generate。这也顺带让客户端获得了 agent 能力（工具调用、文件读写）。

## 快速开始

```bash
# 1. 编译
cd janus
make build                 # 自动注入版本号（git tag / VERSION 文件）

# 2. 配置（最少只改这两个）
cp janus.env.example janus.env
$EDITOR janus.env        # 设 BRIDGE_API_KEY 和 BRIDGE_DIRECTORY

# 3. 启动（自动发现上游，无需填端口/密码）
./scripts/run.sh
```

启动日志：

```
INFO  Janus v0.1.0 (commit none, built 2026-10-04T06:04:03Z)
INFO  auto-discovered OpenCode at http://127.0.0.1:61573 (pid 697)
INFO  janus listening on 0.0.0.0:2810 (upstream=..., dir=/path/to/project, agent=build)
INFO  event stream connected (text/event-stream)
```

用 `scripts/run.sh` 可以带默认值一键启动。配置文件见 `janus.env.example`，
优先级是**真实环境变量 > janus.env > 内置默认值**。

### 构建、版本与发布

```bash
make build          # 本机二进制（-trimpath + 注入 Version/Commit/Date）
./janus --version   # Janus v0.1.0 (commit abc1234, built ...)
make test           # 单元测试
make test-race      # 竞态检测（需本机 C 工具链）
make vet            # go vet
make dist-linux     # Linux 全架构发布包：amd64 / arm64 / arm / 386
make dist-darwin    # macOS：amd64（Intel）/ arm64（Apple Silicon）
make dist-windows   # Windows：amd64 / arm64（zip）
make dist           # 全部平台（linux + darwin + windows）
```

- 版本号来源：`make VERSION=...` > `git describe --tags` > `VERSION` 文件 > `dev`
- 发布包为**纯静态**二进制（`CGO_ENABLED=0`），产物在 `dist/janus_<version>_<os>_<arch>.tar.gz`
  （Windows 为 `.zip`）；包里除二进制外还附带 **`janus.env.example`**（配置模板）与 **`README.md`**
- 推 `v*` tag 会触发 GitHub Actions（`.github/workflows/release.yml`）自动出 Release + 全平台附件；
  普通 push / PR 走 `.github/workflows/ci.yml`（vet + test + **test-race** + build + `--version`）
- **Docker / systemd 部署**：见 `deploy/`（`Dockerfile` 基于 distroless 非 root 静态镜像；`deploy/janus.service` 为加固后的 systemd 单元）

### 跨平台说明（Linux / macOS / Windows）

| 平台 | 产物 | 默认数据目录（DB / workspaces） |
|---|---|---|
| Linux | `janus_<v>_linux_<arch>.tar.gz` | `$XDG_DATA_HOME/janus` 或 `~/.local/share/janus`；workspaces 默认 `/var/lib/janus/workspaces` |
| macOS | `janus_<v>_darwin_<arch>.tar.gz` | `~/Library/Application Support/janus`（workspaces 在其下） |
| Windows | `janus_<v>_windows_<arch>.zip` | `%LOCALAPPDATA%\janus`（workspaces 在其下） |

- 以上默认值只在**未显式配置**时生效；设了 `BRIDGE_DB` / `BRIDGE_WORKSPACES_DIR` / `OPENCODE_DB` 一律以配置为准。
- `OPENCODE_DB`（上游 OpenCode 的库）仍按 OpenCode 自己的 XDG 约定找（macOS 上也是 `~/.local/share/opencode`），未配置 `sqlite3` 时 `/v1/usage` 不可用（可选功能）。
- 上游 OpenCode 需要单独安装（`opencode` 在 PATH 上即可，Windows 会按 `opencode.exe` 查找）。

### 在 Windows 上运行

1. 解压 `janus_v<版本>_windows_<arch>.zip`（得到 `janus_windows_<arch>.exe`）。
2. 装上游 OpenCode（`opencode.exe` 放进 PATH 即可；janus 会自动发现/拉起它）。
3. 把 `janus.env.example` 复制成 `janus.env`，至少设：
   ```ini
   BRIDGE_ADDR=127.0.0.1:2810
   BRIDGE_API_KEY=sk-change-me
   ```
4. 运行（两种任选）：
   ```bat
   :: A) 配置文件放在 exe 同目录或当前目录，直接运行即可
   janus_windows_amd64.exe

   :: B) 显式指定配置文件
   set JANUS_CONFIG=C:\path\to\janus.env
   janus_windows_amd64.exe
   ```
   配置查找顺序：`JANUS_CONFIG` / `BRIDGE_CONFIG` > exe 同目录 `janus.env` > 当前目录 `janus.env`。
5. 客户端 Base URL 填 `http://127.0.0.1:2810/v1`，Key 填 `BRIDGE_API_KEY`。

> Windows 无 systemd；要开机自启可用「任务计划程序」或 `nssm`。
> `/v1/usage` 需要 `sqlite3.exe` 在 PATH（可选）。

### 客户端配置

| 项 | 值 |
|---|---|
| Base URL | `http://<bridge-host>:2810/v1` |
| API Key | `BRIDGE_API_KEY` 的值（若已设置） |
| Model | **`default`** — 使用 `BRIDGE_DEFAULT_MODEL`；未配置时跟随上游当前默认 |

**推荐填 `default`，不要写死具体模型名。** 通过 `BRIDGE_DEFAULT_MODEL` 固定你订阅的套餐模型；
未配置时才实时解析上游 `GET /api/model/default`。
当前配置已设为 `opencode-go/gpt-6-luna`。`auto` / 留空也是同一个别名。

需要指定模型时，填 `/v1/models` 里的 `provider/model`，例如
`opencode-go/deepseek-v4.1-flash`。模型列表：`GET /v1/models`
（首条固定是 `default`，共 69 条）。

## 自动发现 / 自动拉起上游（重要）

OpenCode 桌面端/Hub **每次重启都会换随机端口和随机密码**，写死配置撑不过一次重启。
因此默认 `OPENCODE_URL=auto`：

1. 启动时先扫 `/proc/*/cmdline` 找已在跑的 `opencode ... serve --port N` 进程
2. 从 `/proc/<pid>/environ` 读 `OPENCODE_SERVER_PASSWORD`，Basic 探活 `/api/info`
3. **没找到在跑的**：检查 `opencode` 是否安装——
   - 已安装 → 由本桥以随机端口拉起
     `opencode serve --hostname 127.0.0.1 --port <随机>`，密码由本桥生成并直接使用
     （`OPENCODE_AUTOSTART=false` 可关闭自动拉起）
   - 没安装 → 打日志提示安装：`curl -fsSL https://opencode.ai/v2/install | bash`
4. 断连时后台每 30s 重试一次（同样是「先发现、没有就拉起」），探到新端点就热替换（含 SSE 重连）

所以桌面端开不开都行：开着就复用它，没开本桥自己拉一个。模型授权（OAuth）仍由 OpenCode
自己存在库里，本桥不参与。

若要指向固定/远程实例，显式设置 `OPENCODE_URL` 即可（此时需同时给 `OPENCODE_PASSWORD`，
且不会自动拉起）。

> 需要与 OpenCode 进程同用户运行、且 `/proc` 可读（Linux）。其他平台请显式配置。

### 用量与余额（`GET /v1/usage`）

OpenCode 的**本地** server 只暴露"已消耗"（`/api/experimental/session/stats`），
真正的**余额**在 `opencode.ai` 的 console API 上，需要 OAuth 凭据。

桥把两者合并成一个端点：

```bash
curl -s http://127.0.0.1:2810/v1/usage -H "Authorization: Bearer sk-your-key"
```

```jsonc
{
  "billing": {
    "mode": "pay-as-you-go",
    "billing_mode": "prepaid",
    "balance_usd": 0,          // 余额
    "available_usd": 0,        // 可用（余额 + 信用额度）
    "credit_limit_usd": null,
    "can_purchase_credits": true,
    "can_enable_auto_recharge": true
  },
  "totals": { "requests": 4109, "input_tokens": 16613841, "output_tokens": 3019449,
              "cache_read_tokens": 1557191801, "cost_usd": 8.60 },
  "models": [                  // 按模型分组的累计消耗
    { "provider": "opencode-go", "model": "deepseek-v4.1-flash",
      "requests": 3754, "cost_usd": 8.5879 },
    { "provider": "opencode", "model": "fledge-alpha-free",
      "requests": 82, "cost_usd": 0 }
  ],
  "by_day": [ { "date": "2026-10-03", "requests": 2815, "cost_usd": 5.176 } ],
  "go": {                       // Go 套餐三窗口限额（非 Go 账号时省略）
    "rolling": { "status": "ok", "percent": 4,  "resets_in_sec": 10900 },
    "weekly":  { "status": "ok", "percent": 50, "resets_in_sec": 43100 },
    "monthly": { "status": "ok", "percent": 25, "resets_in_sec": 2411600 }
  },
  "fetched_at": "2026-10-03T14:20:24Z",
  "source": "https://opencode.ai/console/api"
}
```

**怎么拿到余额的**：桥只读打开 OpenCode 的 SQLite 库
（`credential` 表，`integration_id='opencode'`），取出 OAuth access token 和 `orgID`，
再调 console API：

| Console 端点 | 内容 |
|---|---|
| `GET {console}/billing/status` | 余额、可用额度、计费模式 |
| `GET {console}/usage/summary` | 总消耗（tokens + cost） |
| `GET {console}/usage/models` | 按模型分组 |
| `GET {console}/usage/cost-by-day` | 按天分组 |
| `GET {go}/v1/usage` | Go 套餐 5h 滚动 / 周 / 月限额（inference 主机） |

金额单位是 **micro-cents**（1 USD = 1e8 micro-cents），桥已换算成 `*_usd`。

`go` 区块来自 inference 主机（默认 `https://opencode.ai/inference/go/v1/usage`），
用同一个 OAuth token 鉴权。它是**非公开接口**（OpenCode 只在 console 页面展示，
尚未提供官方 API），失败或非 Go 账号时静默省略，不影响其余字段。每个窗口给出
已用百分比 `percent`、重置时间 `resets_at` 和倒计时 `resets_in_sec`；超过 100% 会被限流。

**重要限制**：

- **没有"按模型的剩余额度"这种东西** —— 额度是**账号级**的（`balance` / `available`）。
  `/usage/models` 给的是每个模型**已消耗**多少，不是还剩多少。
- 想按模型限额，得用 console 的 budget 功能（`/api/budgets/*`），当前你的账号未设置。
- 依赖 `sqlite3` CLI 和本地凭据库；取不到时端点返回 503 并说明原因，不影响对话功能。
- 用 `BRIDGE_USAGE_ENABLED=false` 可关闭。

**余额为 0 时的实际影响**（实测）：

| 模型 | 结果 |
|---|---|
| `opencode/fledge-alpha-free` 等 `*-free` | ✅ 正常 |
| `opencode-go/*` | ✅ 正常（走独立额度） |
| `opencode/claude-sonnet-5-5`、`opencode/gpt-6.1-sol` 等付费档 | ❌ 429 `insufficient_quota` |

先用 `GET /v1/usage` 看余额，再用 `GET /v1/models?provider=opencode-go` 挑还能用的模型。

相关环境变量：`OPENCODE_DB`（凭据库路径）、`OPENCODE_CONSOLE`（console API 基址）、
`BRIDGE_USAGE_TTL`（缓存时长，默认 30s）。

### Web 用量面板（`GET /ui`）

浏览器打开 `http://<bridge-host>:2810/ui`（根路径 `/` 自动跳转），零前端依赖、离线可用：

- **余额**：余额 / 可用额度 / 信用额度 / 计费模式
- **Go 套餐限额**：5 小时滚动 / 每周 / 每月的已用百分比与重置倒计时（仅 Go 账号显示，数据来自 inference 主机的非公开接口）
- **最近请求（缓存命中率）**：逐条列出上游模型请求的命中/未命中/命中率/输出/耗时（`/v1/requests`，可直接对账官方 console 的 request-logs，无需打开官方页面）
- **累计用量**：花费、请求数、输入/输出 tokens、缓存读取/写入、**缓存命中率**、Token 结构占比
- **每日趋势**：按天的花费 / 请求数 / tokens 柱状图（7/14/30/90 天 / 全部）
- **模型用量**：按模型分组的请求、输入/输出、缓存读取、命中率、花费及占比，表头点击排序
- **支持的模型**：`/v1/models` 的全部可用模型（Provider、上下文/输出上限、输入模态、思考档位），支持搜索、Provider 过滤、表头排序，标记 `default` 别名与已用模型；每个模型右侧 **`⧉ 复制` 一键复制完整模型名**（`provider/id`，可直接粘到 Claude Code 的 `ANTHROPIC_MODEL` 或客户端的 `model` 字段）
- **桥接进程**：来自 `/metrics` 的运行时长、在飞流式、内存会话、上游 tokens、工具调用、限流/错误计数

页面本身是静态 HTML（编译进二进制，不含数据、免鉴权）；浏览器请求 `/v1/usage`、
`/metrics` 时自动带上你在页面里填的 API Key（只存 localStorage，也可用
`.../ui?key=sk-...` 传一次）。未设置 `BRIDGE_API_KEY` 时无需填写。

## 环境变量

> 优先级：**真实环境变量 > 配置文件（`janus.env`）> 内置默认值**。
> 配置文件定位：`JANUS_CONFIG`（兼容旧名 `BRIDGE_CONFIG`）> 可执行文件同目录 `janus.env` > 当前目录 `janus.env`。

### 服务与鉴权

| 变量 | 默认 | 说明 |
|---|---|---|
| `JANUS_CONFIG` / `BRIDGE_CONFIG` | 空 | 显式指定配置文件路径 |
| `BRIDGE_ADDR` | `0.0.0.0:2810` | 监听地址。`127.0.0.1:2810` 仅本机 |
| `BRIDGE_API_KEY` | 空 | 客户端鉴权 Key。留空=接受任意 Key 并打 WARN；**绑非回环必须设置** |
| `BRIDGE_CORS_ORIGIN` | 空 | 允许的浏览器来源。空=不发 CORS 头（拒绝跨源）；`*`=任意源；`a,b`=白名单。`/`、`/ui`、`/healthz` 属公开静态路径，不受限 |
| `BRIDGE_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

### 上游 OpenCode

| 变量 | 默认 | 说明 |
|---|---|---|
| `OPENCODE_URL` | `auto` | `auto`=自动发现/自动拉起（推荐）；或固定 `http://host:port`（此时必须配密码） |
| `OPENCODE_AUTOSTART` | `true` | `auto` 且没找到在跑的 OpenCode 时，由本桥以随机端口拉起一个；`false`=只发现不拉起 |
| `OPENCODE_REUSE_EXTERNAL` | `true` | 是否复用已在跑的外部 OpenCode。`false`=总是自己拉起（这样才能注入 janus 自动生成的 agent 配置） |
| `OPENCODE_BIN` | 空 | 显式指定 `opencode` 可执行文件；空=自动查找（PATH、`~/.opencode/bin/opencode`） |
| `OPENCODE_USERNAME` | `opencode` | 上游 Basic 用户名 |
| `OPENCODE_PASSWORD` | 空 | 上游密码（优先） |
| `OPENCODE_SERVER_PASSWORD` | 空 | 上游密码别名（与 `OPENCODE_PASSWORD` 二选一） |
| `OPENCODE_DB` | 见说明 | 凭据库路径（读余额用）。默认 `$XDG_DATA_HOME/opencode/opencode.db`，再退到 `~/.local/share/opencode/opencode.db` |
| `XDG_DATA_HOME` | 空 | 影响 `OPENCODE_DB` 的默认定位 |
| `OPENCODE_CONSOLE` | `https://opencode.ai/console/api` | console API 基址 |
| `OPENCODE_GO_USAGE` | `https://opencode.ai/inference/go/v1/usage` | Go 套餐限额端点（非公开接口）；失败自动忽略 |

### 会话与 Agent

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_DIRECTORY` | 桥启动时的工作目录 | 会话默认工作目录（OpenCode 项目路径）。远程 + mode B 时客户端项目路径在 janus 上不存在，会自动改用 `BRIDGE_WORKSPACES_DIR/<scope>` |
| `BRIDGE_SCOPE_KEY` | `false` | 客户端不给会话 id 时，按 `scope = IDE + 项目` 定位会话：**同 IDE + 同项目（跨设备）共享**、不同项目/IDE 隔离。见「跨设备共享」 |
| `BRIDGE_PROJECT` | 空 | 显式项目名（优先于自动识别）。适合「一台 janus 只服务一个项目」 |
| `BRIDGE_PROJECT_MAP` | 空 | 设备路径→项目名映射（逗号分隔，如 `/opt/tvfusion=tvfusion,D:\project\tvfusion=tvfusion`）；跨设备路径不同时用 |
| `BRIDGE_WORKSPACES_DIR` | `/var/lib/janus/workspaces` | 远程时 per-scope 的中性工作目录根（客户端项目路径在 janus 上不存在时用它，避免误认项目 / 上游 500） |
| `BRIDGE_DEFAULT_MODEL` | 空 | `default`/`auto`/空别名使用的模型（如 `opencode-go/gpt-6-luna`）；**留空=跟随上游默认**。客户端显式传的 `model` 始终透传，不受此影响。注意：上游默认若是 `opencode/*` 免费模型，经 API 调用会 403（免费额度只能在 OpenCode 内用），需要支持"不带 model"的请求就显式填一个可用的 |
| `BRIDGE_MODEL_MAP` | 空 | 模型别名映射（逗号分隔 `from=to`，键不区分大小写，键以 `*` 结尾为前缀通配）。主要给 Claude Code：**只需映射 opus/sonnet/haiku 三个档位**，如 `claude-opus*=opencode-go/deepseek-v4-pro,claude-haiku*=opencode-go/glm-5.3-flash`。想省事就用 `BRIDGE_DEFAULT_MODEL` 一个开关；用 `janus models` 查当前映射 |
| `BRIDGE_WEBSEARCH_ENABLED` | `true` | 是否支持 Claude Code 的 `web_search` 服务端工具（由 Janus 内部调用上游 `/api/websearch` 执行） |
| `BRIDGE_AGENT` | `build` | 默认 agent，取值见下 |
| `BRIDGE_SESSION_TTL` | `30m` | 会话空闲回收时间（同时删上游 session） |
| `BRIDGE_REQUEST_TIMEOUT` | `600s` | 单次补全总超时 |
| `BRIDGE_MAX_CONVERSATIONS` | `256` | 内存中最大会话数（LRU） |

**`BRIDGE_AGENT` 取值**（填 OpenCode 里真实存在的 agent id）：

| 值 | 说明 |
|---|---|
| `build`（默认） | 完整读写代码的 agent，权限较宽 |
| `plan` | 只读/规划，先规划再动手 |
| `general` | 通用 agent（OpenCode 内置，偏子任务） |
| `explore` | 搜索/阅读代码，不改文件 |
| 自定义 id | 在 OpenCode 配置（`~/.config/opencode/opencode.jsonc` 的 `agents`）里自定义的 agent。janus 的 `orchestrator` 属于此类，但**由 janus 自动生成并注入**（见「工具执行模式」方式 B），**无需手写** |

> 填一个不存在的 id，上游通常不报错，但行为不保证；请用实际存在的 agent。

### 工具执行模式（三选一，重要）

「工具在哪执行」由 `BRIDGE_AGENT` + `BRIDGE_TOOL_CALLING` 两个开关决定，按部署形态选一种：

#### 方式 A：OpenCode 原生执行（工具跑在 janus 主机上）

让 OpenCode agent 用**它自带**的 `read/write/edit/bash` 等工具，在**服务端（janus 所在机器、会话目录下）**执行。客户端只收发文本，**无需声明 tools**。

```ini
BRIDGE_AGENT=build                  # 任意有工具权限的 agent：build / plan / general …
BRIDGE_TOOL_CALLING=false           # 关掉客户端工具桥
BRIDGE_DIRECTORY=/path/to/project   # 必需：原生工具在此目录下执行
```

- 适用：janus 与「要操作的目录」在同一台机器（本机自用）。
- 优点：链路最短，**绕开 MCP 工具桥**，没有 404 / 回填 / 等待超时那一套。
- 注意：工具以 janus 运行用户（常为 root）在 `BRIDGE_DIRECTORY` 下执行。

#### 方式 B：客户端执行（内置 MCP 工具桥）

禁用 OpenCode 自带工具，agent 只调用**客户端在请求里声明的 `tools`**；janus 用内置 MCP server 把它们暴露给 agent，工具实际在**客户端**执行。

```ini
BRIDGE_AGENT=orchestrator    # janus 自动生成的 agent：禁用全部内置工具（见下）
BRIDGE_TOOL_CALLING=true     # 打开工具桥（默认值）
```

`orchestrator` **由 janus 自动生成并注入**（通过 `OPENCODE_CONFIG_CONTENT`，优先级高于全局/项目配置），**不需要手写** `~/.config/opencode/opencode.jsonc`。生成的是**白名单**：

```jsonc
{
  "agents": {
    "orchestrator": {
      "mode": "primary",
      "permissions": [
        { "action": "*",       "resource": "*", "effect": "deny"  },  // 禁掉所有内置工具
        { "action": "execute", "resource": "*", "effect": "allow" },  // Code Mode 必需，否则 MCP 工具全看不到
        { "action": "ob-*",    "resource": "*", "effect": "allow" }   // 只放行 janus 工具桥注册的客户端工具
      ]
    }
  }
}
```

> 为什么用白名单而不是逐个 deny：黑名单会随 OpenCode 版本改工具名而失效（实测 v2 就漏过 `execute`/`search` 之类）。
>
> **前提**：注入只对 **janus 自己拉起的** OpenCode 生效。若本机另有 OpenCode 在跑，janus 默认会复用它、注入不生效 —— 设 `OPENCODE_REUSE_EXTERNAL=false` 让 janus 总是自管上游即可（见配置表）。

- 适用：janus 集中部署，而文件/命令要在**客户端那台机器**上执行（opencode-openai-bridge 的远程模式）。
- 代价：依赖客户端工具桥，可能出现 MCP 404 / 客户端不回填 / 等待超时等；相关参数见「工具调用」一节。

#### 方式 C：不用工具（纯问答）

agent 不做任何读写/命令，只用模型知识回答：

```ini
BRIDGE_AGENT=orchestrator    # janus 自动生成的 orchestrator（无内置工具）
BRIDGE_TOOL_CALLING=false    # 也不暴露客户端工具 → agent 手里没工具
```

- 适用：只想把它当普通聊天/问答用，或安全上不允许它动文件系统。

> 小结：**A = 服务端原生执行**，**B = 客户端桥接执行**，**C = 不执行工具**。

### 输出与事件

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_TOOL_ANNOTATIONS` | `true` | 是否把 agent 工具活动以 `<opencode-tool>` 注释写进 content |
| `BRIDGE_RECONCILE_INTERVAL` | `3s` | 事件流对账间隔 |
| `BRIDGE_IDLE_POLL_INTERVAL` | `1s` | 空闲轮询间隔（终态兜底） |
| `BRIDGE_PROMPT_GRACE` | `2s` | prompt 后多久才信任 idle 信号 |
| `BRIDGE_STREAM_HEARTBEAT` | `15s` | SSE 心跳间隔（长工具任务时保活） |

### 工具调用（把客户端 `tools` 暴露给 agent）

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_TOOL_CALLING` | `true` | 是否启用内置 MCP server 透传客户端 `tools` |
| `BRIDGE_TOOL_SOFT_FAIL` | `false` | 工具注册失败时：`true`=降级为无工具继续；`false`=直接报错 |
| `BRIDGE_TOOL_REREGISTER` | `10m` | 多久主动续注册一次 MCP（上游重启会丢注册） |
| `BRIDGE_TOOL_CALL_WAIT` | `5m` | 工具调用挂起、等客户端回填结果的最长时间（**执行类**工具：`RunCommand`/`execute_command` 等） |
| `BRIDGE_TOOL_CALL_WAIT_FAST` | `90s` | **只读/编辑类**工具的短等待。这类工具正常秒回，卡住基本是客户端卡死，快速判失败能让模型继续，而不是干等 5m 撞上 IDE 自身超时。`<=0` 或 `>= BRIDGE_TOOL_CALL_WAIT` 时不启用 |
| `BRIDGE_TOOL_CALL_WAIT_FAST_TOOLS` | `Grep,Read,Glob,LS,WebFetch,Write,SearchReplace,DeleteFile` | 走短等待的工具名（逗号分隔，不区分大小写）。**未列出的工具（含未知新工具）一律走长等待**，避免误杀长任务。设成 `none`/`off`/`-` 表示禁用短等待 |
| `BRIDGE_MCP_URL` | 空 | 注册给 OpenCode 的 MCP 基址（空=本机回环） |
| `BRIDGE_MCP_ALLOW` | 空 | 限制内置 MCP 端点 `/mcp/{token}` 的来源（逗号分隔 IP/CIDR）。空=不限制；对外暴露或上游在别机时建议设为上游网段 |
| `BRIDGE_USER_AGENT` | 空 | 所有**出站**请求的 `User-Agent`。空=内置 `janus/<version> (<os>/<arch>; +https://github.com/qist/janus)`。Go 默认的 `Go-http-client/1.1` 易被网关/风控当脚本拦（403/429）；需要时也可覆盖成浏览器式 UA |
| `BRIDGE_PERMISSION_REPLY` | `once` | 自动应答权限请求：`once`（仅本次）/ `always`（记住）/ `reject`（拒绝）/ `off`（不干预） |

> **结果回填**：客户端用 `role:"tool"` 消息回填结果，按 `tool_call_id` 对应。部分客户端（实测 Trae）会用**自己生成的** id（而非 janus 下发的 `call_`+hex），桥会按「**精确 id → 工具名 → 顺序**」三级对齐，并且只匹配**当前这一轮**的结果（不回放旧结果）。
> **注册生命周期**：MCP server 在**会话存活期内保持注册**（注销发生在：会话重置 / 客户端不再声明 tools / TTL）。不每轮注销，是因为 OpenCode 的 agent 会话**不会在重新注册后刷新工具目录**，会导致下一轮「Code Mode 目录为空」。

### Responses API / 用量

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_RESPONSES_ENABLED` | `true` | 是否开放 `/v1/responses` |
| `BRIDGE_RESPONSE_TTL` | `30m` | 已保存响应的保留时长（`previous_response_id` 依赖） |
| `BRIDGE_ANTHROPIC_ENABLED` | `true` | 是否开放 `/v1/messages`（Anthropic Messages API，供 Claude Code） |
| `BRIDGE_MODEL_ECHO` | `real` | 响应 `model` 字段回显什么：`real`=解析后的真实模型（如 `opencode-go/mimo-v2.5-pro`）；`request`=客户端请求里的原始名（如 `claude-sonnet-4-5`） |
| `BRIDGE_DB` | 默认 `$XDG_DATA_HOME/janus/janus.db` | 持久化库路径（SQLite）。不设=默认路径；`memory`/`off`=纯内存。开启后**响应、会话映射与 Chat 历史快照**都落盘，Chat / Responses 均可跨进程重启续接 |
| `BRIDGE_HISTORY_MAX_BYTES` | `1048576` | 落库的 Chat 历史快照上限（字节）；超过只存会话映射。0=不限 |
| `BRIDGE_CONV_TTL` | `168h` | 持久化的会话映射/历史保留时长（janitor 清理） |
| `BRIDGE_USAGE_ENABLED` | `true` | 是否开放 `/v1/usage` |
| `BRIDGE_USAGE_TTL` | `30s` | 用量报告缓存时长（避免频繁打 console API） |

### 限流 / 指标

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_RATE_LIMIT` | `0`（不限） | 每 key/IP 每分钟请求数 |
| `BRIDGE_RATE_BURST` | `0`（同 limit） | 突发容量（桶大小） |
| `BRIDGE_RATE_LIMIT_GLOBAL` | `0`（不限） | 全局每分钟上限（跨所有 key/IP） |
| `BRIDGE_TRUST_PROXY` | `false` | 是否信任 `X-Forwarded-For` / `X-Real-IP` 识别客户端 IP |
| `BRIDGE_METRICS_PUBLIC` | `false` | `/metrics` 是否免鉴权 |


## 已实现端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 探活（含上游连通性），无需鉴权 |
| GET | `/ui` | 内置 Web 用量面板（页面免鉴权，数据仍要 API key） |
| GET | `/` | 302 跳转到 `/ui` |
| GET | `/v1/models` | 模型列表（支持过滤，见下） |
| GET | `/v1/models/{id}` | 单个模型**完整详情**（上下文、思考档位、能力、价格） |
| GET | `/v1/usage` | 账号余额 + 用量（扩展端点，见下） |
| GET | `/v1/requests` | 最近逐条请求日志（含每次缓存命中/未命中/命中率；扩展端点，`?since=<ms>&limit<=100`） |
| GET | `/metrics` | Prometheus 指标（默认需鉴权） |
| POST | `/v1/chat/completions` | 流式 + 非流式 |
| POST | `/v1/responses` | OpenAI **Responses API**（流式 + 非流式） |
| GET | `/v1/responses/{id}` | 取回已保存的响应 |
| DELETE | `/v1/responses/{id}` | 删除响应 |
| POST | `/v1/messages` | **Anthropic Messages API**（供 Claude Code / Anthropic SDK，流式 + 工具） |
| POST | `/v1/messages/count_tokens` | Anthropic token 估算 |
| GET | `/anthropic/v1/models` | **Anthropic 格式**模型列表（Claude Code 模型选择器；`/v1/models` + `anthropic-version` 头同样返回） |
| POST | `/v1/completions` | 旧版补全，内部降级为单轮 chat |
| * | `/v1/*` | 其余一律返回 OpenAI 格式 404（客户端不会因解析失败而崩） |

### 使用 Anthropic / Claude Code

Janus 同时兼容 **Anthropic Messages API**，Claude Code / Anthropic SDK 可直接指向它。
接口同时挂在两个前缀下，任选（对齐 DeepSeek 等厂商的 `/anthropic` 约定）：

```bash
# 方式一：根路径
export ANTHROPIC_BASE_URL=http://127.0.0.1:2810
# 方式二：/anthropic 前缀（与 DeepSeek 一致）
export ANTHROPIC_BASE_URL=http://127.0.0.1:2810/anthropic

export ANTHROPIC_AUTH_TOKEN=sk-bridge-dev     # 也支持 ANTHROPIC_API_KEY（x-api-key）

# 把 CC 的各类模型都指向 Janus（发出去的 model 名保持 claude-*，由服务端映射）
export ANTHROPIC_MODEL=claude-sonnet-4-5
export ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-1
export ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5
export ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-3-5-haiku
```

- `POST /v1/messages`（或 `/anthropic/v1/messages`）：`system` + `messages`（text / image / tool_use / tool_result）、`tools[].input_schema`
- 流式：`message_start → content_block_start/delta/stop → message_delta → message_stop`（工具块用 `input_json_delta`）
- 工具：Anthropic `tool_use`（`id/name/input`）↔ 客户端声明的 tools；`tool_result` 回填给 agent
- 鉴权：`x-api-key`（也接受 `Authorization: Bearer`）；`anthropic-version` / `anthropic-beta` 忽略
- **模型路由（重要）**：CC 只会用少数几个**档位名**（`claude-opus*` / `claude-sonnet*` / `claude-haiku*`），
  所以**你不需要逐个映射模型**。请求按下面的优先级解析：

  1. `BRIDGE_MODEL_MAP` 显式映射（支持前缀通配 `*` 结尾）
  2. 请求的 `model` 本身能解析（例如客户端直接填 `opencode-go/xxx`）
  3. `BRIDGE_DEFAULT_MODEL` —— **多数用户只需配这一个**，所有档位都走它
  4. 都没有时，按档位启发式自动挑：opus 挑强的、haiku 挑便宜快的
- **怎么从一堆订阅模型里挑那一个**：`janus models` 列出上游全部可用模型
  （价格 / 上下文 / 能力，并标注哪些经 API 会 403），末尾直接给出三个档位的当前映射结果，
  复制一行填进 `BRIDGE_DEFAULT_MODEL` 即可：

  ```bash
  janus models            # 表格
  janus models --json     # 给脚本/二次处理
  ```

- **只用一个模型（最常见）**：

  ```bash
  BRIDGE_DEFAULT_MODEL=opencode-go/gpt-6-luna
  ```

- **想分档位**（例如 opus 用强模型、haiku 用便宜快的）—— 只需映射 3 个档位：

  ```bash
  BRIDGE_MODEL_MAP=claude-opus*=opencode-go/deepseek-v4-pro,claude-sonnet*=opencode-go/deepseek-v4.1-flash,claude-haiku*=opencode-go/glm-5.3-flash
  ```

  （正是 DeepSeek 的 `claude-opus* → 强模型`、`claude-sonnet*/haiku* → 快模型` 那套；用户只改 base_url + key 即可）
- **模型列表（Anthropic 格式）**：`GET /anthropic/v1/models`（或带 `anthropic-version` 头访问 `/v1/models`）
  返回 Claude Code 模型选择器认的格式，`display_name` 里带上“实际映射到谁”，方便核对
- **Web Search（服务端工具）**：CC 声明的 `web_search` 由 Janus **内部执行**（调用上游 OpenCode 的
  `/api/websearch`），结果回喂给 agent，不会作为 `tool_use` 甩回 CC；`BRIDGE_WEBSEARCH_ENABLED=false` 可关闭。
  OpenAI **Responses API** 的 `{"type":"web_search"}` 同样支持（同一实现）
- **会话锚点**：Claude Code 的 `x-claude-code-session-id` 头会自动作为会话键，不同 CC 会话天然隔离
- **配完整模型名（DeepSeek 式）**：CC 也可以直接把 `ANTHROPIC_MODEL` 设成真实模型，如
  `opencode-go/deepseek-v4.1-flash`。CC 若用 `mimo-v2.5-pro[1m]` 这种**长上下文标记**，
  官方会在发送前自动去掉 `[1m]`（只影响客户端选型/beta 头）；Janus 也兼容"未去掉"的情况，
  收到的 `mimo-v2.5-pro[1m]` 会兜底 strip 成 `mimo-v2.5-pro` 再解析。裸模型名要求在上游唯一，
  有歧义时带上 `provider/` 前缀。排查时用 `BRIDGE_LOG_LEVEL=debug`，日志会打
  `anthropic request: client model="…" → resolved=…`

### 查看模型

**列表**（轻量，给客户端下拉框用）：

```bash
curl -s http://127.0.0.1:2810/v1/models -H "Authorization: Bearer sk-your-key"
```

```jsonc
{"object":"list","data":[
  {"id":"default","object":"model","created":1790812800,"owned_by":"opencode",
   "context_length":1048576,"max_output_tokens":131072,
   "supported_reasoning_efforts":["low","high","max"],
   "input_modalities":["text","image"]},
  {"id":"opencode/claude-sonnet-5-5","object":"model","created":1790553600,"owned_by":"opencode",
   "context_length":1000000,"max_output_tokens":128000,
   "supported_reasoning_efforts":["low","medium","high","xhigh","max"],
   "input_modalities":["text","image","pdf"]}
]}
```

支持过滤（脚本 / 模型选择器很好用）：

```bash
curl -s '.../v1/models?provider=opencode-go'        # 只看某个 provider
curl -s '.../v1/models?tools=true&min_context=200000' # 支持工具调用且上下文 >= 20 万
curl -s '.../v1/models?modality=image'              # 支持图片输入
curl -s '.../v1/models?refresh=1'                   # 强制刷新上游缓存
```

**详情**（挑模型时看这个）：

```bash
curl -s http://127.0.0.1:2810/v1/models/opencode/claude-sonnet-5-5 -H "Authorization: Bearer sk-your-key"
```

```jsonc
{
  "id": "opencode/claude-sonnet-5-5",
  "object": "model", "created": 1790553600, "owned_by": "opencode",
  "name": "Claude Sonnet 5.5",
  "family": "claude-sonnet",
  "status": "active",
  "enabled": true,
  "tool_calling": true,
  "context_length": 1000000,
  "max_output_tokens": 128000,
  "input_modalities": ["text", "image", "pdf"],
  "supported_reasoning_efforts": ["low", "medium", "high", "xhigh", "max"],
  "capabilities": { "tools": true, "input": ["text","image","pdf"], "output": ["text"] },
  "variants": [                                  // 思考档位及其底层 provider 设置
    {"id": "low",  "settings": {"reasoningEffort": "low"}},
    {"id": "high", "settings": {"reasoningEffort": "high"}},
    {"id": "max",  "settings": {"reasoningEffort": "max"}}
  ],
  "pricing": [                                   // USD / 百万 token
    {"input_per_million": 2, "output_per_million": 10,
     "cache_read_per_million": 0.2, "cache_write_per_million": 2.5}
  ]
}
```

`/v1/models/default` 也支持，返回上游当前默认模型的详情。

> 想看上游原始数据：`GET <upstream>/api/model?directory=<dir>`（需上游 Basic 认证）。
> 桥的详情接口是它的 OpenAI 化视图。

### 虚拟模型与默认模型选择（`janus`）

`/v1/models` 里有两个**虚拟模型**，客户端下拉框里可直接选：

| 虚拟模型 | 含义 |
|---|---|
| **`janus`** | **面板里选定的默认模型**（`/ui` →「模型」→ 设为 janus）。**推荐 IDE 填这个** |
| `default` | 上游 / `BRIDGE_DEFAULT_MODEL` 的默认（旧语义） |

**换模型不用改 env、不用改客户端**：

1. 打开 `/ui` →「模型」标签页；
2. 找到目标模型行，点「**设为 janus**」（或直接在该行「思考档位」点一个档位）；
3. IDE 里模型名**固定填 `janus`**。

解析优先级：**面板选择（存 DB） > `BRIDGE_DEFAULT_MODEL` > 上游默认**。

- **透传仍保留**：IDE 填真实 `provider/id`（如 `opencode-go/glm-5.3-flash`）就原样使用，不受面板设置影响。
- **档位**：面板「思考档位」按钮组选的档位会随默认模型一起下发（存成 `default_model` 的 `:variant` 后缀，如 `opencode-go/glm-5.3-flash:high`）——适合**无法传 `reasoning_effort` 的自定义模型客户端**。
- **切换时机**：面板改完，**下一条请求**就原地切换（`POST /api/session/{id}/model`），**不重开会话、上下文保留**，无需等会话结束。

接口：`GET /v1/settings` 读；`POST /v1/settings {"default_model":"provider/id[:variant]"}` 写（空字符串=清除，回落配置）。

## 跨设备共享（scope）

客户端大多不给「会话 id」（Trae / Copilot / CodeBuddy 都不给），Janus 就退化为按 **`scope = IDE + 项目`** 定位会话。开启：

```ini
BRIDGE_SCOPE_KEY=true
```

- **同 IDE + 同项目（跨设备）→ 同一会话**（共享）；**不同项目 / 不同 IDE → 隔离**。
- **项目识别**取自各客户端 firstUser 的权威字段：

  | 客户端 | 来源 |
  |---|---|
  | Trae | `Primary working directory: <path>` |
  | CodeBuddy | `Workspace Folder: <path>` |
  | GitHub Copilot Chat | `following folders: - <path>` |
  | 兜底 | 消息里最频繁的路径前缀（排除系统目录） |

  可用 `BRIDGE_PROJECT`（固定项目名）或 `BRIDGE_PROJECT_MAP`（设备路径→项目名）覆盖。
- **IDE** 从 `User-Agent` 归一（`trae` / `codebuddy` / `githubcopilotchat`），**剥掉版本号**，升级不换 scope。
- **会话工作目录**：远程 + mode B 时客户端项目路径在 janus 主机上不存在，用 `BRIDGE_WORKSPACES_DIR/<scope>`（janus 创建的空目录），避免 agent 误认成 `BRIDGE_DIRECTORY` 那个项目、也避免上游对不存在目录注册 MCP 报 500。

> 这不是「可靠的会话识别」，而是**没有会话 id 时的明确退化**：宁可退化到「IDE+项目」级上下文，也不做隐式猜测。未来某 IDE 开放真实 session id，第一优先级会自动用它。

远程部署时把 `BRIDGE_ADDR` 设为 `0.0.0.0:2810`，其它设备把 IDE 的 Base URL 指到 `http://<janus-ip>:2810/v1`（Key 填 `BRIDGE_API_KEY`）。

## 核心机制

### Responses API（`/v1/responses`）

和 Chat Completions **共用同一套会话/执行/工具内核**，只是输入输出换成 Responses 形状：

```bash
curl -s http://127.0.0.1:2810/v1/responses \
  -H "Authorization: Bearer sk-your-key" -H "Content-Type: application/json" \
  -d '{"model":"default","input":"打个招呼","store":true}'
```

```jsonc
{
  "id": "resp_666b7a28b436ccffd61a8fb21e1298f9",
  "object": "response",
  "status": "completed",
  "output_text": "你好呀",
  "output": [
    {"type":"reasoning","id":"rs_…","summary":[{"type":"summary_text","text":"…"}]},
    {"type":"message","id":"msg_…","role":"assistant","status":"completed",
     "content":[{"type":"output_text","text":"你好呀","annotations":[]}]}
  ],
  "usage": {"input_tokens":43,"output_tokens":3,"total_tokens":46,
            "input_tokens_details":{"cached_tokens":4864},
            "output_tokens_details":{"reasoning_tokens":16}}
}
```

**支持**：

| 能力 | 说明 |
|---|---|
| `input` | 字符串，或 items 数组（`message` / `function_call` / `function_call_output`） |
| `instructions` | 作为 system 提示 |
| `stream` | SSE 事件流：`response.created` → `response.output_text.delta` / `response.reasoning_summary_text.delta` → `response.completed` |
| `previous_response_id` | **接着上一轮的会话继续**，此时 `input` 只放新增内容 |
| `tools` | 扁平定义 `{type:"function",name,description,parameters}`，走同一套 MCP 工具闭环 |
| `reasoning.effort` | 映射到上游思考强度档位 |
| `store` | 是否保存响应（默认保存，`BRIDGE_RESPONSE_TTL` 后回收） |
| `GET` / `DELETE` | 取回 / 删除已保存的响应 |

实测 `previous_response_id` 续链：

```
第一轮  input="记住暗号 RED-7。只答“已记”"                    → resp_9e3b…
第二轮  previous_response_id=resp_9e3b…, input="暗号是什么？"  → "RED-7"   ← 上下文延续
```

`previous_response_id` 的实现方式：把响应链映射到一个**独立的会话键**（`resp:<id>`），
每轮只把新增 `input` 作为 prompt 发出，复用同一个 OpenCode session 保存上下文。
这条路径刻意不走 Chat 的历史前缀匹配（Responses 是无状态客户端 + 有状态服务端的语义）。

配置 `BRIDGE_DB` 后，响应与"会话键 → sessionID"映射会落本地 SQLite
（`github.com/qist/sqlite`），**进程重启后 `previous_response_id` 仍能续上同一个 session**。

### 思考强度（`reasoning_effort`）

OpenCode 的模型可以带 **variant**，实测它就是思考强度档位
（`variant.settings.reasoningEffort = <档位>`）。实测存在的档位：
`none / minimal / low / medium / high / xhigh / max / thinking`。

桥把它对齐到 OpenAI 标准字段 `reasoning_effort`：

```jsonc
POST /v1/chat/completions
{
  "model": "opencode-go/deepseek-v4.1-flash",
  "reasoning_effort": "max",     // none|minimal|low|medium|high|xhigh|max
  "messages": [{"role": "user", "content": "..."}]
}
```

映射规则（`PickVariant`）：

1. 留空 / `default` / `auto` → 不指定，用模型默认档
2. 与某档位名完全一致 → 直接用（所以 `xhigh`、`max` 这类扩展档位也能传）
3. 常见别名归一化：`min`→`minimal`、`mid`→`medium`、`extra-high`→`xhigh`、`maximum`→`max` …
4. 模型只有布尔档位（如 `none`/`thinking`）→ 非 `none` 一律用 `thinking`
5. 否则按强度取最接近的档位（并列取更弱的，更省资源）

也支持直接在模型名后缀指定，优先级更高：

```
"model": "opencode-go/deepseek-v4.1-flash:max"
```

模型有哪些档位，看 `/v1/models` 的 `supported_reasoning_efforts` 字段。

> 切换档位不会重开会话 —— 桥调用 `POST /api/session/{id}/model` 原地切 variant
> （实测切换后 session 的 `model.variant` 正确变化，上下文保留）。

### 上下文与输出上限

`/v1/models` 除了 OpenAI 标准的四个字段，还带上：

```jsonc
{
  "id": "opencode/claude-sonnet-5-5",
  "object": "model", "created": 1790553600, "owned_by": "opencode",
  "context_length": 1000000,                       // 上下文窗口
  "max_output_tokens": 131072,                     // 单次最大输出
  "supported_reasoning_efforts": ["low","high","max"],
  "input_modalities": ["text","image","pdf"]
}
```

这些是各家网关（OpenRouter / LiteLLM / vLLM）的通行扩展字段，
标准客户端会忽略它们，不影响兼容性。

**`max_tokens` / `max_completion_tokens`（尽力映射）**：上游 prompt 接口没有输出
上限参数，agent 的 `request.body` 也只能静态配置。桥侧按标准语义尽力做到：

- 用近似 token 计数（ASCII ≈ 4 字符/token，中文 ≈ 1.3 字符/token）跟踪输出
- 超出上限后停止发送正文，`finish_reason = "length"`
- **并 `interrupt` 上游**，让客户端立刻拿到结果，而不是等模型把整段生成完
- Responses API 对应 `status: "incomplete"` + `incomplete_details.reason = "max_output_tokens"`

实测 `max_tokens=5` → `finish_reason=length`，正文截到 5 个 token 左右。

> 计数是**估算**，不是精确 tokenizer —— 只用于"别超太多"的软限制，
> 不保证精确等于客户端请求的上限。

### 会话亲和（无状态请求 → 有状态 agent 会话）

OpenAI 客户端每次都把**完整历史**发过来，而 OpenCode 的会话是有状态的。
桥在内存里维护 `对话 → OpenCode sessionID` 映射：

```
OpenAI conversation/session          客户端每轮发完整 messages[]
        ↓
Bridge session mapping              key 分桶 + 严格历史前缀匹配
        ↓
OpenCode session  (ses_…)           上下文存在这里，prompt 只发增量
```

实测一条 3 轮对话：

```
turn1  session created ses_efda8286…   delta=47B  →  已记
turn2  同一 session                     delta=57B  →  BLUE-42
turn3  同一 session                     delta=61B  →  BLUE-42
```

**只在第一轮建会话**，后续每轮只发新增部分，上下文由 OpenCode 保留。

- **key**：优先 `X-Session-ID` 请求头；否则 `hash(首条 system + OpenAI 的 user 字段 + directory)`
- **同一个 key 下分桶**：key 只是分桶依据，不是唯一会话
- **严格前缀匹配**：在桶里找「保存的历史是本次请求前缀」的会话
  - 命中 → 只把新增部分（delta）发给上游
  - 没命中（新话题 / 历史被改写 / 被截断）→ 另起一个会话，**绝不污染旧上下文**

这样带来的效果（实测）：

| 场景 | 行为 |
|---|---|
| 多轮追问 | 复用会话，只发增量（例：第二轮只发 66 字节而非全文） |
| 同一 key 下两个不相关话题交替发 | 各占一个会话，各记各的（甲记得红、乙记得黄） |
| 客户端改写历史 | 另起会话 |
| 客户端截断历史 | 另起会话 |
| 完全相同的请求重发 | 直接返回缓存结果（毫秒级） |

> 客户端不需要带 system 提示词。带了会更好（分桶更准），不带也不会串。

**隔离保证**（均有测试）：并发首请求、同分桶不同内容 → 各自独立会话；同话题多轮 → 续接同一会话。
配置 `BRIDGE_DB` 后，会话映射与 Chat 历史快照落本地 SQLite，**重启后 Chat 也按前缀命中续接同一会话**。

**Responses 也走同一套内核**：`previous_response_id` 直接复用上一条响应所属的 Janus 会话
（也就是同一个 OpenCode session），每轮只发新增 `input`；Chat 与 Responses 只是键空间不同
（`x:` / `f:` / `resp:`），底层 Store / executor / ToolBridge 共用。

**已知限制**：既没有 `X-Session-ID`/`X-OpenCode-Session` 头、也没有 `user` 字段，且两个 client
的首条历史**完全相同**时，会被当作同一条会话线（这是"多轮自动续接"所依赖的匹配）。要强隔离，
请让客户端带 `user` 或显式会话头。

### 并行工具调用（`parallel_tool_calls`）

OpenAI 默认允许一轮返回多个 `tool_calls`。Janus 默认（缺省 / `true`）把 agent 在同一轮里
发起的多个工具调用**一次性**回给客户端（流式 `index` 0/1/2…，`tool_call_id` 原样保留）。

请求里带 `"parallel_tool_calls": false` 时，桥改为**串行**：一轮只回一个 `tool_call`，
客户端回填结果后再回下一个（agent 已并行发起的调用会按到达顺序逐个释放），全部回填完
才让 agent 继续。适合只支持单个工具调用的客户端。Chat Completions 与 Responses 都支持；
Responses 响应里会回显 `parallel_tool_calls`。

### 流式转换

驱动一次执行需要同时消费 4 类信号，缺一不可：

1. **SSE 事件**（`/api/event`）——低延迟正文/推理增量
2. **空闲轮询**（`GET /api/session/{id}` → `time.idle`）——最可靠的终态
3. **`/wait`**——阻塞到 agent loop 空闲，作为兜底
4. **定期对账**（`GET /api/session/{id}/message`）——补齐丢失的尾巴

事件映射：

| OpenCode 事件 | 转成 |
|---|---|
| `session.text.delta` | `choices[0].delta.content` |
| `session.reasoning.delta` | `choices[0].delta.reasoning_content` |
| `session.tool.input.started/ended` | 注入 `<opencode-tool>…</opencode-tool>` 注释（可关） |
| `session.step.ended` | `finish_reason` + token 累计 |
| `session.execution.succeeded/failed/interrupted` | 结束 |

`finish` 映射：`stop→stop`、`length→length`、`tool-calls→tool_calls`、`content-filter→content_filter`。

### 工具注释与上下文卫生

agent 干活时（读文件、跑命令）会沉默很久。开启 `BRIDGE_TOOL_ANNOTATIONS`
后，工具活动以注释形式插进 content，客户端能看到进度：

```
<opencode-tool> bash: ls -la /path/to/project</opencode-tool>
```

这些注释在**重放历史时会被自动剥掉**，不会污染上游上下文。若客户端自己做
agent 循环、不希望 content 里混入这些标记，设 `BRIDGE_TOOL_ANNOTATIONS=false`。

### 图片与附件

OpenAI 的 `content` 数组形态（`image_url`）已支持，Chat 与 Responses 都走同一套：

- `data:image/...;base64,...` → 直接作为附件传给上游
- `http(s)://...` → 先下载再转 base64（上游**只认 data URI**，实测 https 会 400）
- 超过 12MB 或下载失败 → 降级，并在 prompt 里用文字说明"有张图看不到"，
  避免模型自信地回答"未看到图片"
- **按模型能力门控**：查模型的 `capabilities.input`，支持 `image`/`pdf` 才附带；
  纯文本模型收到图片会丢弃并说明（`模型支持就接收`）
- Responses 的 `input` 里 `input_image`（`image_url` 字符串或 `{url}` 两种形态）已解析

### 指标（`/metrics`）

Prometheus 文本格式，零依赖：

```bash
curl -s http://127.0.0.1:2810/metrics -H "Authorization: Bearer sk-your-key"
```

| 指标 | 说明 |
|---|---|
| `opencode_bridge_requests_total{endpoint,status}` | 请求量 |
| `opencode_bridge_request_duration_seconds{endpoint}` | 延迟直方图 |
| `opencode_bridge_rate_limited_total` | 被限流次数 |
| `opencode_bridge_upstream_errors_total` | 5xx（上游错误）次数 |
| `opencode_bridge_tool_registrations_total` / `_tool_calls_total` | 工具注册 / 工具调用 |
| `opencode_bridge_active_streams` | 在飞 SSE 数 |
| `opencode_bridge_conversations` | 内存中的会话数 |
| `opencode_bridge_tokens_total{direction}` | 上游上报的 token 数 |

默认需要 API key；`BRIDGE_METRICS_PUBLIC=true` 可放开给 Prometheus 直接抓。

## 测试

```bash
# Go 单测（含 -race）
CGO_ENABLED=1 go test -race ./...

# 端到端：用 OpenAI 官方 SDK 打真实 bridge
python3 -m venv .venv && .venv/bin/pip install openai
BRIDGE_API_KEY=sk-bridge-dev .venv/bin/python tests/compat_test.py

# 兼容性矩阵（重点：Tool Calling + Responses 事件）
# 需指定一个可用模型（免费额度模型经 API 会 403）
make compat BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max
# 或分组：python3 tests/compat_matrix.py tools|responses
```

`compat_test.py` 覆盖 22 项断言：模型列表、流式/非流式、`include_usage`、
多轮上下文、`reasoning_content`、错误语义（404/400/401）、图片、并发隔离。

`compat_matrix.py` 是**兼容性矩阵**，覆盖最容易出兼容问题的工具与事件：
- 工具调用：单个 / 多个并行 / 工具报错 / 超大结果 / 流式协议字段 / 带 reasoning
- Responses：SSE 事件序列（`response.created` → `output_text.delta` → `completed`）、
  `function_call_arguments.delta/done`、`output_item.added/done`、`previous_response_id` 续链
- 并发：8 客户端不串会话

结果（本机实测）：**25 项断言全过**。每个 Chat 用例都带唯一 `user`，本身也是"会话隔离"的验证。


## 安全边界（重要）

Janus 背后是一个**能执行 shell、读写文件**的 agent（默认以启动用户身份运行，常见是 root）。把它暴露出去 ≈ 把 shell 交出去。要点：

- **默认只监听回环**（`BRIDGE_ADDR=127.0.0.1:2810`）。要远程访问，优先走 SSH 隧道 / VPN / Tailscale，而不是直接 `0.0.0.0`。
- **必须设 `BRIDGE_API_KEY`**；绑定非回环且 key 为空时任何人都能消耗你的额度。
- 桥自身**不做 TLS**，公网/不可信网络务必套反向代理（Caddy/nginx）加 HTTPS。
- **内置 MCP 端点** `/mcp/{token}` 用 128 位随机 token 鉴权，随会话短命（**会话重置 / 客户端不再声明 tools / TTL** 时注销，不再每轮注销）；仍可用 `BRIDGE_MCP_ALLOW` 按来源 IP/CIDR 再收一层。防串会话：空闲会话的工具调用会被立即拒绝。
- **权限自动应答** `BRIDGE_PERMISSION_REPLY`：`once`（默认）只放行单次，`always` 会写入 OpenCode 的持久权限，`reject` 更保守。
- 想要"agent 不碰本机、工具在客户端执行"，用 `orchestrator` agent（由 janus 自动生成并注入，见「工具执行模式」方式 B）。

## 已知限制

- **上游必须是 OpenCode**，本桥不做通用 OpenAI 代理。
- 会话映射在**内存**里：bridge 重启后，客户端下一次请求会被当作新会话
  （客户端仍能拿到正确回答，只是上下文要重新积累一轮）。
- 分桶上限 256 个会话 + 单 key 8 个候选，LRU 淘汰，被淘汰的上游会话由
  janitor 异步删除。
- 图片仅支持 data URI 与可下载的 http(s)，不支持 `file://` 等本地路径。
- 客户端自带工具（`tools` 参数）目前不转发给上游，属 Phase 3。
- 自动发现依赖 Linux `/proc` 与同用户权限。

## 目录结构

```
janus/
├── go.mod
├── Makefile
├── janus.env.example      配置模板（复制成 janus.env）
├── README.md
├── cmd/                    （预留：多二进制/入口时用）
│
├── web/
│   └── ui.html            用量面板页面（go:embed，零依赖离线可用）
│
├── docs/
│   ├── DESIGN.md          设计文档（含调研过程）
│   ├── COMPAT.md          兼容性实测记录
│   └── AUDIT.md           13 维审计报告
│
├── scripts/
│   └── run.sh             一键启动
│
├── tests/
│   └── compat_test.py     端到端兼容矩阵（OpenAI 官方 SDK）
│
└── *.go                   Go 源码（单二进制，package main 位于仓库根，符合 Go 约定）
    ├── main.go            启动、janitor、优雅退出
    ├── config.go          环境变量/配置文件
    ├── server.go          路由、鉴权、CORS、公开路径
    ├── chat.go            /v1/chat/completions 主流程 + executor
    ├── complete.go        流式/非流式收尾
    ├── sse.go             OpenAI chunk 写出 + 错误映射
    ├── store.go           会话分桶 + 历史差分
    ├── flatten.go         messages → prompt 文本
    ├── attach.go          图片附件提取/下载
    ├── models.go          模型缓存、命名解析、免费判定
    ├── upstream.go        OpenCode 客户端（endpoint 热替换）
    ├── eventbus.go        单条 SSE 连接 + fan-out + 权限自动应答
    ├── discover.go        上游自动发现 + 端点热切换
    ├── autostart.go       没找到在跑的 OpenCode 时自动拉起（随机端口 + 生成的密码）
    ├── mcp.go             内置 MCP server（暴露客户端 tools）
    ├── toolbridge.go      工具注册表 / 挂起调用 / 防串会话
    ├── toolflow.go        工具调用编排（注册/回填/恢复/清理）
    ├── responses.go       Responses API（输入输出转换 + 响应存储）
    ├── responses_stream.go Responses 的 SSE 事件流
    ├── ui.go              用量面板 HTTP 处理（go:embed web/ui.html）
    └── *_test.go          单元测试
```

> 单二进制 Go 项目遵循惯例把 `package main` 放在仓库根；`web/`、`docs/`、
> `scripts/`、`tests/` 已分层。若以后要拆成多个二进制/库，再引入 `cmd/` +
> `internal/`（注意 `go:embed` 不能引用上级目录，嵌入式页面需随包同目录）。

## 许可证

MIT © 2026 qist，详见 [LICENSE](LICENSE)。

