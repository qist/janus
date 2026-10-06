# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)；日期为本地时区（Asia/Shanghai）。

---

## [v0.3.5] - 2026-10-06

### 新增
- **虚拟模型 `janus`**：映射到 `/ui` 里选定的默认模型。客户端/IDE 只填 `janus`，换模型在面板点一下即可，**不用改 env、不用改客户端**。
- **`GET/POST /v1/settings`**：运行时读写默认模型（`default_model`），持久化到 SQLite 的 `settings` 表。
- **`/ui`「模型」页**：
  - 每行「**设为 janus**」按钮；
  - 「**思考档位**」列改为**点选按钮组**（`默认 / low / high / max …`，点一下即设、当前高亮）；
  - 档位随默认模型一起下发，解决「自定义模型名无法传 `reasoning_effort`」的客户端。
- **`/ui` 标签页**：拆成 `用量 / 最近请求 / 模型 / 运行` 四页（选中项存 localStorage）。

### 变更
- `default` / `auto` / 留空 仍指向上游默认（**旧语义不变**）；**`janus` 才是面板里选的默认**（避免与既有 `default` 撞名）。
- 模型/档位切换在**下一条请求**原地生效（`POST /api/session/{id}/model`），**不重开会话、上下文保留**。

### 文档
- 新增本 `CHANGELOG.md`。
- `README` 补「虚拟模型与默认模型选择」；`docs/DESIGN.md` 补运行时默认模型/`janus`/切换语义。

---

## [v0.3.4] - 2026-10-06

### 新增
- web 可选默认模型（运行时可改、存 DB）。
- 虚拟模型改用产品名 `janus`。
- `/ui` 面板拆成标签页。

### 文档
- 同步 `docs/DESIGN.md` / `README`（orchestrator 自动注入、`OPENCODE_REUSE_EXTERNAL`、`/v1/requests` 缓存等）。

---

## [v0.3.3] - 2026-10-05

### 新增
- **自动生成并内联注入 OpenCode agent 配置**：janus 拉起 OpenCode 时通过 `OPENCODE_CONFIG_CONTENT` 注入 `orchestrator` **白名单**（`*` deny + `execute` allow + `ob-*` allow），**不再手写** `~/.config/opencode/opencode.jsonc`。
- **`OPENCODE_REUSE_EXTERNAL`**：设为 `false` 时 janus 总是自己拉起上游（注入的前提）。
- `docs/ARCHITECTURE.md`：控制平面 + Agent 生态的架构演进与路线。

### 修复
- 会话重置时**立即删除旧 MCP server**，消除 agent 收到 `bridge: this tool server is stale (session was reset)` 的报错。
- `/v1/requests` 的 console 拉取加 **5s 短缓存**；客户端取消（`context.Canceled`）降为 Debug，不再刷 WARN。
- 模型列表预热改为**重试**，不再在刚拉起 OpenCode 时刷 `model list warmup failed`。

---

## [v0.3.2] - 2026-10-05

### 新增
- 工具**结果**写入正文注释（`<opencode-tool>`），工具型任务期间客户端能看到进展；去掉空名噪声。

### 修复
- 客户端**手动终止**任务后，新任务**重开干净会话**，不再续接到已中止的残缺会话（否则会看到旧任务上下文/中断残影）。

### 文档
- 说明「工具执行模式（三选一）」。

---

## [v0.3.1] - 2026-10-05

### 修复
- `/healthz` 报**实时上游**，而不是启动时的旧地址。

---

## [v0.3.0] - 2026-10-05

### 新增
- **自动托管 OpenCode**：启动时发现已跑的 `opencode serve`，没有且已安装就随机端口拉起；未装则提示安装（`OPENCODE_URL=auto`）。
- `janus models` CLI 也走自动发现/拉起。
- `/v1/usage` 增加 **OpenCode Go 套餐限额**（5 小时滚动 / 周 / 月）。
- **`/v1/requests` + `/ui`「最近请求」**：逐条请求的缓存命中率、思考 token；支持搜索、排序、分组汇总（逐条 / 按模型 / 按会话）。
- 补 **DeepSeek 风格缓存明细字段**（`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`）。

### 修复
- 流式始终带 `usage`，并按 OpenAI 语义修正 token 统计。
- 减少会话重建与 MCP 404（容忍历史重排 + 未知 token 可恢复）。
- 重开会话必须先于工具桥注册，否则新会话丢工具。
- janitor 删上游会话时同步清 DB `session_id` + 会话自愈。
- SSE 保活改用空 delta 数据块，避免客户端误判「超过 30 秒未返回数据」。
- systemd 改以 root 运行，修好数据目录与 `/v1/usage`。

---

## [v0.2.0] - 2026-10-04

### 新增
- **Anthropic Messages API（`/v1/messages`）**：Claude Code 可直接使用；档位（`claude-opus/sonnet/haiku`）自动映射，`/anthropic` 前缀，`x-claude-code-session-id` 会话锚点。
- **Responses API**：`previous_response_id` 续链、流式事件状态机、服务端 `web_search` 工具。
- **持久化**：Responses 与会话映射落 SQLite（`qist/sqlite`），Chat 历史快照落库，**跨进程重启可续链/续接**。
- 附件按模型能力接收（含 Responses `input_image`）。
- `parallel_tool_calls=false` 串行返回工具调用。
- `/ui`「支持的模型」一键复制模型名。

### 修复
- 高并发下的空回复；上游免费额度限制说明。

---

## [v0.1.0] - 2026-10-03

### 初始版本
- 把只有 `/api/*` 的 **OpenCode server** 包装成 **OpenAI 兼容 API**（`/v1/models`、`/v1/chat/completions` 流式+非流式）。
- 会话分桶与历史重放、EventBus 单连接 fan-out、四级终态对账、上游端点自动发现与热切换。
