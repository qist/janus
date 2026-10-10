# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)；日期为本地时区（Asia/Shanghai）。

---

## [v0.3.30] - 2026-10-10

### 新增
- **远程 mode B 出向路径一致性改写**：客户端 IDE 不再收到远端主机的目录地址。会话落在
  `BRIDGE_WORKSPACES_DIR/<scope>` 中性工作区（远程 + mode B）时，agent 产出里的 janus 主机路径
  会自动改写回客户端声明的项目路径（`X-OpenCode-Directory` > 消息抽取的项目根），覆盖四处出向通道：
  - **回答正文 / 工具注释 / 思考流**（`pathRewriter`）：跨 delta 被切断的路径前缀先扣住、拼全再放行，
    终态 flush 补发；对账基准 `serverText` 保持上游原文不受扰动；同机部署无规则、零开销；
    scope 共享会话多设备共用时按当前请求者重算。
  - **客户端工具调用参数**（`toolbridge.rewriteArgs`）：MCP `tools/call` 挂起前整体替换，
    客户端路径以 JSON 转义形式写入（Windows 反斜杠双写），保证 args 始终是合法 JSON；
    服务端自执行工具（web_search）不经过此路径。
  - **会话首轮 prompt 注入路径映射说明**：告知 agent「工具在用户设备执行，你的工作目录 X 对应
    用户项目目录 Y，参数一律用 Y 下的路径」，从源头避免它把工作区路径传进工具参数；
    只注入一次随会话留存，不逐轮重复。
  - 不同 scope 的路径不误伤；Chat / Responses / Anthropic 三套接口行为一致。**无需任何新配置**。

---

## [v0.3.29] - 2026-10-09

### 修复
- **工具调用超时后，下一轮重放保护不生效**：客户端在 `BRIDGE_TOOL_CALL_WAIT` 内没回工具结果时，
  桥会释放并中断 agent，但会话没有 terminated 标记 —— 客户端自动重发同内容时会把被中断的旧回合
  再跑一遍。现在工具超时即标记会话 terminated（`pendingCall.markStale` + `conv.markTerminated`）：
  同内容复读直接回 canceled（499，不白跑），携带新内容才重建干净会话。

---

## [v0.3.28] - 2026-10-09

### 修复
- **scope 模式新话题被 append 进旧共享会话**：共享会话（IDE+项目）遇到历史不匹配（换新话题 /
  起子代理）时若容忍或增量续跑，新旧内容会合拼回答，并发请求还会互相打断报 502。现在一律
  重置共享会话、重开干净会话全量重发，保证话题隔离。

---

## [v0.3.27] - 2026-10-09

### 修复
- **DB 恢复的会话首用不再 502**：从持久化库恢复的上游 sessionID（跨进程重启续链）在上游
  OpenCode 重启/换实例后已失效，此前首个请求 502、等下一轮才自愈。现在恢复的会话首次使用前
  先探活，失效就直接重建新会话，无缝续链。

---

## [v0.3.26] - 2026-10-09

### 新增
- **scope 模式支持中途插入新话题/子代理**：scope 只作分桶依据，桶内按历史前缀匹配（与普通
  模式一致）。中途插入的新话题/子代理落到独立会话立即回答，不再阻塞在在飞会话的会话锁上；
  跨设备同话题仍共享同一会话（前缀命中复用）。

---

## [v0.3.25] - 2026-10-09

### 修复
- **终止标记跨重试保留**：Trae 等 IDE 断线/点停止后会原样重发上一轮 payload。同内容复读不再
  消费 terminated 标记，直接回 canceled（499）等真实新内容；真正携带新内容的请求才消费标记、
  重置会话、全量重发。Chat / Anthropic / Responses 三条路径行为一致。

---

## [v0.3.24] - 2026-10-09

### 修复
- **会话终止后换新话题不再续答旧内容**：断连时清空缓存响应（被终止的回合绝不能作为缓存回放，
  否则表现为"终止后继续回答前面的内容"）；scope 模式下历史不匹配时仅在「最后一条 user 为新增
  内容」时追加续跑，否则重置会话全量重发，保证新话题必进 prompt。

---

## [v0.3.23] - 2026-10-08

### 修复
- **Responses 接口未按远程模式处理会话目录**：`/v1/responses` 此前对工作目录只取
  `X-OpenCode-Directory`，没有则直接回落部署目录（`BRIDGE_DIRECTORY`），远程 + mode B
  时工具桥 / 上游会话被建在 janus 部署目录而非客户端项目。现在与 Chat / Anthropic
  Messages 一致改用 `scopeOf + sessionDir`：客户端路径在 janus 主机上不存在时改用
  `BRIDGE_WORKSPACES_DIR/<scope>` 中性工作目录；`previous_response_id` 续链还会**锁定
  原链目录**（增量 input 不含项目路径，重算会漂移），dir 随响应落 SQLite 支持跨重启续链。
- **`sessionDir` 对客户端目录头不做存在性校验**：三套接口（Chat / Messages / Responses）
  共用逻辑此前对 `X-OpenCode-Directory` 是"非空即用"——远程客户端（如 Windows 上的
  Claude Code）传来的路径在 janus 主机上不存在时也会被直接使用，上游对不存在目录注册
  MCP 会报 500。现在目录选择链统一为「header（真实存在才用）> 消息抽取的项目根（同样
  校验）> per-scope 中性目录」，与文档承诺的远程 + mode B 行为一致。

---

## [v0.3.22] - 2026-10-08

### 修复
- **全部工具调用超时后，客户端整批回填旧结果会导致"数据回放"**：客户端（Trae）会把会话里
  积压的一整批 tool 结果（实测一次 18 条、含早已消费过的）在下一个请求里一起回填。此前
  "全部超时按新轮次处理"会把它们全部平铺进新 prompt，agent 把过期数据当新上下文用。
  现在三条路径（Chat / Anthropic / Responses）都会**先剥离过期 tool 消息与纯 tool_calls
  中间轮**（`stripStaleToolResults`）再发 prompt，只保留真实对话内容；客户端只回填结果、
  没带新指令时保留最后一条 user 供 agent 回应。
- **执行类工具 5 分钟超时太短，掐掉还在跑的长命令**：`sleep 75`（触发推送+等节点+查版本）
  这类发布流水线超过 5 分钟就被判超时、中断上游。`BRIDGE_TOOL_CALL_WAIT` 默认从 `5m`
  提到 `30m`（仍可用环境变量按部署调整）。

---

## [v0.3.21] - 2026-10-08

### 修复
- **配套状态查询工具名不再靠猜**（承接 v0.3.20 的 RunCommand 配套工具）：上线实测客户端（Trae）
  对小写 `check_command_status` 报"工具名不对"。现在：
  - 优先从 `RunCommand` 等触发工具**自带的描述**里识别配套工具的真实名字
    （`CheckCommandStatus` / `check_command_status` / `GetCommandStatus` / `get_command_status`…
    描述里写了哪个就用哪个，不靠猜）；
  - 描述没写才退回默认两个名字（`CheckCommandStatus` + `check_command_status`）都暴露；
  - 配套工具调用**失败时，桥把另一个可用的名字直接附进错误提示**给 agent
    （`bridge hint: 请改用 CheckCommandStatus…，command_id 不变`），agent 立即换名重试，
    不再在工具列表里反复"搜索正确的工具名"卡住。
  - **无需任何新配置**。

---

## [v0.3.20] - 2026-10-08

### 修复
- **`RunCommand` 发起 build 等长命令后 agent 卡住**：Trae 等客户端对长任务让 `RunCommand`
  立即返回 `command_id` 异步执行，但配套的状态查询工具（`check_command_status`）**只在客户端
  本地存在、不会写进 `tools[]` 声明**（"在工具列表外"）→ agent 既看不到也调不到，编译结束后
  不知道结果，只能干等/反复找工具名 → 卡住。
  现在客户端声明 `RunCommand`/`run_command`/`execute_command` 等「异步命令」工具时，桥自动补
  **配套工具** `CheckCommandStatus` 与 `check_command_status`（入参 `command_id` 取 RunCommand
  的返回值）暴露给 agent，走同一透传链路由**客户端本地**执行；客户端已声明同名工具则不重复。
  可用 `BRIDGE_TOOL_COMPANIONS=false` 关闭。

---

## [v0.3.19] - 2026-10-08

### 修复
- **`kill janus` 后它拉起的 opencode 变孤儿、越积越多**：旧实现用 `Setsid` 把
  `opencode serve` 放到独立会话（有意让「kill janus 不带走上游」），但 nohup/裸进程
  部署（无 systemd/容器 cgroup 兜底）下，每次重启都会留下一个没人管的孤儿，再启动
  又拉新的。
  现在改为「谁拉起谁负责」（默认开，`BRIDGE_UPSTREAM_CLEANUP`，`false` 还原旧行为）：
  - 拉起的实例打 `JANUS_MANAGED_UPSTREAM` 标记；
  - **优雅退出**（`kill <pid>` / Ctrl+C）时，先 SIGTERM、超时再 SIGKILL 收掉自己托管的
    upstream；
  - **`kill -9`/崩溃留下的孤儿**（标记仍在、父进程已死 / PPID=1），下次拉起前自动清扫；
  - `janus models` 自己拉起的实例用完即收，不再是孤儿制造机；
  - 外部实例（桌面端等，无标记）不受影响；`WatchUpstream` 换到托管实例时同步接管回收。

### 变更
- `Endpoint` 增加 `Spawned` / `Managed` 标识（本次进程拉起与否 / 是否带托管标记）。

---

## [v0.3.18] - 2026-10-08

### 修复
- **工具调用超时后，客户端晚到的结果会触发"幽灵续跑"**：`SearchReplace` 等编辑类工具走
  `BRIDGE_TOOL_CALL_WAIT_FAST`（默认 90s）短等待；客户端（IDE）在该时间内没回填结果时，
  桥按设计释放并中断 agent。但**会话上挂起的 pending 未清理** —— 客户端之后才把结果发回来，
  （例如用户审批 diff 超过 90s 后点重试 / IDE 慢一拍回传）会误走 resume：在**已中断的会话**上
  空转 → `idle_timeout` → 客户端**再收到一次"模型请求失败"**（4054）。
  现在：超时的调用被打上 stale 标记，Chat / Anthropic / Responses 三条 resume 路径都会先过滤
  （`livePending`）；全部超时时**按普通新轮次处理**，把工具结果平铺进提示词（`[Tool: name]`）
  让 agent 从结果里接着干，不再重复报错。
- **工具结果对齐改为三路径共用一套逻辑**：新增 `assignToolResults`（精确 id → 工具名 → 顺序），
  Chat / Anthropic / Responses 统一走它。**Responses 路径此前只按精确 id 回填**，客户端换成
  自己生成的 `function_call` id 时会 `answered=0/N` —— 现在与其它两条路径一致，按名/序兜底。
- 工具调用超时可观测：新增指标 **`opencode_bridge_tool_timeouts_total`**（`/metrics`），
  配合既有 `tool call timed out: client did not return a result within …` 日志即可量化。
- **Anthropic（Claude Code）档位模型解析不认面板选择**：`claude-*` 档位别名此前只认
  `BRIDGE_DEFAULT_MODEL`，**忽略 `/ui` 面板选的默认模型（存 DB，含思考档位）**，导致
  「选了其它模型走 Anthropic API 不生效、强制走了 Anthropic 模型」。现在按与虚拟模型
  `janus` 一致的优先级解析：**面板选择（存 DB）→ `BRIDGE_DEFAULT_MODEL` → 上次显式
  用过的模型 → 档位启发式 → 上游默认**。
- **Anthropic 工具结果回填 answered=0/N**：客户端回填 `tool_result` 时若用自己生成的
  `tool_use_id`（实测会换成 `toolu_…`），与 janus 下发的 `call_xxx` 对不上，agent 会拿到
  `bridge: client did not supply a result`。`resumeAnthropic` 补齐与 OpenAI 路径一致的
  「**① 精确 id → ② 工具名 → ③ 顺序**」三级对齐；工具结果回填后也落库。
- **Anthropic 路径会话重置后丢工具**：`handleMessages` 里工具桥注册在 `DiffReset`（会
  注销工具桥）**之前**，重置后新会话只剩内置工具。顺序调整为与 Chat 路径一致
  （**先处理 DiffReset，再注册工具桥**），并补 `persistConv`。

### 说明
- 若 IDE 侧审批/执行 `SearchReplace` 等工具经常超过默认的 90s，可调大
  `BRIDGE_TOOL_CALL_WAIT_FAST`（如 `300s`），或从
  `BRIDGE_TOOL_CALL_WAIT_FAST_TOOLS` 里去掉 `SearchReplace,Write,DeleteFile` 让它们走
  长等待（`BRIDGE_TOOL_CALL_WAIT`，默认 5 分钟）。

---

## [v0.3.17] - 2026-10-06

### 新增
- **孤儿 agent 快速收敛**：新增 **`BRIDGE_TOOL_ORPHAN_WAIT`**（默认 **`30s`**）。工具调用落到
  一个**没有在飞请求**的会话上（客户端已离开、agent 还在调工具）时，等这么久仍无人接手就判定
  为孤儿 → 拒绝并**中断该会话的上游 agent**，从源头结束（不再只是拒绝、让 agent 一直重试）。
  客户端正常在跑（有在飞请求）时不受影响。

### 变更
- idle 会话的守卫宽限从固定 `3s` 改为 `BRIDGE_TOOL_ORPHAN_WAIT`（默认 `30s`）。

---

## [v0.3.16] - 2026-10-06

### 修复
- **孤儿 agent 刷屏**：IDE 在两轮之间关闭时，janus 没有在飞请求可感知断连，上游 agent 会一直
  调工具 → 被 MCP 守卫拒绝 → 再重试，日志被刷爆（实测约 40 分钟 600+ 条）。
  - **工具调用超时（客户端没回结果）现在会中断该会话的上游 agent**，从源头掐掉孤儿。
  - **idle 拒绝日志限流**：同一会话最多每 60s 打一条（附被抑制条数），不再刷屏。

---

## [v0.3.15] - 2026-10-06

### 新增
- **流式空闲超时**：新增 **`BRIDGE_STREAM_IDLE_TIMEOUT`**（默认 **`120s`**）。流式请求超过这么久
  没有任何**真实数据**（正文/推理 delta、工具调用、上游 session 事件）就判定上游卡死 →
  中断上游并给客户端收尾（`idle_timeout`）。
  - **心跳（`BRIDGE_STREAM_HEARTBEAT`）不算数据**，否则永远不触发；一直有真实输出的长回答不受影响。
  - `0` / `never` / `off` 可关闭。

### 修复
- 流式路径（Chat / Anthropic / Responses）此前**没有服务端超时**：上游 agent 卡住时
  `BRIDGE_REQUEST_TIMEOUT` 不生效（只对非流式生效），客户端会一直挂到自己超时断开
  （实测挂过约 15 分钟）。现在由空闲超时兜住。

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
