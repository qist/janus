# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)；日期为本地时区（Asia/Shanghai）。

---

## [v0.3.14] - 2026-10-06

### 新增
- **共享会话独立保留时间**：新增 **`BRIDGE_SHARED_SESSION_TTL`**（默认 **`24h`**），只作用于
  scope（`s:`）共享会话；普通会话仍用 `BRIDGE_SESSION_TTL`（默认 `30m`）。
  跨设备共享希望保留更久——空闲超过才回收，一直用则不回收。
- **TTL 支持"永不回收"**：`BRIDGE_SESSION_TTL` / `BRIDGE_SHARED_SESSION_TTL` / `BRIDGE_CONV_TTL`
  均支持 `never` / `off` / `none` / `0` = 永不按时间回收（会话仍受 `BRIDGE_MAX_CONVERSATIONS` 的 LRU 上限约束）。

---

## [v0.3.13] - 2026-10-06

### 新增
- **统一的出站 User-Agent**：对上游 OpenCode / usage / 图片下载等所有出站请求，默认带
  `janus/<version> (<os>/<arch>; +https://github.com/qist/janus)`，替代 Go 默认的
  `Go-http-client/1.1`（不少网关 / CDN / 风控会把它当脚本流量直接拦掉，403/429）。
  - 新增 **`BRIDGE_USER_AGENT`** 可从外部覆盖（例如需要伪装成浏览器 UA 时）。
  - 通过一个 `RoundTripper` 统一注入：**未显式设置 UA 的请求自动补，已设置的保留**
    （附件下载故意用的浏览器式 UA 不受影响）。

---

## [v0.3.12] - 2026-10-06

### 变更
- **macOS 默认数据目录**改为 `~/Library/Application Support/janus`（DB 与 workspaces 都在其下），更符合 macOS 惯例；
  Linux 保持 XDG（`~/.local/share/janus`）不变，Windows 保持 `%LOCALAPPDATA%\janus`。
  `OPENCODE_DB`（上游 OpenCode 的库）仍按 OpenCode 自身的 XDG 约定定位，不套用 macOS 目录。

### 文档
- README 新增「在 Windows 上运行」：解压/装 OpenCode/配置 `janus.env`/两种启动方式（含配置查找顺序）、
  开机自启用「任务计划程序」或 `nssm`。

---

## [v0.3.11] - 2026-10-06

### 新增
- **跨平台发布**：新增 **macOS**（`darwin/amd64` + `darwin/arm64`）与 **Windows**（`windows/amd64` + `windows/arm64`）构建。
  - 新增 `make dist-darwin` / `make dist-windows` / `make dist`（全部平台）；CI 随 `v*` tag 自动出全平台附件。
  - 发布包除二进制外仍附带 `janus.env.example`（配置模板）与 `README.md`；Windows 用 `.zip`。

### 修复
- **Windows 无法编译**：`autostart.go` 里的 `syscall.SysProcAttr{Setsid: true}` 是 Unix-only，改为 build tag 拆分
  （`procattr_unix.go` / `procattr_windows.go`；Windows 用 `CREATE_NEW_PROCESS_GROUP` 达到同样的"脱离"效果）。

### 变更
- **跨平台默认路径**：Windows 默认数据目录改为 `%LOCALAPPDATA%\janus`（`BRIDGE_DB`/`BRIDGE_WORKSPACES_DIR`/`OPENCODE_DB`
  未配置时），Linux/macOS 保持原有 XDG 约定不变；显式配置一律优先。

---

## [v0.3.10] - 2026-10-06

### 新增
- **工具调用等待分两档**：`BRIDGE_TOOL_CALL_WAIT`（长等待，执行类工具，默认 `5m`）+ 新增
  `BRIDGE_TOOL_CALL_WAIT_FAST`（短等待，只读/编辑类工具，默认 `90s`）与
  `BRIDGE_TOOL_CALL_WAIT_FAST_TOOLS`（短等待工具名列表，默认
  `Grep,Read,Glob,LS,WebFetch,Write,SearchReplace,DeleteFile`）。
  - 只读/编辑类工具正常秒回，卡住基本是客户端卡死；短等待让 janus 快速判失败、把工具错误
    还给模型继续，而不是干等 5m 撞上 IDE 自身请求超时（表现为 `Connection timeout (HTTP Status: 500)`）。
  - **未列出的工具（含未知新工具）一律走长等待**，避免误杀 `RunCommand`/build 等长任务。
  - `BRIDGE_TOOL_CALL_WAIT_FAST <= 0` 或 `>= BRIDGE_TOOL_CALL_WAIT` 时不启用；列表设成
    `none`/`off`/`-` 表示禁用短等待。

---

## [v0.3.9] - 2026-10-06

### 变更
- **发布包附带配置模板**：`make dist-linux` 产出的 tar.gz 里，除二进制外现在也含 **`janus.env.example`**（配置模板）与 **`README.md`**。
- 配置模板 / README 补齐 scope 相关新增项（`BRIDGE_SCOPE_KEY` / `BRIDGE_PROJECT` / `BRIDGE_PROJECT_MAP` / `BRIDGE_WORKSPACES_DIR`）；**模板配置项与 `config.go` 全量对齐**。

---

## [v0.3.8] - 2026-10-06

### 新增
- **项目级隔离 + 跨设备共享（scope）**：没有客户端会话 id 时，按 `scope = IDE + 项目` 定位会话。
  - `BRIDGE_SCOPE_KEY=true` 开启：**同 IDE + 同项目 → 同一会话（跨设备共享）**；不同项目 / 不同 IDE → 隔离。
  - 项目识别取自各客户端 firstUser 里的**权威字段**：
    | 客户端 | 来源 |
    |---|---|
    | Trae | `Primary working directory: <path>` |
    | CodeBuddy | `Workspace Folder: <path>` |
    | GitHub Copilot Chat | `following folders: - <path>` |
    | 兜底 | 消息里最频繁的路径前缀（已排除 `/usr`、`/etc`、`/root/.trae*`、`/root/.vscode*`、`/tmp` 等系统目录） |
  - `BRIDGE_PROJECT`（显式项目名）/ `BRIDGE_PROJECT_MAP`（设备路径→项目名）可覆盖。
  - IDE 从 `User-Agent` 归一（`trae` / `codebuddy` / `githubcopilotchat`，**剥掉版本号**，避免升级产生新 scope）。
- **远程 per-scope 中性工作目录**：客户端项目路径在 janus 主机上**不存在**时（远程 + mode B），会话目录用 `BRIDGE_WORKSPACES_DIR/<scope>`（janus 创建的空目录），而不是回落到 `BRIDGE_DIRECTORY` —— 既避免 agent 误认成那个项目，也避免上游对不存在目录注册 MCP 报 500。
- **客户端诊断日志**：`client:` 行打印 `ip= / xff= / ua= / dir= / scope= / key= / firstUser=` 等（按签名去重）；`client headers:` 打印全部请求头；Trae 的 `Acl-Token` JWT payload 也解码。

### 变更
- `BRIDGE_ADDR` 可用 `0.0.0.0:2810` 让其它设备访问（内网自用；公网请加 TLS 反代 + 强 `BRIDGE_API_KEY`）。

---

## [v0.3.7] - 2026-10-06

### 修复
- **工具桥「下一轮目录为空」**：OpenCode 的 agent 会话**不会**在 MCP server 重新注册后刷新工具目录，而 janus 之前**每轮结束都注销**（`releaseToolsIfIdle`），导致下一轮 agent 看到「Code Mode 目录为空」、一个工具都调不了（时好时坏：上一轮刚注册过就还好，隔久了就空）。改为**在会话存活期内保持注册**；注销只留给「会话重置 / 客户端不再声明 tools / janitor(TTL)」。

---

## [v0.3.6] - 2026-10-06

### 修复
- **工具桥 id 对不上**：部分客户端（实测）用它**自己生成的** `tool_call_id`，而 janus 下发的是 `call_`+hex，导致 `answered=0/N`、agent 收到 `bridge: client did not supply a result`。现在按「**① 精确 id → ② 工具名 → ③ 顺序**」三级对齐 `pending ↔ 客户端结果`；并且只匹配**当前这一轮**的结果，避免回放上一轮的旧结果。
- **`janus` 默认解析竞态**：启动瞬间模型列表可能为空/不完整，解析不到就落到上游**免费默认模型**（经 API 403 `free tier`）。改为**强制刷新一次模型列表再试**，避免误落免费档。
- **`/v1/requests` 超时卡顿**：console 的 `request-logs` 超时/连不上时，面板每轮刷新都卡满超时。加**失败负缓存（60s）**并缩短 handler 超时（20s→10s）。

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
