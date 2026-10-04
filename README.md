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
make vet            # go vet
make dist-linux     # Linux 全架构发布包：amd64 / arm64 / arm / 386
```

- 版本号来源：`make VERSION=...` > `git describe --tags` > `VERSION` 文件 > `dev`
- Linux 发布包为**纯静态**二进制（`CGO_ENABLED=0`），产物在 `dist/janus_<version>_linux_<arch>.tar.gz`
- 推 `v*` tag 会触发 GitHub Actions（`.github/workflows/release.yml`）自动出 Release + 全架构附件；
  普通 push / PR 走 `.github/workflows/ci.yml`（vet + test + build + `--version`）

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

## 自动发现上游（重要）

OpenCode 桌面端/Hub **每次重启都会换随机端口和随机密码**，写死配置撑不过一次重启。
因此默认 `OPENCODE_URL=auto`，启动时和断连时都会：

1. 扫 `/proc/*/cmdline` 找 `opencode ... serve --port N` 的进程
2. 从 `/proc/<pid>/environ` 读 `OPENCODE_SERVER_PASSWORD`
3. 用 Basic 认证逐个探活 `/api/info`，第一个通的即采用
4. 断连时后台每 30s 重试一次，探到新端点就热替换（含 SSE 重连）

若要指向固定/远程实例，显式设置 `OPENCODE_URL` 即可（此时需同时给 `OPENCODE_PASSWORD`）。

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

金额单位是 **micro-cents**（1 USD = 1e8 micro-cents），桥已换算成 `*_usd`。

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
- **累计用量**：花费、请求数、输入/输出 tokens、缓存读取/写入、**缓存命中率**、Token 结构占比
- **每日趋势**：按天的花费 / 请求数 / tokens 柱状图（7/14/30/90 天 / 全部）
- **模型用量**：按模型分组的请求、输入/输出、缓存读取、命中率、花费及占比，表头点击排序
- **支持的模型**：`/v1/models` 的全部可用模型（Provider、上下文/输出上限、输入模态、思考档位），支持搜索、Provider 过滤、表头排序，标记 `default` 别名与已用模型
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
| `OPENCODE_URL` | `auto` | `auto`=自动发现（推荐）；或固定 `http://host:port`（此时必须配密码） |
| `OPENCODE_USERNAME` | `opencode` | 上游 Basic 用户名 |
| `OPENCODE_PASSWORD` | 空 | 上游密码（优先） |
| `OPENCODE_SERVER_PASSWORD` | 空 | 上游密码别名（与 `OPENCODE_PASSWORD` 二选一） |
| `OPENCODE_DB` | 见说明 | 凭据库路径（读余额用）。默认 `$XDG_DATA_HOME/opencode/opencode.db`，再退到 `~/.local/share/opencode/opencode.db` |
| `XDG_DATA_HOME` | 空 | 影响 `OPENCODE_DB` 的默认定位 |
| `OPENCODE_CONSOLE` | `https://opencode.ai/console/api` | console API 基址 |

### 会话与 Agent

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_DIRECTORY` | 桥启动时的工作目录 | 会话默认工作目录（OpenCode 项目路径） |
| `BRIDGE_DEFAULT_MODEL` | 空 | `default`/`auto`/空别名使用的模型（如 `opencode-go/gpt-6-luna`）；**留空=跟随上游默认**。客户端显式传的 `model` 始终透传，不受此影响。注意：上游默认若是 `opencode/*` 免费模型，经 API 调用会 403（免费额度只能在 OpenCode 内用），需要支持"不带 model"的请求就显式填一个可用的 |
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
| 自定义 id | 在 OpenCode 配置（`~/.config/opencode/opencode.jsonc` 的 `agents`）里自定义的 agent。例如 `orchestrator`：禁用内置工具、只用客户端声明的 tools，实现"桥只部署一处、工具在客户端执行"（见 `docs/DESIGN.md` §5.7.2） |

> 填一个不存在的 id，上游通常不报错，但行为不保证；请用实际存在的 agent。

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
| `BRIDGE_TOOL_CALL_WAIT` | `5m` | 工具调用挂起、等客户端回填结果的最长时间 |
| `BRIDGE_MCP_URL` | 空 | 注册给 OpenCode 的 MCP 基址（空=本机回环） |
| `BRIDGE_MCP_ALLOW` | 空 | 限制内置 MCP 端点 `/mcp/{token}` 的来源（逗号分隔 IP/CIDR）。空=不限制；对外暴露或上游在别机时建议设为上游网段 |
| `BRIDGE_PERMISSION_REPLY` | `once` | 自动应答权限请求：`once`（仅本次）/ `always`（记住）/ `reject`（拒绝）/ `off`（不干预） |

### Responses API / 用量

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_RESPONSES_ENABLED` | `true` | 是否开放 `/v1/responses` |
| `BRIDGE_RESPONSE_TTL` | `30m` | 已保存响应的保留时长（`previous_response_id` 依赖） |
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
| GET | `/metrics` | Prometheus 指标（默认需鉴权） |
| POST | `/v1/chat/completions` | 流式 + 非流式 |
| POST | `/v1/responses` | OpenAI **Responses API**（流式 + 非流式） |
| GET | `/v1/responses/{id}` | 取回已保存的响应 |
| DELETE | `/v1/responses/{id}` | 删除响应 |
| POST | `/v1/completions` | 旧版补全，内部降级为单轮 chat |
| * | `/v1/*` | 其余一律返回 OpenAI 格式 404（客户端不会因解析失败而崩） |

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

**Responses 也走同一套内核**：`previous_response_id` 直接复用上一条响应所属的 Janus 会话
（也就是同一个 OpenCode session），每轮只发新增 `input`；Chat 与 Responses 只是键空间不同
（`x:` / `f:` / `resp:`），底层 Store / executor / ToolBridge 共用。

**已知限制**：既没有 `X-Session-ID`/`X-OpenCode-Session` 头、也没有 `user` 字段，且两个 client
的首条历史**完全相同**时，会被当作同一条会话线（这是"多轮自动续接"所依赖的匹配）。要强隔离，
请让客户端带 `user` 或显式会话头。

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

### 图片

OpenAI 的 `content` 数组形态（`image_url`）已支持：

- `data:image/...;base64,...` → 直接作为附件传给上游
- `http(s)://...` → 先下载再转 base64（上游**只认 data URI**，实测 https 会 400）
- 超过 12MB 或下载失败 → 降级，并在 prompt 里用文字说明"有张图看不到"，
  避免模型自信地回答"未看到图片"

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
```

`compat_test.py` 覆盖 22 项断言：模型列表、流式/非流式、`include_usage`、
多轮上下文、`reasoning_content`、错误语义（404/400/401）、图片、并发隔离。

## 安全边界（重要）

Janus 背后是一个**能执行 shell、读写文件**的 agent（默认以启动用户身份运行，常见是 root）。把它暴露出去 ≈ 把 shell 交出去。要点：

- **默认只监听回环**（`BRIDGE_ADDR=127.0.0.1:2810`）。要远程访问，优先走 SSH 隧道 / VPN / Tailscale，而不是直接 `0.0.0.0`。
- **必须设 `BRIDGE_API_KEY`**；绑定非回环且 key 为空时任何人都能消耗你的额度。
- 桥自身**不做 TLS**，公网/不可信网络务必套反向代理（Caddy/nginx）加 HTTPS。
- **内置 MCP 端点** `/mcp/{token}` 用 128 位随机 token 鉴权，且随会话（一轮结束即注销）短命；仍可用 `BRIDGE_MCP_ALLOW` 按来源 IP/CIDR 再收一层。防串会话：空闲会话的工具调用会被立即拒绝。
- **权限自动应答** `BRIDGE_PERMISSION_REPLY`：`once`（默认）只放行单次，`always` 会写入 OpenCode 的持久权限，`reject` 更保守。
- 想要"agent 不碰本机、工具在客户端执行"，用 `orchestrator` agent（见 `docs/DESIGN.md` §5.7.2）。

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

