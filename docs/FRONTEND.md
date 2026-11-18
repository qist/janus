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
| 样式 | Tailwind CSS 3 | 语义色用 CSS 变量（light/dark 一套 token） |
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
├── tailwind.config.js / postcss.config.js
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

还没做的两件事：

- **控制台页面**：`/ui/models` 仍只读 `JANUS_MODELS`，下一步接 provider / 凭据 / 路由的增删与启停；
- **OAuth 登录**：设备码/回调用服务端流程，浏览器只拿一次性 state，不接触 refresh token；
  表结构已留 `kind=oauth`，但授权流程未实现。

在页面接上之前，UI 明确标注配置来自环境变量，避免假装能改。

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

Vite 配置 `base: "/ui/"`、`outDir: "../internal/ui/dist"`；
`internal/ui` 用 `//go:embed all:dist` 打包，因此：

- `go build` / `go test` **不需要 Node**（dist 已提交）；
- 改了 `web/` 必须 `make ui` 并提交 dist，否则二进制里还是旧页面；
- `index.html` 用 `no-store`，hashed assets 用 `immutable`。

开发：

```bash
make ui-dev        # cd web && npm run dev  （/api、/v1 代理到 127.0.0.1:8080）
```

## 10. 多语言（i18n）

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

## 11. 约定

- 界面文案只通过 `t()` 输出；代码、类型、日志、API 字段用英文。
- 新增文案必须同时补 `zh.ts` 与 `en.ts`，否则类型检查失败。
- 不在前端渲染未转义的用户数据；React 默认转义，禁用 `dangerouslySetInnerHTML`。
- 密钥永远不进入前端：配置页只显示 masked 值与状态。
- 新增页面：`views/<domain>/<Page>.tsx` + 路由 + `api/<domain>.ts` 类型化请求，不直接写 `fetch`。
