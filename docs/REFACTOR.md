# Janus 重构设计：独立模型网关 + 可插拔 Agent

> 状态：设计基线（待评审后实施）
> 日期：2026-10-18
> 原则：**不做兼容**。旧 env / 旧表 / 旧 `auth switch` 链路全部废弃，按本文重建。

---

## 0. 一句话目标

Janus 从「OpenCode 的协议兼容桥」升级为**厂商无关的模型网关 + 控制面**：

- **模型面**：Janus 直连厂商，provider / 凭据 / 模型目录 / 路由全部是数据库里的数据。
- **Agent 面**：OpenCode 只是 `AgentRuntime` 的一个实现，和 Claude Code、Codex、direct 平级。
- **配置面**：除「监听地址、数据库路径、主密钥」三个 bootstrap 项，其余全部落库、全部可在 Web 配置。

```
                  ┌──────────── 控制面（DB + /api/v1） ────────────┐
                  │  providers / credentials / models / routes     │
                  │  keys / agents / settings                      │
                  └────────────────────────────────────────────────┘

IDE ──client key──► ┐
                    ├─► janus ──► 厂商 A / B / C（模型面，严格透传）
Agent ─agent key──► ┘      │
  (opencode)               └──► agent runtime（会话/工具/权限，保持现有逻辑）
        ▲                              │
        └──────── tool key ────────────┘  (MCP 回环)
```

两条方向明确的链路：

| 链路 | 说明 | 行为 |
|---|---|---|
| **Agent → Janus → 厂商** | OpenCode / Codex / CC 作为 Janus 的客户端 | 模型名、参数、响应**全部透传**，Janus 只注入凭据与选路 |
| **IDE → Janus → Agent** | IDE 调会话型接口 | **保持现有逻辑**：会话映射、工具桥、权限、流式 |

---

## 1. 配置边界：什么还留在 env

只有「数据库还没打开之前就必须知道」的项留在 env，其余一律入库。

### 1.1 保留的 env（bootstrap）

| 变量 | 说明 | 默认 |
|---|---|---|
| `JANUS_ADDR` | 监听地址 | `0.0.0.0:2810` |
| `JANUS_DB` | SQLite 路径 | `paths.DefaultDBPath()` |
| `JANUS_DB_KEY` | 凭据加密主密钥（或 `JANUS_DB_KEY_FILE`） | 首次启动生成并写 `data_dir/master.key` |
| `JANUS_DATA_DIR` | 数据目录（workspaces / 日志 / 取证） | `paths.DataDir()` |
| `JANUS_LOG_LEVEL` | 启动早期日志级别 | `info` |
| `JANUS_BOOTSTRAP_TOKEN` | 可选：首启时指定的 admin key（一次性） | 空 = 自动生成 |

理由：这六项是「打开数据库」的前置条件，写进数据库会形成循环依赖。

### 1.2 全部下沉到 DB 的配置（旧 env 一律废弃）

`BRIDGE_API_KEY`、`BRIDGE_MODEL_MAP`、`BRIDGE_DEFAULT_MODEL`、`BRIDGE_AGENT`、`BRIDGE_DIRECTORY`、
`BRIDGE_PROJECT*`、`BRIDGE_SESSION_TTL`、`BRIDGE_REQUEST_TIMEOUT`、`BRIDGE_STREAM_*`、
`BRIDGE_TOOL_*`、`BRIDGE_MCP_*`、`BRIDGE_PERMISSION_REPLY`、`BRIDGE_RATE_LIMIT*`、
`BRIDGE_RESPONSES_ENABLED`、`BRIDGE_ANTHROPIC_ENABLED`、`BRIDGE_MODEL_ECHO`、`BRIDGE_USAGE_*`、
`BRIDGE_WORKBUDDY_*`、`BRIDGE_WEDGE_FORENSICS`、`BRIDGE_CORS_ORIGIN`、`BRIDGE_METRICS_PUBLIC`、
`OPENCODE_*`（端点/密码/自动发现全部改为 `agents` 表里的记录）。

`janus.env` / `janus.env.example` 删除，替换为 `janus.bootstrap.example`（只含 §1.1 六项）。

### 1.3 首启引导

```
$ janus serve
[init] created admin key: sk-janus-a-3f9c...   ← 只打印一次，同时写 data_dir/bootstrap.key (0600)
```

- 已存在 admin key → 不打印。
- `bootstrap.key` 文件删除后仍可从 DB 里轮换（`POST /api/v1/keys/{id}/rotate`）。

---

## 2. 数据模型

用 GORM 定义，`db.Open` 里 `AutoMigrate`。所有时间字段为 Unix 秒（int64），与现有代码一致。

### 2.1 `providers`：厂商

```go
type Provider struct {
    ID        string `gorm:"primaryKey;size:64"`  // anthropic / openai / opencode-go / workbuddy
    Name      string `gorm:"size:120"`
    Type      string `gorm:"size:32"`   // openai-chat | openai-responses | anthropic-messages
    BaseURL   string `gorm:"size:512"`
    AuthKind  string `gorm:"size:32"`   // api_key | oauth | none
    Headers   []byte `gorm:"type:blob"` // 附加静态头（JSON object），如桌面端身份头
    Enabled   bool   `gorm:"index"`
    CreatedAt int64
    UpdatedAt int64
}
```

`Type` 决定协议翻译方向；`Headers` 解决 WorkBuddy 这类「上游要求特殊头」的问题，不再需要专门代码路径。

### 2.2 `credentials`：凭据（取代 `accounts`）

```go
type Credential struct {
    ID         string `gorm:"primaryKey;size:64"`
    ProviderID string `gorm:"index;size:64"`
    Label      string `gorm:"index;size:80"`   // 唯一（provider 内），客户端可用 "<label>/<model>" 指定
    Secret     []byte `gorm:"type:blob"`       // AES-GCM
    Priority   int    `gorm:"index"`           // 越小越优先
    Enabled    bool   `gorm:"index"`
    LastUsedAt int64
    LastError  string `gorm:"size:512"`
    CreatedAt  int64
    UpdatedAt  int64
}
```

`Secret` 明文结构按 `provider.auth_kind`：

```json
{"api_key": "..."}
{"access_token": "...", "refresh_token": "...", "expires_at": 1760000000}
```

**没有 `Active` 字段**。这是本次重构最关键的一处：Janus 直连厂商后，每份凭据就是一个独立的 HTTP client，天然并发，不再有「同一时刻只有一份凭据生效」的全局状态。

### 2.3 `models`：模型目录

```go
type Model struct {
    ID           string `gorm:"primaryKey;size:200"` // provider_id + "/" + upstream_id
    ProviderID   string `gorm:"index;size:64"`
    UpstreamID   string `gorm:"size:160"`            // 发给厂商的原始 id
    DisplayName  string `gorm:"size:200"`
    Capabilities []byte `gorm:"type:blob"`           // {tools,vision,reasoning,context}
    Pricing      []byte `gorm:"type:blob"`
    Source       string `gorm:"size:16"`             // discovered | manual
    Enabled      bool   `gorm:"index"`
    UpdatedAt    int64
}
```

目录来源：`POST /api/v1/providers/{id}/discover` 拉厂商 `/models`，合并进表；用户可手工增删改。
**未知模型不拒绝**：只要 provider 存在就透传（保留现有 `modelx.Resolve` 的语义）。

### 2.4 `model_routes`：路由规则（本次需求核心）

```go
type ModelRoute struct {
    ID       string `gorm:"primaryKey;size:64"`
    Scope    string `gorm:"index;size:16"` // global | provider | credential | role
    ScopeID  string `gorm:"index;size:64"` // 对应 providers.id / credentials.id / role 名
    Alias    string `gorm:"index;size:160"`// 小写，尾部 * 为前缀通配
    Target   string `gorm:"size:200"`      // provider/model[:variant]
    Priority int    `gorm:"index"`
    Enabled  bool   `gorm:"index"`
    CreatedAt int64
    UpdatedAt int64
}
```

唯一约束 `(scope, scope_id, alias)`。删除 provider / credential 时级联删除。

这解决了原设计的硬伤：**映射键可以带作用域**，同一个别名在不同凭据下指向不同模型：

```
scope=credential, scope_id=cred_work, alias=claude-opus*  → anthropic/claude-opus-4-5
scope=credential, scope_id=cred_personal, alias=claude-opus* → opencode-go/claude-opus-4-1
scope=global,     scope_id="",        alias=claude-haiku* → opencode-go/glm-5.3-flash
```

### 2.5 `api_keys`：三类 key 隔离

```go
type APIKey struct {
    ID           string `gorm:"primaryKey;size:64"`
    Kind         string `gorm:"index;size:16"` // admin | client | agent | tool
    Name         string `gorm:"size:120"`
    Prefix       string `gorm:"index;size:24"` // sk-janus-a- / -c- / -g- / -t-
    Hash         string `gorm:"index;size:64"` // sha256(plaintext)
    Scopes       []byte `gorm:"type:blob"`     // ["model:chat","model:list","session:*","admin:*"]
    ProviderIDs  []byte `gorm:"type:blob"`     // agent key 白名单，空 = 全部
    CredentialIDs []byte `gorm:"type:blob"`    // agent key 可用的凭据白名单
    ExpiresAt    int64  `gorm:"index"`         // 0 = 永久
    Revoked      bool   `gorm:"index"`
    LastUsedAt   int64
    CreatedAt    int64
}
```

| Kind | 可访问 | 不可访问 |
|---|---|---|
| `admin` | `/api/v1/*` | 模型端点 |
| `client` | `/v1/*`（模型 + 会话）、`/mcp/{session}`（自己的） | `/api/v1/*` |
| `agent` | `/v1/models`、`/v1/chat/completions`、`/v1/messages` | `/api/v1/*`、会话管理端点 |
| `tool` | 仅 `/mcp/{session}` 且 token 与会话绑定 | 其余全部 |

明文只在创建/轮换时返回一次。存储只存 `sha256`。

### 2.6 `agents`：harness 注册表

```go
type Agent struct {
    ID         string `gorm:"primaryKey;size:64"` // opencode / codex / claude-code / direct
    Kind       string `gorm:"size:32"`            // opencode | direct | cli
    BaseURL    string `gorm:"size:512"`           // 内部端点（opencode serve）
    AuthKind   string `gorm:"size:32"`            // basic | bearer | none
    Secret     []byte `gorm:"type:blob"`          // 连 agent 的凭据
    APIKeyID   string `gorm:"size:64"`            // 反向注入给它的 agent key
    Enabled    bool   `gorm:"index"`
    Config     []byte `gorm:"type:blob"`          // 启动参数 / 环境 / 模型清单覆盖
    CreatedAt  int64
    UpdatedAt  int64
}
```

OpenCode 的自动发现（端口/密码每次都变）变成 `Kind=opencode` + 一个后台探测任务，探测结果写回 `BaseURL/Secret`，不再是散落的 env。

### 2.7 `settings`：其余全部配置

```go
type Setting struct {
    Key       string `gorm:"primaryKey;size:80"`
    Value     []byte `gorm:"type:blob"` // JSON 编码，保留类型
    UpdatedAt int64
}
```

设置项由一个 **Go 侧 schema 注册表** 声明，前端据此自动生成表单：

```go
type SettingSpec struct {
    Key     string
    Type    string // string | int | bool | duration | enum | json
    Default any
    Enum    []string
    Min, Max *int64
    Group   string // session | stream | tools | mcp | limits | features | usage
    Title   string
    Desc    string
    Secret  bool   // 前端掩码显示
}
```

`GET /api/v1/settings/schema` 返回全部 `SettingSpec`，UI 无需硬编码任何字段。

### 2.8 会话表变更

`conversations` 增加三列，用于「换凭据必须重建会话」：

```go
AgentID      string `gorm:"size:64"`
ProviderID   string `gorm:"size:64"`
CredentialID string `gorm:"size:64"`
```

请求进来时若 `(provider, credential, model)` 与库里不一致 → 丢弃旧 `session_id`、重放历史重建会话。

### 2.9 删除的表

`accounts`、`account_models` 直接移除。首次启动检测到旧表 → 报错并提示删除数据库（不做迁移，符合「不兼容」要求）。

---

## 3. 后端分层

```
internal/
  store/            ← 新：DB 访问层（providers/credentials/models/routes/keys/agents/settings）
  gateway/          ← 新：模型面（路由解析 + 协议翻译 + 透传）
    router.go       ← Route()
    provider.go     ← Provider client（按 auth_kind 注入凭据）
    translate.go    ← anthropic ↔ openai 双向翻译
  agent/            ← 新：AgentRuntime 接口
    runtime.go
    opencode/       ← 现有 chat.go 的会话/流式/工具逻辑搬入
    direct/         ← 无会话，直接走 gateway
  api/
    admin.go        ← /api/v1/*
    model.go        ← /v1/*（薄壳，调 gateway）
    auth.go         ← key 解析 + scope 校验
```

### 3.1 路由解析（唯一入口）

```go
type RouteInput struct {
    WireModel      string
    Protocol       Protocol  // openai-chat | openai-responses | anthropic
    Key            Principal // 来自 api_keys，带 ProviderIDs/CredentialIDs 白名单
    CredentialHint string    // header X-Janus-Credential 或 "<label>/" 前缀
    Session        *SessionBinding
}

type RouteResult struct {
    ProviderID   string
    CredentialID string
    UpstreamID   string // 发给厂商的模型 id
    Variant      string
    Rule         string // 命中说明，进日志与 /api/v1/routes/resolve
    Passthrough  bool   // provider 已知但模型不在目录
}

func (g *Gateway) Route(ctx context.Context, in RouteInput) (RouteResult, error)
```

**优先级（确定性，逐级短路）：**

1. **会话钉住**：会话已绑定 `(provider, credential, model)` 且 `WireModel` 未变 → 直接复用。
2. **显式限定**：`<label>/<model>`（label 命中唯一 credential）或 `X-Janus-Credential` 头。
3. **路由表**：`scope=credential`（已确定凭据时）→ `scope=provider` → `scope=global`；同层先精确匹配，再取最长 `*` 前缀。
4. **虚拟模型**：`janus`、`janus/<role>[@auto|:variant]` → `roles` 绑定。
5. **目录精确匹配**：`provider/model`，或裸 id 在目录中唯一。
6. **档位兜底**：仅 `Protocol=anthropic` 且能识别 tier（opus/sonnet/haiku）→ 该凭据/全局默认模型。
7. **失败**：见 §3.3。

关键：**第 2 步就把 credential 绑定进结果**，后续规则查询都带作用域。旧实现「先剥账号前缀再映射」导致账号信息丢失的问题不复存在。

### 3.2 凭据选择

同一 `(provider, model)` 可能有多份凭据：

```
按 Priority ASC, CreatedAt ASC 排序
  ├─ 命中 Key.CredentialIDs 白名单 → 过滤
  ├─ Enabled=false → 过滤
  ├─ LastError 在冷却窗口内 → 跳过（指数退避，上限 5 分钟）
  └─ 取第一个
```

**没有 hint 且候选 >1 时返回歧义错误**，而不是静默挑一个（旧 `db.First` 的随机行为正是问题根源）。

### 3.3 错误语义

| 情况 | HTTP | code | 附带 |
|---|---|---|---|
| provider 未知 | 404 | `model_not_found` | 候选模型列表 |
| provider 已知、模型不在目录 | — | — | **透传**，让厂商报真实错误 |
| 同名模型多凭据且无 hint | 409 | `ambiguous_model` | `["work/claude-opus-4-1", "personal/claude-opus-4-1"]` |
| 凭据全部不可用 | 503 | `no_available_credential` | 每份凭据的 `LastError` |
| 路由目标自身是别名 | 400 | `invalid_route` | 禁止链式，防循环 |

### 3.4 透传规则

模型面**只改一个字段**：请求体里的 `model` 替换为 `UpstreamID`。

- 其余字段（`tools`、`tool_choice`、`temperature`、`stream`、`metadata`…）原样转发。
- 响应字节原样回写；`model` 回显按 `settings.model_echo` 处理（`request` 回显客户端原名，`real` 回显 `provider/model`）。
- 只在「客户端协议 ≠ provider.type」时做翻译（Anthropic ↔ OpenAI），翻译范围限于结构映射，不改语义。

### 3.5 Agent 面

```go
type AgentRuntime interface {
    Name() string
    EnsureReady(ctx context.Context) error
    CreateSession(ctx context.Context, spec SessionSpec) (string, error)
    Prompt(ctx context.Context, sid string, req PromptReq) (<-chan Event, error)
    SetModel(ctx context.Context, sid, model string) error
    Interrupt(ctx context.Context, sid string) error
    Close(ctx context.Context) error
}
```

- `opencode`：现有 `internal/api/chat.go` 的会话/流式/工具/权限逻辑整体迁入，行为不变。
- `direct`：不建会话，直接调 `Gateway.Chat`，用于纯 LLM 客户端。
- 工具桥（MCP）留在 janus 侧，因为 MCP 是通用协议，不绑定 harness。

**OpenCode 不再持有厂商凭据**：启动时注入

```json
{
  "provider": {
    "janus": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "http://127.0.0.1:2810/v1", "apiKey": "<agent key>" },
      "models": { "anthropic/claude-opus-4-5": {}, "opencode-go/mimo-v2.5-pro": {} }
    }
  }
}
```

于是 `opencode auth switch`、`SetActiveAccount`、`ModelsForAccount`、`ExternalID` 全部删除。

---

## 4. API 设计

### 4.1 管理面 `/api/v1/*`（admin key）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/providers` | 列表 / 新建 |
| GET/PATCH/DELETE | `/providers/{id}` | 详情 / 修改 / 删除 |
| POST | `/providers/{id}/discover` | 拉厂商 `/models` 合并目录 |
| POST | `/providers/{id}/test` | 最小探测（1 token） |
| GET/POST | `/credentials?provider_id=` | 列表 / 新建 |
| PATCH/DELETE | `/credentials/{id}` | 修改 / 删除 |
| POST | `/credentials/{id}/verify` | 校验并刷新 `LastError` |
| POST | `/credentials/{id}/refresh` | OAuth 刷新 |
| GET | `/models?provider_id=&q=` | 目录 |
| PATCH | `/models/{id}` | 启停 / 改名 / 能力 |
| POST | `/models/sync` | 全量同步 |
| GET/POST | `/routes` | 路由规则 |
| PATCH/DELETE | `/routes/{id}` | 修改 / 删除 |
| POST | `/routes/resolve` | 调试：`{model, credential?, protocol}` → `RouteResult` + 命中规则 |
| GET/POST | `/keys` | 列表 / 创建（明文只返回一次） |
| DELETE | `/keys/{id}` | 吊销 |
| POST | `/keys/{id}/rotate` | 轮换 |
| GET/POST | `/agents` | harness 列表 / 注册 |
| PATCH/DELETE | `/agents/{id}` | 修改 / 删除 |
| POST | `/agents/{id}/provision` | 返回该 harness 的配置片段（env / config.toml） |
| GET | `/settings` | 全部设置值 |
| GET | `/settings/schema` | 设置项 schema（前端自动生成表单） |
| PATCH | `/settings` | 批量更新 `{key: value}` |
| GET | `/sessions` | 会话列表（替代现有内存态） |
| DELETE | `/sessions/{key}` | 强制重建 |

统一响应：成功返回资源本体；失败 `{"error":{"code","message","details"}}`。

### 4.2 模型面 `/v1/*`（client / agent key）

保留现有路径，语义改为「Janus 直连厂商」：

```
GET  /v1/models
GET  /v1/models/{id...}
POST /v1/chat/completions
POST /v1/completions
POST /v1/responses
GET  /v1/responses/{id}
POST /v1/messages
POST /v1/messages/count_tokens
GET  /anthropic/v1/models
```

新增：

```
POST /v1/embeddings     ← 可选，透传
```

删除：`/v1/accounts*`、`/v1/settings`（并入 `/api/v1/settings`）、`/v1/usage`（并入 `/api/v1/usage`）。

### 4.3 工具面

```
POST /mcp/{session_token}   ← 只认 tool key，且必须等于该会话的 token
```

### 4.4 鉴权中间件

```go
func (s *Server) requireScope(scope string) func(http.Handler) http.Handler
```

- 从 `Authorization: Bearer` 或 `x-api-key` 取 key → `sha256` → 查 `api_keys`（带 30s 内存缓存）。
- 校验 `Revoked`、`ExpiresAt`、`Scopes`。
- agent key 额外校验请求模型所属 provider 是否在 `ProviderIDs` 白名单内。
- 失败统一 401，**不区分**「key 不存在」与「scope 不足」，避免枚举。

---

## 5. 前端设计

### 5.1 框架选型

约束：单二进制、`go:embed`、离线可用、无 CDN 依赖、配置表单密集。

| 方案 | 体积(gz) | 构建步骤 | 适合度 |
|---|---|---|---|
| **Preact + htm** | ~5 KB | **无** | ✅ 组件化、ESM 直接跑、vendor 后离线 |
| Alpine.js | ~15 KB | 无 | 表单简单，但多页面状态与复用差 |
| htmx + Go template | ~14 KB | 无 | CRUD 好，交互（路由调试器）弱 |
| Vue 3 全局构建 | ~34 KB | 无 | 可用但偏重 |
| Svelte 5 + Vite | ~8 KB | **需要** | 体验最好，但引入 Node 构建链 |

**推荐：Preact + htm，零构建。** 文件直接以 ES Module 形式 `go:embed`，`index.html` 用 import map 指向本地 vendor 文件。

若后续接受 Node 构建，可平滑换 Svelte 5（API 层不变）。

### 5.2 目录

```
internal/ui/
  ui.go                 // go:embed dist
  dist/
    index.html          // import map + 挂载点
    app.js              // 路由 + 布局
    api.js              // fetch 封装（统一带 admin key、错误提示）
    store.js            // 轻量信号（signal/computed/effect，约 60 行）
    components/
      table.js  form.js  modal.js  toast.js  keybadge.js
    pages/
      overview.js  providers.js  credentials.js  models.js
      routes.js    keys.js       agents.js       settings.js
      requests.js  sessions.js
    vendor/
      preact.module.js  hooks.module.js  htm.module.js
```

### 5.3 页面

| 页面 | 核心交互 |
|---|---|
| 概览 | 健康状态、今日请求/用量、错误率、最近失败 |
| Providers | 表格 + 抽屉编辑；类型切换时联动 base_url 模板；「测试连接」 |
| Credentials | 按 provider 分组；新增时粘贴 key 或走 OAuth 引导；显示 `LastError` 与冷却倒计时 |
| Models | 目录表格，按 provider 折叠；启停开关；能力标签；「同步」 |
| **Routes** | 规则表格（作用域 / 别名 / 目标 / 优先级，可拖拽排序）；**内置解析器**：输入模型名 + 选凭据 → 实时显示命中规则链 |
| Keys | 按 kind 分组；创建后弹一次性明文 + 复制；吊销 / 轮换 |
| Agents | harness 卡片；状态灯（探测结果）；「生成配置」弹窗给出 env / config.toml |
| Settings | **由 `/settings/schema` 自动渲染**：分组折叠、类型化控件、即时保存、脏值提示 |
| Requests | 沿用现有请求日志，增加「命中的路由规则」列 |
| Sessions | 会话列表，显示绑定的 provider/credential/model，可强制重建 |

### 5.4 鉴权与引导

- 页面静态资源公开（不含敏感数据）。
- 首访无 key → 显示引导页：粘贴 admin key（存 `localStorage`）或按提示读取 `bootstrap.key`。
- 所有请求 401 → 清除本地 key 并回到引导页。
- 不做服务端 session/cookie，保持无状态。

### 5.5 路由

hash 路由（`#/providers`），无需服务端 rewrite。`GET /ui/*` 提供静态文件，`GET /` 重定向 `/ui`。

---

## 6. 实施顺序

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P0 骨架** | 新 schema、`store` 层、bootstrap key、`requireScope`、`/api/v1/settings` + schema | 空库启动 → 打印 admin key → 能用 key 读写设置 |
| **P1 模型面** | providers / credentials / models / routes + `Gateway.Route` + 透传代理 | `curl` 用 agent key 打 `/v1/chat/completions`，模型透传到厂商；`/routes/resolve` 输出命中链 |
| **P2 Agent 面** | `AgentRuntime`、opencode adapter、反向注入 janus provider、删除 `auth switch` | IDE → janus → opencode 行为与现在一致；OpenCode 不再持有厂商凭据 |
| **P3 前端** | Preact 重写全部页面 | 所有配置可在 UI 完成，DB 里能看到变更 |
| **P4 扩展** | `direct` / codex / claude-code adapter、`roles`、fallback 链 | 新增 harness 只需实现接口 |

P2 完成即可删除：`accounts`/`account_models` 表、`auth switch` 调用、`workbuddy` 反向注入、`inlineconfig.go` 里的 provider 拼装。

> **P2 进度（未完全）**：`AgentRuntime` 接口与注册表（`internal/agent`）、
> `opencode` / `direct` 两个适配器、`GET /api/v1/runtimes`、反向注入 janus provider
> （`opencode_provider_inject`，`inlineconfig.go` 统一生成）已落地；`accounts` /
> `account_models` / `auth switch` 在 P1 已删除。
> **仍待做**：把 `internal/api/chat.go` 的会话/流式/工具/权限逻辑物理搬进
> `internal/agent/opencode`（当前 opencode 适配器是薄适配，复用 `upstream` + EventBus，
> 未搬迁），以及把反向注入从「可选开关」变成默认路径。

> **harness 接入层（Provision，新增）**：`AgentRuntime` 只解决「怎么驱动一个已在跑的
> harness」，但「怎么让任意 harness 用上 janus（模型 provider + MCP 注册）」原先硬编码在
> `handleProvisionAgent` 里、只认 opencode。现抽出 `agent.Harness` 接口 +
> `agent.ToolMode`（`native`/`bridge`/`none`，即旧 README 的 A/B/C 三模式），
> 由 `internal/channel/{opencode,codex,claudecode}` 各自实现，`/agents/{id}/provision`
> 按 `kind` 分发。
>
> **仍待做（按优先级）**：
> 1. ~~**稳定 MCP 端点**~~ **已做**：`/mcp/<bindingToken>`，token 由 agent key
>    确定性派生（`b`+sha256("janus-mcp:"+keyID)[:16]），provision 已把真实地址
>    写进 codex/CC 的配置。该地址是 key 级**回退入口**：只在候选会话唯一
>    （或唯一有在飞请求）时命中，并发歧义时 fail-closed（tools/list 为空，
>    tools/call 仍可按会话命名空间精确路由）。janus 自己拉起 CLI 时，
>    `runtimeMCPURL` 会改写为会话级精确 token，不依赖「最近活跃」猜测。
> 2. **工具来源**：模式 B 现在只能消费「客户端在请求里声明的 tools」。CLI harness
>    独立运行时无人声明 → 需要「janus 侧按项目声明的工具目录」作为回退。
> 3. ~~**CLI 型 harness 驱动**~~ **已做（核心链路）**：`internal/agent/cliproc.go`
>    （通用子进程驱动：按行读 stdout、`Kill`/`Wait`）+ `internal/agent/cliruntime.go`
>    （通用 `CLIRuntime`：一 Prompt 一子进程，多轮靠 harness 会话 id + resume），
>    `internal/channel/{codex,claudecode}/runtime.go` 提供各自的 argv / env /
>    事件解析（codex `exec --json` + `exec resume`；claude `-p --output-format
>    stream-json` + `--resume`）。`GET /api/v1/runtimes` 已列出二者，
>    `POST /api/v1/runtimes/{name}/prompt` 可直接驱动一轮并按 SSE 回放事件。
>    ~~**事件来源抽象**~~ **已做**：`internal/api/turn.go` 引入 `turnDriver` /
>    `turnSession`，`chat.go` 的 executor 只消费 `agent.Event`，不再直接碰
>    `upstream.Upstream` + EventBus。
>    ~~**CLI 接进 IDE 请求路径**~~ **已做**：`internal/api/cli_turn.go` 提供
>    `cliTurnDriver` / `cliTurnSession`，`/v1/chat/completions`、`/v1/responses`、
>    `/v1/messages` 均可用 `X-Janus-Runtime: codex|claude-code` 显式选择 CLI
>    harness；不带该头时行为不变（agent key / `agent=direct` → direct，其余 →
>    opencode）。三处缺口按下面的方式补齐：
>
>    | 缺口 | 处理 |
>    |---|---|
>    | 对账（无 `ListMessages`） | `Reconcile` 返回 false、`ReconcileSupported` 为 false，收尾不再退避重试；事件流是唯一真相 |
>    | 会话状态 / 空闲（无 `GetSession` / `WaitUntilIdle`） | `State` 恒 false、`WaitIdle` 只等 ctx；收尾由 `EventDone` 驱动 |
>    | 工具桥生命周期 | MCP 地址写进子进程 argv/env（`ensureCLITools` 复用 `prepareToolSession`），不走 `s.up.AddMCP`；超时中断换成 `rt.Interrupt`（杀子进程） |
>
>    跨请求续流依赖 `CLIRuntime` 的会话级 `eventHub`：子进程事件始终广播，订阅者
>    每请求一个、来去自由；`Cancel` 只解绑订阅，不杀进程（工具调用回合要停在
>    MCP `tools/call` 上等下一次请求回填）。新一轮开始前 `PrepareTurn` 清理被
>    放弃的旧进程，避免撞上 `already has a running turn` 或把旧收尾事件混进
>    新订阅。**仍待做**：`tool source` 回退目录（见上第 2 条）仍只对 opencode
>    生效；CLI 独立运行（无客户端 `tools` 声明）时暂无 janus 侧工具目录。

---

## 7. 多 Agent 协作：自认领工作板

> 目标：多个 agent（opencode / codex / claude-code / direct）在同一个 workspace 里共同交付。
> 默认**自己认领**，不靠中央派活；**强制派工永久保留**，作为 admin 的常备能力。
> **省 token 和高质量交付是硬约束**，不是副作用。

### 7.1 六条原则

1. **黑板，不聊天**：agent 之间不直接对话，所有协作通过工作项、产物、评审意见传递。群聊是 token 黑洞，而且不可审计。
2. **引用，不复制**：工作板只传路径、哈希、摘要；正文留在 workspace / 产物库，agent 按需读。
3. **认领为主，强制永久保留**：`work_list` 暴露能力匹配的可认领项，agent 自己选；`work_assign` 是 admin 的常备能力，随时可强制派工，全程留审计。
4. **小步，可评审**：一个工作项 ≤ 1 个可评审变更（默认 ≤ 8 文件 / ≤ 400 行），更大的由 planner 拆。
5. **证据，才算完**：没有测试 / 构建 / 评审证据不能进 `done`。
6. **可回滚**：租约过期自动回收、评审可拒绝、强制可释放、失败有 attempts 上限。

### 7.2 角色与边界

| 角色 | 做什么 | 不做什么 |
|---|---|---|
| Planner | 把目标拆成工作项 + 依赖 DAG + DoD | 不指定谁做 |
| Worker | 认领 `open` 项，实现并提交证据 | 不评审自己的项 |
| Verifier | 认领 `review` 项，独立跑门禁并给结论 | 不与 worker 同 agent / 同会话 |
| Integrator | 认领 `approved` 项，串行合入并复跑门禁 | 不改实现（冲突打回） |
| Human | 处理歧义、安全敏感、超预算、连续失败 | 不参与日常认领 |

同一 agent 可以在不同工作项上扮演不同角色；**同一工作项上 worker 与 verifier 必须是不同 agent、不同会话**，最好不同模型族。

### 7.3 状态机

```
open ──claim(worker)──► running ──submit──► review
  ▲                        │                  │
  │                        └─release/expire───┘
  │                                           │
  ├────────reject─────────────────────────────┤
  │                                           ▼
  └────────release──── integrating ◄────approve
                          │
                          └──merge+gate──► done
blocked（人工 / 依赖 / 预算）可从任意态进入，处理完回 open
```

- `done` 只能由 integrator 在合入并通过门禁后置位。
- 评审拒绝 → `open`，`attempts+1`，保留上一轮拒绝理由与产物引用。
- 租约过期 → `open`，保留已产生的产物（下一个认领者能接着干）。
- `attempts >= 3` → `blocked`，转人工。

### 7.4 数据模型

```go
type WorkItem struct {
    ID           string `gorm:"primaryKey;size:64"`
    WorkspaceID  string `gorm:"index;size:64"`
    ParentID     string `gorm:"index;size:64"`   // 子工作项
    Title        string `gorm:"size:200"`
    Brief        string `gorm:"type:text"`       // ≤ 800 token
    DoD          []byte `gorm:"type:blob"`       // acceptance + checks
    Paths        []byte `gorm:"type:blob"`       // 写作用域前缀
    ExecutionSite string `gorm:"size:16"`        // server|client
    RequiredModes []byte `gorm:"type:blob"`      // native|bridge|none
    DependsOn    []byte `gorm:"type:blob"`
    Capabilities []byte `gorm:"type:blob"`       // go / react / k8s ...
    Priority        int    `gorm:"index"`
    Status          string `gorm:"index;size:16"` // open|running|review|approved|integrating|done|blocked|cancelled
    Attempts        int
    BudgetTokens    int64
    Risk            string `gorm:"size:16"`       // low|medium|high
    Autonomy        string `gorm:"size:8"`        // L0|L1|L2|L3
    CapsuleID       string `gorm:"index;size:64"` // 上下文胶囊
    PrefixVersion   string `gorm:"index;size:64"` // workspace 前缀契约
    Replicas        int                           // 1=普通；>1=投机执行
    ContractVersion int                           // 契约版本
    CreatedBy       string `gorm:"size:64"`
    Version         int64  `gorm:"index"`         // 乐观锁
    CreatedAt       int64
    UpdatedAt       int64
}

type WorkClaim struct {
    ID             string `gorm:"primaryKey;size:64"`
    WorkItemID     string `gorm:"index;size:64"`
    AgentID        string `gorm:"index;size:64"`
    Role           string `gorm:"size:16"`       // worker|verifier|integrator
    LeaseTokenHash string `gorm:"size:64"`
    LeaseEpoch     int64  `gorm:"index"`          // fencing token
    LeaseExpiresAt int64  `gorm:"index"`
    IdempotencyKey string `gorm:"index;size:64"`  // submit/merge 幂等
    HeartbeatAt    int64
    Forced         bool
    Plan           string `gorm:"size:512"`      // 认领时的一句话计划
    Status         string `gorm:"index;size:16"` // active|released|expired|submitted
    CreatedAt      int64
}

type WorkArtifact struct {
    ID         string `gorm:"primaryKey;size:64"`
    WorkItemID string `gorm:"index;size:64"`
    Kind       string `gorm:"size:16"`   // diff|patch|log|test|report|note
    URI        string `gorm:"size:512"`  // 文件路径 / 对象键，正文不落 DB
    SHA256     string `gorm:"index;size:64"`
    Bytes      int64
    Summary    string `gorm:"size:1024"` // ≤ 200 token
    CreatedBy  string `gorm:"size:64"`
    CreatedAt  int64
}

type WorkReview struct {
    ID         string `gorm:"primaryKey;size:64"`
    WorkItemID string `gorm:"index;size:64"`
    ReviewerID string `gorm:"index;size:64"`
    Verdict    string `gorm:"size:16"`    // approve|reject|needs_human
    Reasons    string `gorm:"size:2048"`
    Evidence   []byte `gorm:"type:blob"`  // artifact id
    CreatedAt  int64
}
```

另加 `work_events`（append-only，供 SSE / 审计），字段：`id / work_item_id / type / actor / payload / created_at`。

### 7.5 认领协议

工作板以 **MCP 工具**暴露给 agent（复用 §4.3），不需要每个 harness 自己写 HTTP 客户端：

| 工具 | 作用 | 关键约束 |
|---|---|---|
| `work_list` | 列可认领卡片 | 只返回能力匹配、模式匹配、依赖就绪、未被锁的项；默认 5 条，支持 `since` 增量 |
| `work_get` | 取详情 | `view=work|review`；review 视图不含实现者对话 |
| `work_claim` | CAS 认领 + 租约 | 必须带 `plan`（≤ 80 token）；返回 `lease_token + lease_epoch`；冲突 409 只回短原因 |
| `work_heartbeat` | 续租 | 默认 20 分钟 TTL，2 分钟心跳，最长 2 小时；带 `lease_epoch` |
| `work_release` | 释放 / 阻塞 | 带原因；10 分钟内不得重复认领同一项 |
| `work_submit` | 提交评审 | 必须有 ≥ 1 产物 + DoD 证据；带 `idempotency_key`，重复提交幂等 |
| `work_verdict` | 评审结论 | reviewer ≠ worker；approve 必须引用具体检查 |
| `work_assign` | 强制派工 | 仅 admin；`forced=true` 留审计；agent 可带理由释放 |
| `work_create` | 建项 / 拆项 | planner 或 admin；不能建环 |
| `work_counter` | 认领前反提议：拆项 / 改 checks / 缩 paths / 加预算 | planner 或 verifier 批准后生效；不能改 goal |
| `auth_request` | 申请敏感操作能力令牌 | 结构化 action；审计模型独立；token 单次 / 短 TTL（见 §13） |

**认领怎么选**：`work_list` 按 `priority DESC, age ASC` 返回，对超过 2 小时无人认领的项打 `stale` 标记；**选哪个由 agent 决定**，Janus 不评分、不指派。

**硬性准入**（Janus 侧，不靠 agent 自觉）：

- capability 匹配；
- 依赖全部 `done`；
- 写路径与活跃租约无重叠（`path_locked`）；
- 同一 agent 同时最多 1 个 `running` + 1 个 `review`（可配）；
- 租约未过期；
- `attempts < 3`。

**强制派工（永久保留）**：`work_assign` 走同一张 claim 表，只是 `forced=true`。它是 admin 的常备能力，**不随打分、降级或淘汰关闭**；即使目标组合处于 `suspended`，也可以强制派工，但合入必须人审。被派 agent 若能力不匹配，可以 `work_release(reason=capability_mismatch)`，项回到 `open` 并通知 human；不允许静默不接。

### 7.6 上下文包（省 token 的核心）

`work_claim` 成功时返回一个**有上限**的 context pack，而不是整个工作板 / 历史。它不是临时拼接，而是 **Context Capsule** 的投影（§12.3）：胶囊持久化、版本化，`CapsuleID + version + base_commit` 跟工作项绑定；代码变了旧胶囊标 `stale`，必须重新生成。

| 组成 | 上限 | 说明 |
|---|---|---|
| Brief + DoD | 800 token | 建项时就必须写短 |
| 相关路径 / 符号 | 400 token | 只给文件 + 行号 / 符号，正文让 agent 自己读 |
| 上一轮拒绝理由 | 200 token | 只取最近 1 次 |
| Workspace 事实 | 400 token | 构建 / 测试 / lint 命令、环境变量、约定；按 workspace 缓存 |
| 约束 | 100 token | 禁改路径、风格、安全要求 |

- 超过上限 → 只返回引用，agent 用 `read` 自己取。
- **不返回**：其他工作项、其他 agent 的对话、完整 git 历史、全量文件内容。
- 交接摘要（worker → verifier）≤ 400 token，其余用产物引用。

### 7.7 Token 预算与会话策略

- 每个工作项 `budget_tokens`；Janus 用上游 usage 计量，含 cache read / write。
- 70% → 给 agent 一条短提示；100% → 停止新 turn，要求提交当前产物 + `blocked`，**不静默杀**。
- 一个工作项一个会话，不跨项复用；同一 workspace 固定同一 provider / credential，保证 prompt cache 命中；system + workspace 事实 + 工具定义保持稳定前缀，任务内容放尾部。
- 选活 / 分诊 / 首轮评审用便宜模型；实现和争议评审用强模型；升级要有理由（首轮 reject、DoD 歧义）。
- 不做 agent 间广播；`work_list` 默认 5 条 + `since` 增量，空闲才拉。
- 工具集只在**工作项边界**调整；冷工具不注册。会话中途改工具会让下一次全量重写缓存。
- 产物按 SHA256 去重；同一 diff 不重复进上下文。

### 7.8 质量闭环

**DoD 必须机器可验**：

```json
{
  "acceptance": ["POST /v1/work-items 返回 201", "重复 claim 返回 409"],
  "checks": [
    {"name": "unit", "cmd": "go test ./internal/work/...", "timeout": 300},
    {"name": "build", "cmd": "go build ./...", "timeout": 300}
  ],
  "max_files": 8,
  "max_lines": 400
}
```

- 提交时证据不足 → 400；代码项至少要有 diff + 一条 `checks` 的真实输出。
- Verifier 在干净 workspace 复跑 `checks`，只看 diff + DoD + 证据，**不看实现者对话**；approve 必须写明"跑了什么、结果是什么"。
- Integrator 串行合入；合入后复跑门禁；冲突 → 打回 `open` 并标冲突路径。
- **按风险配验证**：低风险 1 verifier + 自动 checks；中风险 2 verifiers 或 integrator 复跑；高风险 2-of-3 quorum + 人审。风险分由 paths / diff size / task_class / model tier 计算（§12.4）。
- 3 次不过 → `blocked` + 人工；下一次认领只带最近一次失败摘要。
- 质量指标：first-pass yield、rework rate、escaped defects、review 拒绝率、每交付项 token、cache 命中率、租约过期率。挂 `/api/v1/work/metrics` 和 `/ui`。

### 7.9 失败模式与对策

| 现象 | 原因 | 对策 |
|---|---|---|
| 多人抢同一项 | 同时 claim | CAS + 唯一 active claim；409 只回短原因 |
| 占坑不干 | 认领后不 heartbeat | 20m 租约 + 2m 心跳 + WIP=1 + 过期回收 |
| 重复劳动 | 两个项改同一片 | 路径前缀锁 + 产物哈希去重 + planner DAG |
| 冲突编辑 | 路径重叠 | claim 时 `path_locked`；合入串行；冲突打回 |
| 挑肥拣瘦 / 饿死 | 自认领偏好 | `priority+age` 排序、`stale` 标记、强制派工介入 |
| 评审走过场 | 同 agent / 同会话 | reviewer ≠ worker；证据必填；抽检 + 指标 |
| 上下文漂移 | 跨项复用会话 | 一工作项一会话；context pack 有上限；交接摘要 |
| token 爆炸 | 聊天 / 全量广播 / 重复贴 diff | 黑板；卡片 + 增量；引用；预算硬停 |
| 用质量换 token | 压缩过头 / 乱裁工具 | DoD + 门禁 + 独立评审；裁剪只在边界且可回滚 |

### 7.10 与现有设计的关系

- 复用 §2.5 的 agent key 作为 agent 身份（`AgentID = key.ID`），不新增认证。
- 复用 §2.6 `agents` 注册表拿 runtime 类型 / 能力；复用 §3.5 `AgentRuntime` 驱动执行。
- 工作板工具走 §4.3 的 MCP 面：`/mcp/{token}` 注册 `work_*` 工具，opencode / codex / CC 都能用；CLI 另给 `janus work`。
- 模型面保持 §3.4 严格透传，工作板只在控制面，不改请求体。
- 工作项绑定 workspace；默认不允许跨 workspace 认领。

### 7.11 实施顺序

| 阶段 | 内容 | 验收 |
|---|---|---|
| **W0 工作板** | 表（含 `ExecutionSite` / `RequiredModes`）+ `work_list/get/claim/heartbeat/release/submit` + CAS/租约 + 模式准入 | 两个 agent 并发抢同一项，只有一个拿到 lease；`none` 认领实现项被拒 |
| **W1 质量门禁** | 独立 verifier + `checks` 执行 + 证据必填 + attempts/人工升级 | 自己不能批自己；无证据提交被拒 |
| **W2 并行安全** | 依赖 DAG + 路径锁 + 合入队列 + `stale` 老化 | 两个改同文件的项不能同时 running |
| **W3 省 token** | context pack + 预算 + 成本分层 + 工具裁剪 + 缓存策略 | 每交付项 token 有基线，超预算能停 |
| **W4 兜底** | `work_assign` 强制派工 + `/ui` 看板 + metrics | admin 可强制派工，全程可审计 |

### 7.12 三种接入模式（native / bridge / none）

> 对应 §2.6 的 `agent.ToolMode`。工作板必须按模式分配工作项，否则会出现「纯问答模型认领实现项」或「客户端桥接超时被算到模型头上」。

| 模式 | 工具在哪执行 | 适合的工作项 | 主要成本 / 风险 |
|---|---|---|---|
| `native` | janus 主机、workspace 内；agent 用自带 read/write/bash | 服务端仓库实现、跑 checks、验证 | 服务器权限 / 沙箱；工具 schema 基线 |
| `bridge` | 客户端本机；agent 只调客户端声明的 tools，经 janus MCP 回环 | 文件 / 命令必须在用户机器上执行 | 工具声明 + MCP 注册 + 回填等待；超时 / 孤儿 / 串会话 |
| `none` | 不执行工具 | 规划、分诊、纯文本评审、文档 / 方案 | 不能跑 checks、不能改代码；只能做纸面评审 |

**工作项侧**：`ExecutionSite`（server / client）+ `RequiredModes` 由 planner 建项时定。

- 实现类默认 `native`；只有目标文件在客户端时才 `bridge`。
- 评审类优先 `native`（能在干净 workspace 复跑 checks）；`none` 只做首轮纸面评审，不能单独 approve 代码项。
- 规划 / 分诊 / 文档默认 `none`，这是最省 token 的用法。

**认领侧**：`work_list` 只返回模式匹配的项。

- `none` 不能认领实现项；
- `bridge` 不能认领服务端专有路径；
- `native` 不能认领只在客户端存在的路径；
- 强制派工可以越过分数 / tier，但**不能越过物理能力**：`none` 派实现项直接 400 `tool_mode_insufficient`；`bridge` 派不可见路径 400 `path_not_visible`。

**评审侧**：代码项的最终 approve 必须有工具能力（优先 `native`）的 verifier 复跑 checks；`none` 只能给 `needs_human` 或首轮意见，防止纸面放行。

**计费 / 打分侧**：三种模式的工具开销不同，成本必须按模式归一化，不能拿 `bridge` 的 token 去和 `native` 比。`none` 最便宜，但可能因为要贴 diff / 证据而增加上下文，算总账。

**失败归因**：bridge 超时、客户端不回填、MCP 404 不是模型的错；见 §8.5。连续 bridge 失败应把工作项改派 `native` 或转人工，而不是无限重试。

**指标**：按模式看 `claim → submit`、租约过期、工具超时、每交付项 token；bridge 的额外开销要单独可见。

### 7.13 明确不做

- 不做 agent 群聊 / 共享完整 transcript。
- 不做中央自动按负载派活；**强制派工永久保留**，由 admin 手动使用。
- 不做多人同时写同一工作项（要协作就拆子项）。
- 不做无证据的自动 `done`。
- 不在会话中途改工具集 / 压缩历史。

---

## 8. 模型打分与淘汰

> 自认领只解决「谁来干」，不解决「干得好不好」。
> 用**可验证的交付结果**给 `(agent, model, task_class, tool_mode)` 打分；分数只决定**能认领什么**，不负责派活。
> 同一模型在 `native` / `bridge` / `none` 下的成本和可靠性不同，不能混算。
> 连续不达标就降级 / 停权 / 淘汰；新模型有冷启动和探索配额。

### 8.1 六条原则

1. **结果打分，不看自述**：只看 verifier 结论、CI、attempts、租约、真实 usage，不看 agent 自己说完成了。
2. **按任务类 + 接入模式分别打分**：`task_class` 维度隔离，`native` / `bridge` / `none` 也隔离。Go 重构强不代表前端强，服务端原生跑得好也不代表客户端桥接稳。
3. **小样本不淘汰**：Wilson 下界 + 最小样本数；1～2 次失败只降权，不直接停权。
4. **质量权重压倒速度 / 成本**：防止「快而烂」拿高分；成本和周期只做次要项。
5. **可解释、可回滚**：每次分数变化都有事件；admin 可重置、复活、pin。
6. **保留探索**：固定比例低风险项留给 probation / 新模型，避免赢家通吃后没有新样本。

### 8.2 评分维度

| 维度 | 权重 | 来源 |
|---|---|---|
| 验收通过率（Wilson 下界） | 50% | `approve / attempts`，按 task_class 统计 |
| 一次通过率 | 15% | 无 reject 直接 approve |
| 可靠性 | 15% | 租约过期 / 放弃 / 超时 / 未 heartbeat |
| 成本 | 12% | 每交付项 token（含 cache read/write）相对预算 |
| 周期 | 8% | `claim → done` 中位时长相对目标 |

```
score = 100 * Σ(weight_i * normalized_i) - penalties
```

**先过质量闸门，再算加权**：样本 ≥ 10 时，验收点估计 ≥ 80%、一次通过率 ≥ 60%、可靠性 ≥ 85% 才参与排名；不达标直接 `probation`。排名里的验收通过率用 Wilson 下界，小样本自然被压低；这样低成本 / 短周期无法把质量问题补回来。

- 用最近 20 个样本的滑动窗口 + EWMA；样本 < 10 时向 task_class 全局均值收缩。
- `model_version` 进 key；模型升级 / 换版本 → 旧分数不继承，回 `probation`。
- 惩罚项：返工、超预算、越界改文件、证据缺失、评审争议。

**硬闸门（直接停权，不走加权）**：

| 事件 | 处理 |
|---|---|
| 伪造证据 / 跳过 checks | 立即 `suspended` + 人审 |
| 安全 / 数据事故 | 立即 `suspended` + 人审 |
| 连续 3 次 model 归因 reject | `suspended` |
| 连续 2 次 model 归因 reject，或 10 项滑窗分数低于阈值 | `probation` |
| 租约过期率 > 20%（≥ 10 次认领） | `probation` |

### 8.3 等级与准入

| tier | 可认领 | 限制 |
|---|---|---|
| `probation` | 低风险、预算 ≤ 50%、必须独立 verifier | 不接高优 / 安全项；占探索配额 |
| `active` | 普通工作项 | 正常认领 |
| `trusted` | 高难 / 高优 / 安全敏感 | 需 score ≥ 85、≥ 20 样本、验收 ≥ 90%、一次通过 ≥ 75%、无事故 |
| `suspended` | 不可自认领 | 可被 admin 强制派工，合入必须人审 |
| `retired` | 不可 | 需 admin 复活，回 `probation` |

- 新 `(agent, model, task_class)` 默认 `probation`。
- 晋升：连续 5 次 verified 且 score ≥ 70 → `active`；`active` 连续 10 次 ≥ 85 → `trusted`。
- 降级 / 淘汰自动执行，但**先通知、可申诉**；admin 可 pin 住某个组合。
- 高风险任务（auth / payment / 数据迁移 / 安全）只允许 `trusted`，且合入必须人审。

### 8.4 打分数据

```go
type ModelScore struct {
    AgentID       string `gorm:"primaryKey;size:64"`
    ModelID       string `gorm:"primaryKey;size:200"`
    TaskClass     string `gorm:"primaryKey;size:64"`
    ToolMode      string `gorm:"primaryKey;size:16"` // native|bridge|none
    ModelVersion  string `gorm:"primaryKey;size:64"`
    Tier          string `gorm:"index;size:16"` // probation|active|trusted|suspended|retired
    Score         int
    Samples       int
    Approvals     int
    FirstPass     int
    RejectStreak  int
    LeaseExpiries int
    CostMedian    int64
    CycleMedian   int64
    LastEventAt   int64
    UpdatedAt     int64
}

type ScoreEvent struct {
    ID         string `gorm:"primaryKey;size:64"`
    AgentID    string `gorm:"index;size:64"`
    ModelID    string `gorm:"index;size:200"`
    TaskClass  string `gorm:"index;size:64"`
    ToolMode   string `gorm:"index;size:16"` // native|bridge|none
    WorkItemID string `gorm:"index;size:64"`
    FailureOwner string `gorm:"index;size:16"` // model|harness|client|task|account|infra
    Type       string `gorm:"size:24"` // claim|submit|verdict|merge|incident|override|reset
    Delta      int
    Payload    []byte `gorm:"type:blob"`
    Actor      string `gorm:"size:64"`
    CreatedAt  int64
}
```

- `score_events` 只追加，可重算、可审计。
- 增量更新（每次 verdict / merge）+ 每晚全量重算，防止漂移。

### 8.5 防刷分

- **Verifier 反评**：verifier approve 后 7 天内被 integrator、CI 或线上推翻 → verifier 也扣分，防止抱团放水。
- **失败归因**：每次 reject 必须标 `model|harness|client|task|account|infra`。只有 `model` 计入模型分；bridge 超时 / MCP 404 / 客户端不回填记 `client` 或 `harness`，扣模式可靠性，不扣模型分；DoD 歧义记 `task`，先修 DoD；额度耗尽记 `account`，只影响账号健康分。
- **难度加权**：低难度项权重 0.5，高难度 1.5；只刷简单项无法晋升。
- **成本按上游 usage**：不按 agent 自报；返工消耗的 token 也算在被评者头上。
- **证据复跑**：verifier 在干净 workspace 跑 `checks`；随机 10% 人审。
- **分数不广播**：agent 只看自己的 tier / score，避免策略性挑活和刷分。

### 8.6 探索与冷启动

- 每个 `task_class` 保留 10% 低风险项给 `probation` / 新模型（可配）；同一工作项允许多模式时，按模式轮换探索，避免某个模式永远没样本。
- 模型版本变更给 3 次 `probation` 配额，不直接判死。
- 某 `task_class` 的 `active + trusted < 2` → 告警并**暂缓淘汰**，先补样本，避免无人可认领。

### 8.7 与自认领、强制派工的关系

- 分数只做 `work_list` 的准入过滤，**不改变「agent 自己选」**。
- **强制派工永久保留**，不受 tier / 淘汰影响；`work_assign` 可越过 tier 限制，即使目标处于 `suspended` 也能派，但必须 admin + 理由 + 人审合入，事件记 `forced_override`。
- 淘汰不是删 agent：只收回认领资格，workspace、历史、产物全部保留。

### 8.8 指标与告警

- 看板：按 task_class 的 leaderboard、score 趋势、每交付项 token、reject 原因 Top N。
- 告警：tier 容量不足、连续 reject、verifier 通过率异常偏高、成本异常、租约过期潮、某个模型版本分数断崖。

### 8.9 自治等级与信任预算

分数决定「能不能干」，自治等级决定「要不要人盯」。两者分开，避免高分模型被过度审查、低分模型完全停摆。

| 等级 | 权限 | 晋升条件 |
|---|---|---|
| L0 | 先交 plan，批准后才改 | 新组合默认 |
| L1 | 可改，逐 diff 审 | L0 连续 3 次通过 |
| L2 | 低风险可自动合入，事后抽检 | L1 连续 10 次通过 + score ≥ 80 |
| L3 | 可认领高风险，仍需人审 | L2 连续 20 次 + score ≥ 90 + 无事故 |

- **信任预算**：每个周期给 `(agent, model, task_class)` 一笔「未审变更额度」（按行数 / 项数）。verified 成功补充，reject / 事故扣减；额度用完自动回 L1。
- 自治等级只影响**审查强度**，不影响认领资格；认领资格仍由 tier + 模式 + 风险决定。
- 高风险项永远人审，不因 L3 放行。
- 这样「好模型越用越省审查成本，差模型自动收紧」，比静态 tier 更可持续。

### 8.10 实施顺序

| 阶段 | 内容 | 验收 |
|---|---|---|
| **S0** | `score_events` + 基础统计 + `probation/active/suspended` 三档 | 每次 verdict 能改分数并留事件 |
| **S1** | Wilson 下界 + 滑动窗口 + 自动升降级 + 通知 | 连续 3 次 reject 自动停权 |
| **S2** | 防刷分：verifier 反评、难度加权、随机人审 | 刷简单项无法晋升 |
| **S3** | 探索配额 + `trusted` + 高风险门控 + leaderboard | 新模型有活干，安全项只给 trusted |
| **S4** | 模型版本重置 + 申诉 / 复活 + admin pin | 升级后回 probation，可申诉 |
| **S5** | 自治等级 + 信任预算 + 自动合入低风险项 | L2 可自动合入，额度用完回 L1 |

---

## 9. 接入面与派生模型

> 三个入口，共用一套模型面 + 工作板：
> **Web 派活（服务器执行）**、**vicore / IDE / CLI 客户端接入**、**原生模型直连**。
> 客户端只看到派生模型名；后面可能是单 agent、agent 组，或一整块工作板。

### 9.1 三个接入面

| 入口 | 谁在用 | 执行位置 | 默认工具模式 | 同步性 |
|---|---|---|---|---|
| Web 控制台 `/ui` | 人 | 服务器 workspace | `native` | 异步：goal → 工作板 |
| 客户端 API（vicore / IDE / CLI） | 外部客户端 | 客户端或服务器 | `bridge` 或 `native` | `agent/` 同步；`team/` chat 同步流式、goals 异步 |
| 原生模型 API | 纯 LLM 客户端 | janus 直连厂商 | `none` | 同步 |

三种工具模式（§7.12）和接入面是**正交**的：Web 也可以用 `bridge`（客户端执行），vicore 也可以用 `native`（服务器执行）；关键看文件在哪、谁有权限。

### 9.2 派生模型名

```
janus                        # 默认（面板设置）
agent/<group>                # 单 agent 服务：同步 chat，内部可换成员/账号
team/<group>                 # 多 agent 工作板：chat 同步流式 / goals 异步
direct/<provider>/<model>    # 直连厂商：严格透传，无 agent、无工具
<provider>/<model>           # 等价 direct（兼容现有客户端）
```

- 客户端要指定模型：`agent/<group>` / `team/<group>` + 请求头 `X-Janus-Model: <provider>/<model>`；只允许组内成员，不允许越权。
- `direct/*` 和裸 `provider/model` 走 §3.4 的严格透传，**不进工作板、不建 agent 会话**。
- agent / team 别名默认回显**别名**（`model_echo=alias`），不泄露真实 provider / 账号；admin 可在 `/api/v1/traces` 看真实路由。

### 9.3 组与虚拟模型

```go
type AgentGroup struct {
    ID             string `gorm:"primaryKey;size:64"`
    Name           string `gorm:"size:120"`
    Kind           string `gorm:"size:16"`  // single|team
    Members        []byte `gorm:"type:blob"`// [{agent_id, model_id, tool_mode, weight}]
    Strategy       string `gorm:"size:16"`  // priority|least_load|quota|round_robin
    ToolMode       string `gorm:"size:16"`  // native|bridge|none
    AllowedModels  []byte `gorm:"type:blob"`
    DefaultModel   string `gorm:"size:200"`
    MaxConcurrency int
    BudgetTokens   int64
    Enabled        bool   `gorm:"index"`
}

type VirtualModel struct {
    Alias     string `gorm:"primaryKey;size:120"` // agent/dev, team/dev, direct/...
    Kind      string `gorm:"size:16"`             // agent|team|direct
    GroupID   string `gorm:"index;size:64"`
    ModelEcho string `gorm:"size:16"`             // alias|real
    Enabled   bool   `gorm:"index"`
}
```

- `/v1/models` 列出别名：能力取成员**交集**、context 取**最小值**、价格给**区间**，并标 `members=N`，避免客户端按最乐观成员选型。
- 同一个组里的成员必须同能力档位；强弱混在一个别名里会造成「同名模型质量随机」。
- `team/<group>` 的成员是**可认领工作项**的 agent；`agent/<group>` 的成员是**等价可替换**的单个 agent。

### 9.4 请求语义

| 别名 | 同步性 | 工具 | 返回 |
|---|---|---|---|
| `agent/<group>` | 同步 chat / stream | 组默认 `native` / `bridge` | 普通 completion；内部换成员 / 账号对客户端透明 |
| `team/<group>` | chat 路径同步流式（上限 10 分钟）；`/api/v1/goals` 异步 `202` | 默认 `native`；客户端 tools 只给**当前持轮的 worker** | 最终答案 + artifacts + 聚合 usage；进度走 SSE |
| `direct/...` | 同步 | 无 | 严格透传 |

- **team 的两条路径**：chat 路径同步流式（心跳保活，上限 10 分钟），适合短 / 中任务；超过上限或 Web 派活走 `/api/v1/goals` 异步，返回 goal_id 续查。不要无限期占用客户端连接。
- 取消 / 断线：取消当前 turn，不杀已完成的工作项；租约释放，检查点保留。
- team 模式下客户端声明的 tools **默认不注入**（多 agent 争用同一批工具会串 call id）；确需使用时只给当前持轮的 worker，`tool_call_id` 加 `w<item>:` 前缀命名空间。
- `X-Janus-Runtime: codex|claude-code` 仍可叠加，表示用哪个 CLI harness 驱动；它不是第四种工具模式。

### 9.5 Web 派活

- `POST /api/v1/goals {title, brief, workspace_id, group?, budget_tokens?}` → planner 拆工作项；`GET /api/v1/goals/{id}` 看进度；`GET /api/v1/goals/{id}/events` SSE。
- `/ui` 看板：goal → items → claims / reviews；人可以直接建项、强制派工、暂停、取消。
- Web 创建的 goal 归属创建者的 key / tenant；默认 `native`，工具在服务器 workspace 执行。

---

## 10. 账号池与额度耗尽切换

> 目标：同一模型多账号；积分 / 额度耗尽自动下一个账号；**只换账号，不换模型**；不丢 prompt cache；不误伤模型评分。

### 10.1 额度状态

```go
type CredentialQuota struct {
    CredentialID  string `gorm:"primaryKey;size:64"`
    ProviderID    string `gorm:"index;size:64"`
    Status        string `gorm:"index;size:16"` // ok|exhausted|rate_limited|invalid|unknown
    Remaining     int64
    Unit          string `gorm:"size:16"`       // credit|token|usd
    ResetsAt      int64
    CooldownUntil int64 `gorm:"index"`
    LastCheckedAt int64
    LastError     string `gorm:"size:512"`
}
```

另加 `account_events`（append-only）：`selected|exhausted|reset|invalid|probe_failed|switch`，用于对账和审计。

### 10.2 候选与选择

```
Route(model, key) -> []Candidate{
  ProviderID, CredentialID, UpstreamID,
  Pinned, Priority, RemainingRatio, ResetsAt, LastUsedAt
}
```

硬过滤：`Enabled`、key 的 `CredentialIDs` 白名单、模型支持、`Status != invalid`、冷却已过。

排序：**会话钉住的账号**（额度仍可用）→ `Priority` 升序 → 剩余额度比例降序 → `ResetsAt` 升序 → `LastUsedAt` 升序。

**不主动轮询做负载均衡**：切账号会丢 prompt cache，缓存损失通常大于均衡收益。只在硬错误时切。

- 切账号优先同 provider（prompt cache 不跨 provider）；`PrefixVersion` 不同的工作项不强行复用缓存，见 §12.6。

### 10.3 错误分类

| 上游信号 | 判定 | 动作 |
|---|---|---|
| 429 `insufficient_quota` / 余额不足 | `exhausted` | 记 `ResetsAt`，换**同模型**下一账号 |
| 429 rate limit | `rate_limited` | 短冷却（60s），换账号 |
| 401 / 403 | `invalid` | 停用 + 告警，换账号 |
| 5xx / 超时 | `infra` | provider 冷却；换 provider / 账号；不扣模型分 |
| 200 | `ok` | 更新用量；钉住会话 |

### 10.4 全部耗尽

| 策略 | 行为 |
|---|---|
| `wait`（默认） | 工作项 / goal → `blocked(quota_exhausted, resume_at)`；定时唤醒；**不计 attempts、不扣模型分** |
| `fallback_model` | 仅组 / 路由显式允许时；换等价模型，回 `X-Janus-Fallback-Model`，记录降级 |
| `fail` | 503 `all_accounts_exhausted` + `Retry-After` |

- **同模型**的定义：`UpstreamID` 完全相同。`deepseek-v4` 和 `deepseek-v4.1` 不自动视为同模型；要等价必须显式配 `model_routes`。
- 客户端指定了真实模型时，**绝不静默换模型**；只能换账号。
- 切账号后会话保留，只换上游 auth；标记 `cache_cold=true`，成本估算重算，不触发会话重建。

### 10.5 额度采集与并发

- 来源：provider usage API（有则用）、响应头、错误体；活跃账号 60s 刷新，空闲 5m；负缓存 30s。
- 本地 token / cost ledger 兜底，定期与 provider 对账。
- 单位统一为 credit / token / USD；重置时区按 provider 配置。
- 每账号信号量 + 每 provider 限流；OAuth refresh 用 singleflight，避免并发刷新把 refresh token 打废。

### 10.6 计费与公平

- 三维记账：`key/tenant → group → account`；每个 key / group 有预算。
- 账号池配额分配：防止一个 goal 或一个 agent 吃光所有账号；账号级并发上限。
- 告警：余额低、即将重置、账号失效、429 潮、切账号频率异常。

### 10.7 与工作板 / 打分的关系

- 额度耗尽 → `blocked`，不是 `reject`；不计入模型分。
- 失败归因新增 `account`：只扣账号健康分，不扣模型分。
- 强制派工仍受账号池约束；没有可用账号时进入等待，不静默失败。

---

## 11. 还需要补的（遗漏清单）

### P0：不做会返工

1. **归属与隔离**：goal 属于哪个 key / tenant；workspace 跨租户隔离；共享账号池的计费归属。
2. **别名契约**：`agent/` 同步、`team/` chat 同步流式 + goals 异步 202，客户端能否接受；`X-Janus-Model` 的优先级和越权边界。
3. **同模型定义**：只认 `UpstreamID`，还是允许跨 provider 的等价模型；谁有权配 `fallback_model`。
4. **切账号与会话**：切账号后是否保留会话（推荐保留，只换 auth）；缓存冷启动的成本上限。
5. **team + 客户端 tools**：默认关闭还是只给当前 worker；call id 命名空间；客户端能否处理多 agent 的工具回填。
6. **取消 / 断线**：goal → turn → 子工作项的取消传播；已完成工作项是否保留。

### P1：质量与安全

7. **Prompt injection**：仓库文件、工具输出、网页内容都不可信；agent 的权限最小化。
8. **Secret 脱敏**：模型面出口脱敏、工具参数还原、日志禁明文；脱敏失败拒绝发送。
9. **沙箱**：`native` 的文件/命令边界、网络出口、资源上限；高风险任务单独 workspace。
10. **证据可信**：客户端回传证据 vs `native` 复跑；抽检比例；伪造证据的停权。
11. **人审**：高风险任务双人确认；强制派工留痕；安全 / 数据事故升级路径。

### P2：运营与成本

12. **计费**：key / group / account 三维；重试是否重复计费；缓存读写计价；预算耗尽后的行为。
13. **队列与公平**：多 goal 争抢账号；优先级反转；队列上限；starvation。
14. **可观测**：`trace_id` 贯穿 goal → item → turn → request → account；token / 成本归因到 key / group / model / account。
15. **数据保留**：artifact / transcript / PII 的保留与清理；导出与备份。
16. **模型漂移**：版本 pin、canary、评分重置；同模型跨 provider 的质量差异。

### P3：边界

17. **合规**：账号共享是否符合供应商条款；数据驻留；审计留痕。
18. **多 region / 多 provider**：时钟偏差、额度重置时区、货币单位、价格差异。
19. **崩溃恢复**：claim / submit / merge 幂等；租约回收；重复提交；半成品产物。
20. **重试风暴**：指数退避 + 抖动 + 熔断 + 全局并发上限；429 时不要把所有账号一起打穿。

---

## 12. 差异化：不照抄的部分

> 上面的机制都能找到参考。只做「LiteLLM + CI Runner + Copilot 的拼装」会被替代；
> Janus 的护城河是把 **契约、胶囊、授权、归因、自治、缓存** 串成同一个交付闭环。

### 12.1 核心命题：交付账本，不是任务队列

- 别人：模型网关只管路由；CI Runner 只管执行；Copilot / Codex 只管单 agent 交付；MTurk / BOINC 只管众包验证。
- Janus：同一份账本记录 `key → goal → contract → claim → capsule → attempt → verification → account/model/tool_mode → cost → score`。
- 能回答：这行代码是谁、用哪个模型 / 账号、花了多少、谁验的、为什么被降级。别人拼不起来。

### 12.2 原语一：Work Contract（可协商契约）

- 工作项不是一段描述，而是一份契约：`goal / DoD / checks / risk / paths / budget / autonomy / capsule`。
- 认领前可以 `work_counter` 反提议：拆子项、改 checks、缩 paths、加预算；planner / verifier 批准后生效。
- 好处：把「理解错 → 干完被拒 → 重来」的 token 消耗前移到认领前。
- 与 CI Runner 的区别：Runner 不会 negotiation；与 MTurk 的区别：HIT 条款固定。

### 12.3 原语二：Context Capsule（有界上下文胶囊）

- 每个工作项维护一个版本化胶囊：目标、约束、不变量、已做决策、失败尝试、产物引用、开放问题。
- 上限 1200 token；超出必须归档 / 删除；**禁止把对话历史当交接**。
- `CapsuleID + version + base_commit` 跟工作项绑定；代码变了旧胶囊标 `stale`，重新生成 / 验证。
- 好处：多 agent 交接不碎片化（Cognition 的警告），token 可控，可审计。
- 与 MetaGPT 消息池的区别：消息池是 append-only 聊天；胶囊是**有界、可 diff、可失效**的状态。

### 12.4 原语三：Risk-Weighted Verification（按风险配验证）

- `risk = f(paths, diff_size, task_class, model_tier, account_history)`。
- 低风险：1 verifier + 自动 checks；中风险：2 verifiers 或 integrator 复跑；高风险：2-of-3 quorum + 人审。
- 验证成本进预算；不是所有项都堆 3 个 agent。
- 与 BOINC 固定 quorum 的区别：按风险动态分配；与 Copilot 的区别：不止人审 / CI。

### 12.5 原语四：Earned Autonomy（挣来的自治权）

- L0 先交 plan；L1 逐 diff 审；L2 低风险自动合入 + 抽检；L3 高风险仍需人审。
- 信任预算：每个周期给「未审变更额度」，verified 成功补充，reject / 事故扣减，用完回 L1。
- 好处：好模型越用越省审查成本，差模型自动收紧；不是永久淘汰。
- 与 MTurk Masters 的区别：按任务类 + 风险 + 预算动态调整，不是身份标签。

### 12.6 原语五：Cache-Aware Work Routing（缓存感知的派活）

- 每个 workspace 定义 **Prefix Contract**：system + repo facts + tool defs 的规范前缀，版本化、哈希。
- 工作项带 `PrefixVersion`；只有能命中同一前缀的 agent / model / account 才优先。
- 切账号优先同 provider（cache 不跨 provider）；切后标 `cache_cold`，成本重算。
- 这是 LiteLLM session affinity 的升级：从「session → deployment」变成「workspace prefix → provider/account 集合」，允许跨 agent 复用。
- 好处：多 agent / 多账号下仍吃 cache；这是省 token 的结构性来源，不是提示词小技巧。

### 12.7 原语六：Blame-Aware Ledger（可归因淘汰）

- 失败归因：`model|harness|client|task|account|infra`；只有 `model` 归因影响模型分。
- 别人淘汰模型时常把网络 / 客户端 / 账号问题算进去；Janus 把账算清，避免误杀。
- 这是「淘汰机制」长期可信的关键：**淘汰的是模型能力，不是基础设施噪声**。

### 12.8 原语七：Auditor-Gated Capability（模型审计授权）

- 敏感操作不是人点头，也不是 agent 自己说"我做了"，而是**独立审计模型 + 单次能力令牌**。
- Auditor 只看结构化 action、路径、命令、diff、契约，不看 worker 的自我辩解；失败 / 不确定即拒。
- 通过后签一个短 TTL、单次、绑定 `lease_epoch + action_hash` 的 token；执行器验 token 才放行。
- 与普通 LLM-as-judge 的区别：judge 只给意见；这里给的是**执行器能验证的 capability**；与人工审批的区别：人只保留给不可逆的窄集合。

### 12.9 组合起来是什么

```
Goal → Contract → Counter/Claim(plan) → Capsule → Authorize(auditor)
     → Execute → Verify(risk-weighted) → Deliver(branch/PR)
     → Ledger(attribution/cost) → Score/Autonomy → 下一次派活
```

- 单点都能找到参考；**闭环 + 归因 + 契约 + 胶囊 + 授权 + 缓存**的组合是护城河。
- 一句话：别人卖「更便宜的 token」或「更能干的 agent」，Janus 卖**「可解释、可复利、可淘汰的交付能力」**。

### 12.10 借鉴对照表

| 参考 | 借什么 | 不照抄什么 | Janus 的加法 |
|---|---|---|---|
| LiteLLM / OpenRouter | model group、order/fallback、cooldown、session affinity | 只做请求路由 | 工作契约 + 风险验证 + 淘汰账本 |
| GitHub Actions / Buildkite | labels/queue、ephemeral、并发组 | 只跑 job | 质量 / 风险 / 缓存感知的准入 |
| Copilot / Codex | 异步、沙箱、branch/PR、评审迭代 | 单 agent、单厂商 | 多 harness / 多账号 / 自认领 |
| MTurk / BOINC | 资格、approval rate、quorum | 人工任务、固定 quorum | 机器可验契约 + 风险加权 |
| Anthropic / Cognition | orchestrator、上下文隔离 | 紧耦合编码也硬拆 | decomposition gate + capsule |
| Agentless | 简单 pipeline | 只做单点修复 | `pipeline` 作为工作项类型 |

### 12.11 反模式（明确不做）

- 不做「所有任务都上多 agent」：紧耦合单线程，pipeline 处理小修。
- 不做 agent 自由群聊：只走胶囊 + 结构化产物。
- 不做「模型总分」：按 `(agent, model, task_class, tool_mode)` + 归因。
- 不做「淘汰即删除」：只收回认领资格 / 自治等级，可复活。
- 不做「为了均衡丢缓存」：缓存亲和优先于负载均衡。
- 不做「无证据自动 done」：风险加权验证 + 证据复跑。

---

## 13. 人工介入策略与敏感操作授权

> 最终目标：一个项目从接需求到交付，**人工介入趋近于零**。
> 人只出现在三种情况：**开发确实卡死**、**不可逆高风险操作**、**审计模型分歧/不确定**。
> 敏感操作默认由**专门审计模型 + 单次能力令牌**授权，不默认找人点头。

### 13.1 六条原则

1. **人工是异常路径，不是流程节点**。流程里没有"等人审批"这一步。
2. **能用确定性策略判的，不调模型；能用审计模型判的，不找人。**
3. **授权不是一句话，是能力令牌**：短 TTL、单次、绑定动作哈希与租约代数。
4. **审计模型独立于执行模型**：不同 provider / model / session，只读，无写工具。
5. **审计失败 / 不确定 → 拒绝或升级，绝不默认放行。**
6. **每次人工介入都必须沉淀**：变成规则、契约或测试；同一类问题不允许第二次找人。

### 13.2 人工介入的窄口径

| 触发 | 处理 |
|---|---|
| 需求歧义 / DoD 无法机器验证 | 人工澄清一次，写回契约 |
| 连续 3 次 reject / 租约反复过期 | 人工判断拆项 / 换模型 / 改契约 |
| 环境缺失（凭据、权限、外部系统） | 人工补环境 |
| 不可逆生产操作（删库、force push 主分支、生产销毁） | 人工最终确认（唯一默认人审类） |
| 审计模型分歧 / 置信度低 | 人工裁决 |
| 安全事件 / 疑似数据外泄 | 人工介入并冻结 |

除此之外全部自动。**"人工接入"不是交付步骤，是故障处理。**

交付验收同样默认自动：`checks + verifier + auditor` 全绿即合入；人只读异常摘要，不做逐行验收。

### 13.3 敏感操作分类

| 类别 | 例子 | 默认授权 |
|---|---|---|
| `fs.write` | workspace 外写入、`.git/`、CI 配置、密钥文件 | 1 个 Auditor |
| `exec` | `rm -rf`、`DROP TABLE`、`kubectl delete`、`terraform destroy` | 2 个 Auditor；不可逆加人审 |
| `net.egress` | 上传、未知外网、webhook | 1 个 Auditor |
| `secret.read` | 读 `.env`、凭据文件 | 1 个 Auditor |
| `secret.write` | 改密钥、轮换凭据 | 2 个 Auditor + 人审 |
| `git.push` | force push、protected branch | 2 个 Auditor + 人审 |
| `deploy` | 生产发布、回滚 | 2 个 Auditor + 人审 |
| `db.migrate` | schema / 数据迁移 | 2 个 Auditor；破坏性变更加人审 |
| `auth` | 权限、鉴权、支付逻辑 | 2 个 Auditor + 人审 |
| 普通读写 / 单测 | workspace 内读、跑测试 | 确定性 allow，不调模型 |

### 13.4 授权流程

```
Agent → auth_request(action, paths, diff_hash, reason)
      → Policy Engine（确定性：路径 / 命令 / 黑名单 / 风险）
      → Auditor（1 或 2 个独立模型，结构化输入）
      → allow: Capability Token
      → deny: 原因 + 可重试建议
      → needs_human: 只给 13.2 的窄集合
      → Executor 校验 token（动作哈希 / 路径 / TTL / 单次）
      → 执行
```

- Agent 拿不到"口头批准"，只拿到 token；**没有 token 的执行一律拒绝并记 incident**。
- `reason` 视为不可信输入；Auditor 主要看结构化字段、diff、命令，不看 agent 自述。
- token 声明：`work_item_id + lease_epoch + action_hash + paths + expires_at(60s) + nonce`；单次使用。
- `native` 由 janus 侧工具包装 / 沙箱校验；`bridge` 由客户端回调 janus introspection 校验；`none` 直接拒。

### 13.5 审计模型（Auditor）

- **独立**：不能与 worker 同 `(provider, model, session)`；高风险用不同模型族。
- **只读**：没有写 / 执行工具，只能查 policy 与静态证据。
- **结构化输入**：`action / target_paths / command / diff_hash / contract / risk / policy_version`；不含 worker 完整对话。
- **结构化输出**：`allow|deny|needs_human` + `policy_refs[]` + `evidence[]` + `confidence`。
- **Rubric**：是否在契约路径内、是否可逆、是否触碰敏感资产、是否有测试证据、是否与批准 plan 一致、是否违反策略。
- **失败即拒**：超时、解析失败、置信度低于阈值 → deny / escalate，不 allow。
- **单独打分**：false allow / false deny / 人工推翻率；不混入 worker 模型分。

### 13.6 授权矩阵

| 风险 | 确定性策略 | 审计 | 人 |
|---|---|---|---|
| low | allowlist / denylist | 不调 | 否 |
| medium | 路径 / 命令规则 | 1 个 Auditor | 否 |
| high | 规则 + 证据 | 2 个独立 Auditor，必须都 allow | 分歧时 |
| critical | 规则 + 证据 + 回滚方案 | 2-of-3 Auditor | 不可逆才人审 |

- 低风险不调模型，省 token；高风险才加审计。
- 审计决策按 `action_hash + policy_version + contract_version` 缓存；token 每次现签、单次使用。

### 13.7 与工具模式的关系

- `native`：janus 能在服务器侧强制拦截 `write / exec / net`，token 校验最可靠。
- `bridge`：执行在客户端，janus 只能要求客户端回调校验；客户端不可信时，敏感操作强制走 `native` 或人审。
- `none`：不能执行敏感操作，直接拒。

### 13.8 指标

- `human_touches_per_delivered_goal`：目标趋近 0；每次 >0 必须归因并沉淀规则。
- `audit_allow / deny`、false allow、false deny、人工推翻率。
- 审计延迟、每审计 token 成本。
- 敏感操作 incident 数、无 token 执行次数（应为 0）。

### 13.9 已定决策

| 项 | 决定 |
|---|---|
| `work_counter` 批准权 | 低风险 planner 即可；中 / 高风险 planner 初判 + verifier 复核；改 DoD / checks 必须 verifier |
| Context Capsule 上限 | 默认 1200 token；docs / tests 800；infra / security 1600；硬上限 2000 |
| L2 自动合入范围 | 先只开放 docs / tests / config-example / 低风险 allowlist；≤ 200 行；全部 checks 通过；不含敏感操作。其余仍 L1 |
| 敏感操作默认授权 | 确定性策略 + Auditor + 单次能力令牌；人审只保留不可逆 / 分歧 |
| 人工介入目标 | 以 `human_touches_per_delivered_goal` 计量，持续逼近 0；每次人介入必须沉淀 |

---

## 14. 待确认

1. **admin key 引导**：首次启动自动生成并打印（推荐），还是强制 `janus init` 子命令？
2. **旧库处理**：检测到旧表直接拒绝启动（推荐，符合「不兼容」），还是自动备份后重建？
3. **agent key 粒度**：一个 key 通吃所有 provider（带白名单），还是每 provider 一个？
4. **`/v1/models` 可见性**：agent key 只看到白名单内模型（推荐），还是全量？
5. **前端是否接受构建步骤**：零构建 Preact（推荐）vs Svelte + Vite。
6. **认领是否强制一句话 plan**：推荐是（≤ 80 token），能逼出思考，也给 verifier 上下文。
7. **同一 agent 的 WIP 上限**：推荐 1 个 `running` + 1 个 `review`，避免占坑和上下文漂移。
8. **Verifier 是否允许同模型族**：推荐至少不同 agent；同模型族的独立评审仍有价值，但容易同盲区。
9. **Integrator 自动合入还是人工点一下**：推荐先人工确认，稳定后再按路径白名单自动合入。
10. **预算单位**：token 计量（可对上上游 usage）+ 美元展示，避免不同模型单价把预算算歪。
11. **工作项默认粒度**：8 文件 / 400 行是否合适；是否需要按仓库类型（前端 / Go / 基础设施）分档。
12. **质量闸门阈值**：验收 ≥ 80%、一次通过 ≥ 60%、可靠性 ≥ 85%（样本 ≥ 10 后生效）是否合适？权重 50/15/15/12/8 先按这个跑，观察两周再调。
13. **样本门槛**：最小样本 10、晋升连续 5 次、`trusted` 20 次，是否太慢或太快。
14. **探索配额**：10% 低风险项是否够？「低风险」按什么定义（路径白名单 / 无 auth / 有回滚）。
15. **强制派工审批级别**：admin 单人即可，还是高风险项（auth / 支付 / 数据迁移）需要双人确认？强制派工本身永久保留，不再讨论是否保留。
16. **Verifier 反评窗口**：7 天是否合适？线上缺陷可能更晚暴露，是否需要 30 天慢通道。
17. **打分粒度**：按 `(agent, model, task_class, tool_mode)` 还是先按 `(model, task_class, tool_mode)` 聚合？前者公平，但样本更稀疏。
18. **模式分数是否合并**：`native` / `bridge` / `none` 分开打分（推荐），还是同一模型跨模式共享分数？
19. **失败归因谁判**：verifier reject 时初判，integrator / 人工抽检复核？推荐前者，避免归因成本过高。
20. **同步性契约**：`agent/<group>` 同步；`team/<group>` chat 路径同步流式（上限 10 分钟）、goals 路径异步 202。这个双轨是否接受？
21. **同模型 fallback**：跨 provider 的等价模型是否允许自动切换？推荐默认只换账号、不换 provider，除非显式配置。
22. **账号全耗尽**：默认 `wait` 还是 `fail`？`wait` 时 goal 暂停、恢复后继续，是否符合预期。
23. **切账号后的会话**：保留会话只换 auth（推荐），还是重建会话？缓存冷启动成本谁来承担。
24. **team + 客户端 tools**：默认关闭（推荐，避免多 agent 串 call id），还是只给当前持轮 worker。
25. **账号池计费归属**：按 key / tenant，还是按 group？共享池的账怎么分。
26. **别名能力聚合**：取成员交集 / 最小值（推荐，防客户端按最乐观成员选型），还是以首个成员为准。
27. **风险权重**：paths / diff_size / task_class / model_tier / account_history 的权重怎么定？建议先人工标 50 个样本校准。
28. **投机执行**：`Replicas > 1` 只用于高风险 / 高不确定项？单次预算上限多少。
29. **Prefix Contract**：workspace 前缀变更谁批准？变更后是否强制重跑所有打开的工作项。
