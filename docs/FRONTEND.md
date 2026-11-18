# 前端架构

> 参考 `/opt/tvgate/ui` 的工程方式（React + TS + Vite + Tailwind + hash router + Go embed），
> 但按 Janus 的使用场景重写：多模型/多凭据管理、远端工作进度与输出查看、人工兜底操作。

## 1. 为什么不再是单页

早期 `/ui` 是一个手写 HTML + 原生 JS，只够「看一眼工作项」。接下来要承载：

- **很多模型的接入**：provider / 端点 / key 池 / 别名 / 额度状态，后续还有 OAuth 登录；
- **远端工作**：goal 进度、工作项租约、评审、日志与产物输出；
- **人工兜底**：建项、强制派工、取消、pin 降级；
- **多角色**：只读观察者、worker、admin 看到的操作不同。

这些需要路由、复用组件、表单校验和可维护的类型层，单文件脚本会成为负担。

## 2. 技术栈

| 层 | 选择 | 说明 |
|---|---|---|
| 框架 | React 19 + TypeScript 5.7 | 与参考项目一致，生态最稳 |
| 构建 | Vite 6 | 产物直接写进 Go embed 目录，Go 构建不需要 Node |
| 样式 | Tailwind CSS 4 + `@tailwindcss/vite` | 主题写在 CSS 的 `@theme`；无 PostCSS / autoprefixer 配置 |
| 路由 | react-router-dom 7，`createHashRouter` | 静态托管无需服务端 rewrite；路由级 lazy chunk |
| 状态 | Zustand 5 | 只放认证与主题；服务端数据留在页面内，避免过早引入全局缓存 |
| 图标 | lucide-react | 与参考项目一致 |
| 组件 | `src/components/ui.tsx` | 轻量设计系统（Button/Input/Field/Badge/Card…），不引 UI 框架 |

设计文档里写的是 Preact/Svelte；这里选择 React 是**有意偏离**：参考实现、组件生态和团队熟悉度更重要，
产物体积在可接受范围（首屏 JS gzip ≈ 110 KB）。

## 3. 目录

```
web/
├── index.html              # 主题预置脚本（避免刷新闪白）
├── package.json / tsconfig.json / vite.config.ts
└── src/
    ├── main.tsx / App.tsx
    ├── router/index.tsx    # hash 路由 + lazy
    ├── layouts/AppShell.tsx# 侧栏、主题、认证守卫
    ├── views/
    │   ├── overview/Dashboard.tsx
    │   ├── goals/{Goals,GoalDetail}.tsx
    │   ├── work/{Items,ItemDetail}.tsx
    │   ├── models/Models.tsx
    │   ├── insight/{Usage,Scores}.tsx
    │   └── system/Login.tsx
    ├── components/ui.tsx   # 设计系统原语
    ├── api/                # 按领域拆的请求函数 + types.ts
    ├── stores/{auth,theme}.ts
    └── lib/{api,cn,format}.ts
```

## 4. 路由与布局

- **hash 路由**：`#/goals`、`#/items/{id}`。Go 只提供 `index.html` 与 hashed assets，
  不需要 history fallback，也不会出现刷新 404。
- **懒加载**：每个 view 一个 chunk，首屏只加载当前模块。
- **布局守卫**：`AppShell` 检查本地 key，没有就 `<Navigate to="/login">`；
  登录页用一次只读请求验证 key。
- 主题在 `index.html` 内联脚本里先应用，避免深色模式下闪白。

## 5. 数据与状态

- **服务端状态不全局化**：页面自己 `useState` + `useEffect` 拉取；动作后局部刷新。
  数据量小、页面独立，过早引入缓存层只会增加失效逻辑。
- **轮询**：goal 详情在非终态时每 2 秒刷新一次，终态自动停止。
  后端也提供 SSE（`/goals/{id}/events`），但 `EventSource` 不能带 header；
  页面统一用轮询，SSE 留给能设 header 的客户端。
- **类型镜像**：`src/api/types.ts` 与 Go 的 JSON 字段一一对应；
  改后端字段必须同步这里（没有代码生成，保持小而显式）。

## 6. 认证与权限

- 登录只保存 API key（`localStorage`，`janus-auth`），服务端无会话、无 Cookie。
- 请求统一走 `lib/api.ts`：带 `X-API-Key`；401 清空并跳登录，403 保留（只是缺 scope）。
- 当前 UI **不解析 scope**，管理员操作失败时展示后端返回的 `forbidden`。
  下一步加 `GET /api/v1/me` 返回 `{agent_id, scopes}`，据此隐藏/禁用按钮。

## 7. 多模型与凭据

后端已经落地（`internal/provider` + `/api/v1/providers`、`/api/v1/model-routes`）：

1. `model_providers`：`id / name / kind(openai|anthropic|custom) / base_url / enabled`；
2. `provider_credentials`：`id / provider_id / label / kind(api_key|oauth) / secret（AES-GCM 密文）/ status / total / remaining`；
3. `model_routes`：`exposed_model / provider_id / upstream_model / priority / aliases`；
4. secret 只在服务端解密；创建与列表都只回 `masked`，`SecretCiphertext` 带 `json:"-"`；
5. 启动时把库中路由并入 `/v1`，同名时 `JANUS_MODELS` 优先。

### 控制台页面

`/ui/models` 是一个管理页，四块内容：

- **客户端可见**：只读展示 `/v1/models`，也就是调用方实际能请求的名字；
- **上游**：列表（ID / 协议 / Base URL / 启停）、新增、启停、删除，以及一键**探测**
  （`GET /models`，不花 token，结果直接写回该行：可用 + 延迟 + 模型数，或不可用）；
- **凭据**：按上游筛选，列表只显示 `masked` 与状态；录入是一次性的，提交后输入框立即清空；
  `openai` / `anthropic` 类型还提供 **OAuth 登录**：点按钮打开厂商授权页，粘回授权码即完成；
- **路由**：暴露模型 → 上游 + 上游模型的映射，别名与优先级可填，支持删除。

权限来自 `GET /api/v1/me`：没有 `admin` scope 的 key 会看到只读提示，增删与启停按钮全部禁用——
不是点了才 403。服务端未设置 `JANUS_MASTER_KEY` 时，页面顶部给出说明与生成密钥的命令，
其余区域保持空态，不会把 501 渲染成红色错误。

### 保存即生效

增删上游、凭据或路由后，服务端立即重建路由表并原子替换（`proxy.Server.SetRoutes`），
无需重启：在途请求保留已解析的 client，新请求看到新表。重载失败（例如某个密钥解不开）
只记日志，写入不回滚，旧表继续服务；控制台的「探测」按钮就是用来发现这种问题的。

### OAuth 登录与多账号

参考 sub2api 与 magpie 的做法，采用**授权码 + PKCE + 粘回授权码**，而不是在本机起回调监听：

- `openai-codex` 走 Codex CLI 的客户端（`auth.openai.com`，回调固定 `localhost:1455`，
  janus 部署在远端时浏览器无法回连，所以必须粘贴）；未选渠道的老上游按 `kind=openai` 命中同一流程；
- `claude-code` 走 Claude Code 的客户端（`claude.com/cai/oauth/authorize`），授权后官方回调页
  直接显示 `code#state`，粘贴即可；
- 会话在内存里、10 分钟有效、一次性；state 不匹配直接拒绝；
- 令牌以 JSON 密文存储（access / refresh / expires_at），**距到期 2 分钟内自动刷新**并写回，
  刷新失败保留旧令牌，由上游 401 暴露问题；
- 控制台拿不到任何令牌，只能看到 `masked`。

**同一个渠道可以有多个账号**。每个 `provider_credentials` 行就是一个账号，`label / account /
plan / priority / disabled` 是运营字段，令牌仍是密文。`POST .../oauth/complete` 每次都新增一行；
带 `credential_id` 调 `oauth/start` 则是**重新授权**，完成后原地更新，不新增、不改优先级。

调度规则（`internal/pool`）：

1. `disabled` 与 `status=invalid` 的账号不参与；
2. 按 `priority` 升序，其次剩余额度、重置时间、最久未用；
3. 同一会话（`X-Session-Affinity` / `X-Session-Id` / `X-Conversation-Id` /
   `X-Magpie-Session` / `prompt_cache_key`）**粘住同一个账号**，保住 prompt cache；
   账号失效时才切换，并把新账号绑定到该会话；
4. 401/403 → `invalid` 并停用；402/额度 429 → `exhausted` 并记 `resets_at`；
   普通 429 → `rate_limited` + 冷却；5xx → 短暂冷却；其它 4xx 不归咎账号；
5. 结果异步写回 `provider_credentials`（status / cooldown / resets_at / last_error / last_used_at），
   **重启后不会把耗尽账号当可用**。`POST /api/v1/credentials/{id}/reset` 手动清除状态；
6. `PATCH /api/v1/credentials/{id}` 改备注、优先级或启停；「上移/下移」在控制台里交换两行优先级。

### 自定义 OAuth 与订阅渠道

`provider.oauth`（`model_providers.oauth_json`）允许每个上游自带一套**公共客户端**流程：
`client_id / authorize_url / token_url / redirect_uri / scopes / extra_auth_params /
token_style(form|json) / verifier_encoding(hex|base64url)`。它优先于内置流程，因此
WorkBuddy、CodeBuddy、Qoder、Cursor、Kiro、ZCode、美团 LongCat 这类渠道可以由运营方
用自己的 client 接入，而 janus 不内置别人的 client id，也不承担对应的封号风险。

不内置的原因很实际：magpie 已把 WorkBuddy、Cursor、Kiro、Qoder、ZCode、Grok、Antigravity、
Command Code、Devin、Factory、MiMo、Zed 等订阅迁到第三方 OpenCode 插件，并注明
"reaching them can break their vendors' terms"。janus 只提供机制，不代替运营方做这个决定。

各渠道差异：

- `workbuddy` / `workbuddy-ai`（腾讯 CodeBuddy 套餐）：OpenAI chat 协议，
  但鉴权头与刷新端点是厂商私有的（`/v2/plugin/auth/token/refresh`）；
  当前只保证**导入的 access token 在有效期内可用**，到期后需要重新导入/授权；
- `opencode-zen` / `opencode-go`：API key 或 OAuth 均可；OpenCode 自己的 `auth.json`
  可以用下面的导入功能直接复用；
- `longcat`（美团）：OpenAI + Anthropic 兼容，API key，不是 OAuth。

### 导入本机登录

`POST /api/v1/providers/{id}/import`（admin）读取服务端主机上已有的 CLI 登录：

| source | 文件 | 说明 |
|---|---|---|
| `codex` | `~/.codex/auth.json` | 取 `tokens.access_token / refresh_token / id_token` |
| `claude` | `~/.claude/.credentials.json` | 取 `claudeAiOauth.*`，毫秒/秒到期时间都兼容 |
| `opencode` | `$XDG_DATA_HOME/opencode/auth.json` 或 `~/.local/share/opencode/auth.json` | 只取 `type=oauth` 且带 access 的条目；`entry` 指定 provider key |

导入按 `account`（id_token 里的 email、OpenCode 条目名）去重：重复导入更新同一行，
不会长出第二个账号。令牌只在服务端解封，接口返回的仍是 `masked`。
和 CC Switch 的 Accounts 面板思路一致，但 janus 没有把凭据写回 CLI 的配置。

还没做：设备码流程（Copilot）、WorkBuddy 等厂商私有刷新协议、精确的 token 计数。

### 协议适配（chat ↔ Responses / Messages）

调用方永远只说 OpenAI chat completions；上游说什么由 `model_providers.protocol` 决定：

| protocol | 上游 | 端点 |
|---|---|---|
| `chat`（默认） | 任何 OpenAI 兼容端点 | `{base}/chat/completions` |
| `responses` | OpenAI Responses / Codex 后端 | `{base}/responses`（`/backend-api/*` 不加 `/v1`） |
| `anthropic` | Anthropic Messages / Claude Code | `{base}/v1/messages` |

`protocol` 为空时按渠道推断：`openai-codex` → `responses`，`claude-code` 或 `kind=anthropic` →
`anthropic`，其余 `chat`。控制台新建上游时可显式覆盖。

翻译层在 `internal/gateway`，三种协议共用同一套选号、失败切换与用量采集：

- **请求**：system 提为 `instructions`（Responses）或 `system`（Messages）；OpenAI `tools` 映射为
  Responses 的 `{type:function,...}` 或 Messages 的 `input_schema`；`assistant.tool_calls` 映射为
  Responses 的 `function_call` 条目 / Messages 的 `tool_use` 块；`role:tool` 映射为
  `function_call_output` 或 `tool_result`。Messages 要求 `max_tokens`，缺省给 8192。
- **响应**：文本、`tool_calls`、`finish_reason`（`max_tokens`→`length`、`tool_use`→`tool_calls`）、
  `usage.prompt/completion` 与 cached tokens 全部还原成 OpenAI 结构。
- **流式**：把上游 SSE 事件重新打包成 `chat.completion.chunk`，末尾补 `[DONE]`；调用方看到的是
  标准 OpenAI 流，不会漏出 `content_block_delta` 或 `response.output_text.delta`。
- **Codex 只支持流式**：所以对 `responses` 的非流式请求由网关内部读流并聚合，再返回一次性 JSON。
- **鉴权**：`anthropic` + API key 用 `x-api-key`，OAuth 用 `Authorization` + `anthropic-beta:
  oauth-2025-04-20`；`responses` + OAuth 额外带 `OpenAI-Beta: responses=experimental`、
  `originator: codex_cli_rs` 与从 access token 里解出的 `chatgpt-account-id`。
- **入站也支持 Messages**：`POST /v1/messages` 让 Claude Code 这类客户端直接指向 janus；
  网关把 Messages 转成 chat 调用路由，再把结果转回 Messages。流式按 Anthropic 事件序列
  （`message_start` → `content_block_*` → `message_delta` → `message_stop`）输出，
  `stop_reason` 与 `tool_use` 一一对应；认证接受 `x-api-key`，错误用 Anthropic 信封。
  `POST /v1/messages/count_tokens` 返回按字符估算的 `input_tokens`（不调用上游分词器）。
- 未翻译的字段（音频、文件、reasoning 配置）被丢弃而不是原样透传：上游报 400 比静默忽略更难查。

测试覆盖请求/响应/流式三条路径、OAuth 头、配额失败后的换号，以及 `provider → proxy` 的端到端。

### 渠道目录

`GET /api/v1/channels` 返回静态渠道表（`internal/provider/catalog.go`），分四类：
`vendor`（厂商直连）、`subscription`（OAuth 订阅套餐）、`relay`（聚合/中转）、`local`（自建网关）。
每条包含 `kind / protocols / auth / base_url / alt_base_urls / note`，覆盖 OpenAI、Anthropic、
Google、DeepSeek、Kimi、GLM、MiniMax、Qwen、火山、混元、千帆、SiliconFlow、OpenRouter、
Vercel、OpenCode、xAI、Groq、Together、Fireworks、Cerebras、Ollama、LiteLLM、WorkBuddy、
CodeBuddy、美团 LongCat 等约 55 个入口；
`protocols` 如实标注上游原生协议，`kind` 只表示 janus 当前能直接说的协议，避免把
"上游支持 Responses" 误渲染成"网关已支持 Responses"。

新增上游时选渠道会自动填 `id / name / kind / base_url` 并可继续改；`channel` 只存 id，
路由与鉴权仍只看 `kind + base_url`。目录是静态数据，双方不共享代码，仅对齐了端点与认证方式。

### 余量（quota）

`POST /api/v1/providers/{id}/quota` 逐个凭据读取厂商额度并落库（`provider_credentials.quota`）：

| 渠道 | 读取方式 |
|---|---|
| DeepSeek | `GET /user/balance` |
| Moonshot / Kimi | `GET /v1/users/me/balance` |
| OpenRouter | `GET /api/v1/credits`（总额 - 已用） |
| Claude Code（OAuth） | `GET api.anthropic.com/api/oauth/usage` + `anthropic-beta` |
| Codex（OAuth） | `GET chatgpt.com/backend-api/wham/usage` |

`Quota` 用统一结构承载：`balance/currency`、`windows[]`（`used_percent / used / limit / unit /
resets_at / text`）、`plan`、`checked_at`、`error`。厂商只给百分比就只存百分比，**不推算 token 数**；
读失败保留旧快照并记 `error`，控制台显示"读取失败"而不是 0。没有额度接口的渠道返回说明文字，
不伪装成成功。`GET /api/v1/quotas?provider=` 返回已存快照，不触发厂商请求。

### 路由同步

`POST /api/v1/providers/{id}/sync-routes` 用第一个可用凭据读上游 `/models`，补齐缺失路由：

- 已有同一 upstream 的 provider 路由 → 计入 `existing`，不重复建；
- 暴露名已被**其它**上游占用 → 计入 `conflicts`，绝不抢占；
- 上游不再提供的模型 → 计入 `stale`；只有请求带 `{"prune": true}` 才删除；
- `{"skip": ["janus","default"]}` 用于忽略上游网关自带的别名；
- 响应 `{total, created, existing, conflicts, skipped, stale, pruned, credential_id, masked}`。

控制台的「同步模型」按钮使用该接口（默认不删），有 `stale` 时才出现「清理过期」。
探测与同步共用同一次 `/models` 请求格式；`model_count` 只统计非空、去重后的 id。

### 用量统计

每次模型调用（成功与失败）都写入 SQLite `usage_entries`：模型、上游模型、provider、凭据、
prompt/completion/cached tokens、耗时、HTTP 状态、错误码、是否流式。旧的进程内账本已被替换，
重启不再丢历史。

- `GET /api/v1/usage`：区间汇总（兼容旧接口）；
- `GET /api/v1/usage/stats?period=24h|7d|30d&provider=&model=&key=&bucket=hour|day`：
  一次返回 `totals + by_model + by_provider + by_key + trend`，趋势在 SQL 里分桶；
- `GET /api/v1/usage/logs?limit=50`：最近调用流水，用于定位某个 key/model 的尖峰。

`/ui/usage` 用这些接口渲染：周期切换、失败数、token/成本卡片、趋势条、按模型/上游拆分、最近调用表。
失败调用同样计入行数，`failed` 单独计数，错误率不需要另外一套监控表。

## 8. 远端工作与输出

工作项详情已经能看：

- 当前租约：agent / role / epoch / 到期 / 是否 forced；
- 提交证据：`diff_hash`、产物引用、每个 check 的 `exit_code` 与 `output_tail`；
- 评审记录：verdict、理由、复跑 checks 的输出尾部。

下一步补：

- `GET /api/v1/work-items/{id}/artifacts`（store 已有 `work_artifacts` 表和查询方法，只差服务层暴露）；
- 产物下载/预览路由（只允许 workspace 内、带 scope 校验）；
- 长任务日志的 SSE（按工作项订阅，带 header 或短期 ticketed URL）。

## 9. 构建与嵌入

```bash
make ui    # cd web && npm ci && npm run build
```

Vite 配置 `base: "/ui/"`、`outDir: "../internal/ui/dist"`，Tailwind 走
`@tailwindcss/vite`（不再有 `postcss.config.js`，也不再装 autoprefixer：v4 用 Lightning CSS 处理前缀）。
`internal/ui` 用 `//go:embed all:dist` 打包，因此：

- `go build` / `go test` **不需要 Node**（dist 已提交）；
- 改了 `web/` 必须 `make ui` 并提交 dist，否则二进制里还是旧页面；
- `index.html` 用 `no-store`，hashed assets 用 `immutable`。

开发：

```bash
make ui-dev        # cd web && npm run dev  （/api、/v1 代理到 127.0.0.1:8080）
```

## 10. Tailwind 4 迁移说明

v4 与 v3 **不兼容**，这次是有意升级并逐项处理，不是改个版本号：

| v3 | v4 | 这里的处理 |
|---|---|---|
| `tailwind.config.js` + `content` 数组 | CSS 优先，`@theme` / `@source` | 删除 JS 配置，主题与扫描范围写进 `src/index.css` |
| `@tailwind base/components/utilities` | `@import "tailwindcss"` | 一条 import 引入 preflight 与工具类 |
| `darkMode: "class"` | 默认跟随 `prefers-color-scheme` | `@custom-variant dark (&:where(.dark, .dark *))` 保留 class 切换 |
| PostCSS + autoprefixer | `@tailwindcss/vite`（Lightning CSS） | 删除 `postcss.config.js` 与两个依赖 |
| 自定义色写 `theme.extend.colors` | `@theme { --color-* }` | `@theme inline` + `hsl(var(--token))`，`:root/.dark` 覆盖仍生效 |
| `shadow-sm` / `outline-none` | 语义改名 `shadow-xs` / `outline-hidden` | `.card`、`.input` 与语言选择器同步改名 |
| `border` 默认色 gray-200 | 默认 `currentColor` | 基础层显式 `* { border-color: hsl(var(--border)) }`，保持 v3 观感 |
| preflight 给按钮 `cursor: pointer` | 移除，按钮回默认光标 | 基础层补回 `button:not(:disabled)` 的 pointer，禁用态仍由工具类覆盖 |

两个容易踩的点：

- **必须用 `@theme inline`**。不加 `inline`，工具类会把 `hsl(var(--background))` 求值成构建期常量，
  深色主题切换直接失效。
- **`source(none)` + 显式 `@source`**。仓库根是 Git 仓库，自动探测会一路扫到 Go 与文档；
  关掉后只扫 `web/index.html` 与 `web/src`，产物稳定、构建更快。

升级后首屏 CSS 从 15 kB 增至 24 kB（gzip 5.4 kB）：增量主要来自 v4 的 `@property` 声明与
color-mix 支持，换取的是不再需要 PostCSS 链路与 JS 配置。

## 11. 多语言（i18n）

界面文案不写死在组件里，全部走 `useI18n()`：

```tsx
const { t, locale, setLocale } = useI18n();
t("goals.title");                                  // 基准语言中文
t("goal.doneOf", { done: 3, total: 8 });           // 插值
t.raw(`status.${status}`, status);                 // 动态键，缺失回落到原值
```

- **基准是 `src/i18n/zh.ts`**：键集合由它定义，`en.ts` 用 `Record<MessageKey, string>` 约束，
  少一条或多一条都是编译错误，不会出现「英文页少一句」的静默降级。
- 语言状态在 `stores/locale.ts`（Zustand + localStorage）。首次访问跟随浏览器，
  之后记住选择；切换时同步更新 `<html lang>`，`formatTime` / `Intl.RelativeTimeFormat`
  直接读取它，日期与相对时间自动本地化。
- 后端错误码也本地化：`ApiError.code` → `error.<code>`，字典缺失时回落到服务端原文，
  所以新增错误码不会导致页面显示空白。
- 状态、角色等枚举用 `status.<value>` / `role.<value>` 动态键，新增状态只需补字典。
- 文案里的技术标识符用反引号包住（例如 `` `JANUS_MODELS` ``），由 `CodeText` 渲染成等宽样式，
  不需要为每种语言拆成多段。

加一门语言只要两步：复制 `zh.ts` 翻译成目标语言；在 `i18n/index.tsx` 的 `DICTIONARIES`
与 `LANGUAGES` 各加一项。页面代码不动。

## 12. 约定

- 界面文案只通过 `t()` 输出；代码、类型、日志、API 字段用英文。
- 新增文案必须同时补 `zh.ts` 与 `en.ts`，否则类型检查失败。
- 不在前端渲染未转义的用户数据；React 默认转义，禁用 `dangerouslySetInnerHTML`。
- 密钥永远不进入前端：配置页只显示 masked 值与状态。
- 新增页面：`views/<domain>/<Page>.tsx` + 路由 + `api/<domain>.ts` 类型化请求，不直接写 `fetch`。
