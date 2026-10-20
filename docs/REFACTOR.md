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
>    `upstream.Upstream` + EventBus；`opencodeTurnSession` 是当前唯一实现，行为与
>    原路径一致。**仍待做**：CLI runtime 尚未接进 `/v1/chat/completions`——除事件流
>    外还差对账（`ListMessages` 无对应物）、会话状态（`GetSession` / `WaitUntilIdle`）
>    与 tool bridge 生命周期；MCP 绑定已改为确定性解析（见上）。

---

## 7. 待确认

1. **admin key 引导**：首次启动自动生成并打印（推荐），还是强制 `janus init` 子命令？
2. **旧库处理**：检测到旧表直接拒绝启动（推荐，符合「不兼容」），还是自动备份后重建？
3. **agent key 粒度**：一个 key 通吃所有 provider（带白名单），还是每 provider 一个？
4. **`/v1/models` 可见性**：agent key 只看到白名单内模型（推荐），还是全量？
5. **前端是否接受构建步骤**：零构建 Preact（推荐）vs Svelte + Vite。
