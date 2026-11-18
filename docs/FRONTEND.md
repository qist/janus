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

### OAuth 登录

参考 sub2api 与 magpie 的做法，采用**授权码 + PKCE + 粘回授权码**，而不是在本机起回调监听：

- `openai` 走 Codex CLI 的客户端（`auth.openai.com`，回调固定 `localhost:1455`，
  janus 部署在远端时浏览器无法回连，所以必须粘贴）；
- `anthropic` 走 Claude Code 的客户端（`claude.com/cai/oauth/authorize`），授权后官方回调页
  直接显示 `code#state`，粘贴即可；
- 会话在内存里、10 分钟有效、一次性；state 不匹配直接拒绝；
- 令牌以 JSON 密文存储（access / refresh / expires_at），**距到期 2 分钟内自动刷新**并写回，
  刷新失败保留旧令牌，由上游 401 暴露问题；
- 控制台拿不到任何令牌，只能看到 `masked`。

还没做：设备码流程（magpie 对 Copilot 等用）、OAuth 凭据的「重新授权」入口、以及
厂商没有 `/models` 时的专用探测。

### 渠道目录

`GET /api/v1/channels` 返回静态渠道表（`internal/provider/catalog.go`），分四类：
`vendor`（厂商直连）、`subscription`（OAuth 订阅套餐）、`relay`（聚合/中转）、`local`（自建网关）。
每条包含 `kind / protocols / auth / base_url / alt_base_urls / note`，覆盖 OpenAI、Anthropic、
Google、DeepSeek、Kimi、GLM、MiniMax、Qwen、火山、混元、千帆、SiliconFlow、OpenRouter、
Vercel、OpenCode、xAI、Groq、Together、Fireworks、Cerebras、Ollama、LiteLLM 等约 50 个入口；
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
