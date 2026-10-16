# 更新日志

本项目遵循[语义化版本](https://semver.org/lang/zh-CN/)；日期为本地时区（Asia/Shanghai）。

---

## [v0.3.52] - 2026-10-16

### 修复
**kill/重启类命令经 RunCommand blocking=true 执行时，客户端偶发楔死**（两次
楔死都是同一条 `kill -9 + nohup 重启` 命令：交付后 Trae 不执行也不回传，任务
卡死，详见 v0.3.51 的取证）。

**规避：RunCommand 类工具描述追加异步引导**——模型对 kill / systemctl /
nohup / 超过 ~30s 的命令一律改用 `blocking=false` + 轮询
`CheckCommandStatus`（异步模式实测稳定，昨天的构建就是这么走的）。

- 注入点在注册源头（ensureTools）：描述变化体现在工具集指纹里 → 指纹改名 →
  opencode 必然重新拉 tools/list（它对同名 server 的重新 PUT 不重列工具）。
  若只在 tools/list 暴露层注入，指纹不变，已有会话永远看不到新描述。
- 仅当配套状态查询工具可用时注入（`BRIDGE_TOOL_COMPANIONS`，避免引导模型
  调用不存在的工具）；幂等，其余工具不受影响。

### 验证
- 新增 `TestSteerAsyncCommandDesc`（触发工具命中/非触发工具/禁用配套/幂等
  四段断言）；`go vet` + `go test ./...` 全绿。

---

## [v0.3.51] - 2026-10-16

### 新增
**楔死取证（`BRIDGE_WEDGE_FORENSICS`，默认关）**：客户端偶发"收到 tool_call
后不执行也不回传"（10-15/16 五次同签名楔死：janus 交付完成、accesslog 显示
客户端零请求，IDE 卡在等待模型），纯服务端日志定位不到客户端解析卡死的触发
点。开启后：
- 每轮以 `tool_calls` 收尾的响应**原始 SSE 字节**落盘到
  `<数据目录>/forensics/<会话>/`（文件名 `<毫秒>-<首个 callID>.sse`，
  每会话保留 20 份，超出淘汰最旧）；
- 楔死发生时（工具结果超时/上游 abort）日志按 callID 指出对应的落盘文件，
  与成功轮逐字节对比即可定位触发字段/分块形态。

实现：`sseWriter` 增加字节捕获（writeChunk/done 原样留底），仅 chat/completions
路径（Trae 楔死均发生于此协议）；落盘器 nil 安全，未开启时零开销。

### 验证
- 新增 4 组测试（save/trace、prune 淘汰、nil no-op、sseWriter 捕获内容断言）；
  `go vet` + `go test ./...` 全绿。180 部署时同步开启。

---

## [v0.3.50] - 2026-10-15

### 修复
- **经 janus 调用 opencode.ai 免费档模型（`opencode/*-free`）恒定 `403
  FreeTierError`**（"OpenCode's free tier can only be used from within
  OpenCode"）。根因不是客户端身份，而是 janus 注入的 `orchestrator` agent 把内置
  工具全 deny 了：上游风控要求请求里带标准内置工具，工具集为空即判定"非 OpenCode
  客户端"。

  实测（同一凭据、同一台机器）：`deny *` 下只 allow 内置 `read` 即放行；
  `execute`/`ob-*`/`bash`/`edit`/`write`/`glob`/`grep`/`task`/`webfetch` 都不行；
  把 `OPENCODE_CLIENT`/User-Agent 改成 `desktop` 同样 403。`build` 等内置 agent
  一直可用。

### 变更
- `orchestrator` 的 permissions 由「禁全部内置工具」放宽为「只留只读 `read`」，
  写/执行仍必须走客户端 `ob-*` 工具桥。要恢复强隔离可删掉这条 `read` —— 代价是
  免费档经 janus 全量 403（订阅档 `opencode-go/*` 不受影响）。
- 新增回归测试 `TestOrchestratorAllowsReadForFreeTier`，并在 `TestOpenCodeInlineConfig`
  中断言 `read` 存在。README「工具执行模式」方式 B/C 与 `docs/DESIGN.md` §5.7.2 同步。

---

## [v0.3.49] - 2026-10-15

### 变更
- **`janus.env.example` 默认启用 `OPENCODE_CLIENT=desktop`**：janus 是「代客户端
  发起调用」的桥，模板里默认按桌面端来源上报（上游请求头 `x-opencode-client:
  desktop`、`User-Agent: opencode/latest/<ver>/desktop`）。想恢复 OpenCode
  自己的默认（`cli`）就删掉这行或置空。

  代码默认值不变（空 = `cli`）：模板的职责是给推荐值（同理 `BRIDGE_ADDR` 在
  模板里是 `127.0.0.1:2810`，而代码默认是 `0.0.0.0:2810`）。README 配置表同步
  标注模板默认。

---

## [v0.3.48] - 2026-10-15

### 新增
- **`OPENCODE_CLIENT`**：指定 janus 拉起的 OpenCode 对外声明的客户端身份，
  显式转发给子进程。OpenCode 用它填上游请求头 `x-opencode-client` 与
  `User-Agent` 尾段（`opencode/latest/<ver>/<client>`）。空=交给 OpenCode
  自己的默认（`cli`）。

  为什么必须显式转发：`janus.env` 是当**配置文件**读的（`JANUS_CONFIG`），
  内容只在 janus 进程内解析成 Config，**不会**进 `os.Environ()`。而 `SpawnOpenCode`
  用 `os.Environ()` 组装子进程环境，所以只把变量写进 `janus.env` 不起作用——
  子进程只拿到 systemd 给的那几个变量。现在由 `opencodeEnv()` 显式追加。

  环境组装同时抽成 `opencodeEnv()` 并去重：`OPENCODE_SERVER_PASSWORD`、
  `JANUS_MANAGED_UPSTREAM`、`OPENCODE_CONFIG_CONTENT`、`OPENCODE_CLIENT`
  这几个由本函数权威决定的键，先剔除继承来的同名项再追加，避免同名两份、
  子进程取值不定。

### 测试
- `autostart_test.go`：锁定 `OPENCODE_CLIENT` 的转发、空值不设置、
  恰好多一份、以及原有密码/托管标记不被弄丢。

### 验证
- `go test ./...` 全绿（新增 2 项回归测试通过）。
- 实测 `OPENCODE_CLIENT=desktop` 后，托管 opencode 进程的 `/proc/<pid>/environ`
  含 `OPENCODE_CLIENT=desktop`；本机 opencode 为 v2.0.22，上游将看到
  `opencode/latest/2.0.22/desktop` + `x-opencode-client: desktop`。
- 流式请求 HTTP 200、字节 7631，会话创建正常。

---

## [v0.3.47] - 2026-10-15

### 修复
**用量面板在 console 抖动期间整块报错且反复卡超时**。`Report()` 此前没有失败
负缓存：console 侧超时/限速的窗口期里（实测诱因：代理链路在客户端重负载时
抖动），UI 每 60s 自动刷新都重新撞满 15s 超时、面板一直显示"用量暂不可用"。
现与"最近请求"标签同策略：失败负缓存 60s 窗口内不重试，有旧数据就返回旧数据
（宁旧不卡，UI 靠 fetched_at 辨别新旧），没有旧数据才报错；窗口过后自动恢复
重试，成功即清错误状态。

### 验证
- 新增 `TestUsageReportNegativeCache`（失败给旧数据、窗口内不重试、窗口后
  恢复、从未成功时才报错四段断言）；`go vet` + `go test ./...` 全绿。

---

## [v0.3.46] - 2026-10-15

### 修复
**长工具调用（编译/测试/发布类命令，客户端执行 >6 分钟）必被掐死**。根因在
opencode 的运行时 Bun：其 `fetch` 对 remote 请求有 **~360 秒总时长硬顶**——
无视调用方 AbortSignal（30m signal 照样 360.3s 死）、无视数据流动（20s 心跳
保活照样死，均实测复现），且无环境变量可调。janus 下发的
`timeout.execution=30m` 只到达 SDK 的 setTimeout 层，永远轮不到它生效。
症状：模型发起编译 → 客户端执行数分钟 → opencode 360s 掐断挂起调用 →
agent 视为超时开始重试 → 全部撞孤儿守卫 → 客户端最终带着一堆已过期结果
回来 → 被判新话题重开会话，任务反复断开（实测 75 次 TimeoutError）。

**修复：工具桥默认改为 local(stdio) 注册**（`BRIDGE_MCP_TRANSPORT=local`，
可设 `remote` 回退旧行为）：
- janus 内嵌桥脚本 `mcpbridge.js`（stdio ↔ janus HTTP 代理），注册为
  local(stdio) MCP server，由 opencode 以内嵌 Bun 运行时拉起；
- 桥脚本自己的 fetch 显式传 Bun 非标准 `timeout` 选项（实测可覆盖 360s 硬顶，
  ToolCallWait 30m + 30s 余量），单次调用存活期不再受 6 分钟制约；
- 脚本落点：workspace 会话写进 scope 目录（workspace 驱动的子进程只见沙箱内
  路径，实测 /tmp 不可见）；同机项目会话写中央数据目录，不弄脏用户项目；
- 注册失败（老版本 opencode 不认 local 类型等）自动退回 remote，工具链路
  始终可用；codemode=false 与工具集指纹改名机制对 local 同样生效。

### 验证
- Bun 1.4.2 三组实证：默认 360.3s 掐断、keepalive 无效（总时长性质）、
  `timeout` 选项覆盖后 600s 请求存活。
- local 注册实测：opencode `mcp connected tools=1`，codemode=false 生效。
- `go vet` + `go test ./...` 全绿；新增脚本落点决策、幂等写入、注册配置
  断言三组测试；>360s 真实工具往返验证见部署冒烟。

---

## [v0.3.45] - 2026-10-14

### 修复
**会话重置后工具桥被"旧纪元残留"毒死（agent 判定工具通道断了并放弃任务）**。
实测事故链：Trae 中止生成 → 重试 499 → 新内容重开会话 → agent 的全部工具
调用连续失败 ~9 分钟后放弃并输出"请重连"（客户端还收不到这段文本，表现成
无限"思考中"）。三个叠加缺陷：

- **stale-token 调用按工具名重路由**：会话重置/工具集换指纹后，opencode
  配置里残留旧 server（URL token 已失效），而全量重放型客户端（Trae）的
  历史里带着旧纪元工具名，agent 依样调用 → 此前一律报
  `stale (session was reset)`。现在 unknown token 的 tools/call 先按工具名
  内嵌的会话命名空间（`ob_<key>_<tool>`）反查活会话，能对上就正常挂起执行，
  兜底才报 stale。
- **janitor 多目录对账**：MCP 注册按会话各自工作区目录写入（分桶会话在
  `WorkspacesDir/<scope>` 下），此前只扫默认目录，残留永不清理。现在扫
  默认目录 + 活跃会话目录 + `WorkspacesDir` 直接子目录（覆盖已被 GC 的
  会话与 janus 重启后的残留）。
- **孤儿守卫拒绝消息劝停 + abort 补 markStale**：旧拒绝文案结尾
  "call a tool exposed for your current conversation" 被模型当成重试暗示，
  实测连续换着花样调了 5 轮、每轮白等 30s。现按状态分流：已有挂起调用时
  明确说"已在等客户端回填"，没有在飞请求时说明原因，都要求停止调工具并
  收尾。另外上游主动掐断 tools/call 连接（opencode 侧超时/重启，实测挂起
  5m56s 后 abort）此前不 markStale，挂起调用残留 live——现与超时路径一致。

### 验证
- `go vet` + `go test ./...` 全绿；新增 stale-token 重路由、stale 兜底、
  拒绝消息分流、`Store.Directories` 去重四组测试。

---

## [v0.3.44] - 2026-10-13

### 变更
**`BRIDGE_TOOL_ANNOTATIONS` 默认 true → false（透传原则）**：该开关把
opencode 内置工具的活动以 `<opencode-tool> name: ...` 注释拼进客户端收到
的正文流——这是桥自己生成的内容混入模型输出，违反"能透传的就透传"的底线
（MCP 客户端工具早已排除，剩余注入对 Trae 这类严格客户端全是正文污染）。
现默认关闭，正文只含模型自己的输出；纯聊天客户端需要进展可视化时显式开启。
180 部署环境同步关闭。

### 验证
- `go vet` + `go test ./...` 全绿（注释机制与剥除逻辑保持原样，仅默认值翻转）。

---

## [v0.3.43] - 2026-10-13

### 修复
**MCP 工具调用 30 秒被掐断（长命令必挂）**：opencode v2 默认
`codemode=true`——桥注册的 MCP 工具经 Code Mode 包装执行，包装路径不遵守
`timeout.execution`（v2 文档称默认 12h），实测工具调用 30 秒即被传输层掐断。
此前所有工具往返都 <15s 所以从未暴露；客户端跑 90s 的 `go build` 时
opencode 连续 4 次 `mcp transport error`（每次 30.001s），回合永久卡死
（IDE 表现为"正在规划下一步"不动）。现注册 MCP 时显式 `codemode:false`
恢复工具直透（与 v2 之前行为一致）。

### 验证
- 180 实测闭环：慢工具（45s 后才回填结果）的 MCP 调用 `dur=45.109s` 完整
  存活，回合正常完成并引用工具结果——修复前 30s 必死。
- `go vet` + `go test ./...` 全绿。

---

## [v0.3.42] - 2026-10-13

### 修复
`/v1/responses` 工具调用两处收尾缺陷（Trae `custom_responses_compatible`
实测定位）：

- **工具轮 `response.completed` 缺 `usage`（"文本通、工具不通"的根因）**：
  官方流 `completed` 事件恒带 `usage`（工具调用轮也有，至少 input tokens）。
  此前工具调用轮上游尚未产出带 token 的消息、统计全零时 `usageToResponses`
  返回 nil → `usage` 整个缺失 → 严格解析器判整个事件非法 → 静默丢弃 →
  客户端永远不执行 function_call、不回传结果。文本轮带 usage 所以一直正常，
  症状表现为"客户端收到 function_call 后 90s 无任何后续请求"→ 桥侧工具等待
  超时 → 会话标记 terminated → 客户端重试相同内容 → 499 防回放护栏。
  现新增 `usageOrZero`：`completed` / `failed` / 非流式响应恒带 usage
  （未知时补零值对象）。
- **中止重置说明**：会话因上一轮被用户手动中止而重建时，历史重放型客户端
  仍会把旧任务的完整上下文（含半途的工具调用记录）全量重发，模型把中断的
  任务当未完成工作继续执行（实测：中断后发新问题，回答仍回到旧任务）。
  现于 prompt 末尾注入 `[中断重置]` 放弃指令（末位指令权重最高），
  chat / responses 两路径一致。

### 验证
- `go vet` + `go test ./...` 全绿；新增回归
  `TestResponsesSSECompletedCarriesUsage`（completed 事件必带 usage）、
  `TestResponsesInterruptedResetNote`（中止重置注入 + 新问题正常发送）。
- 180 上线后实测：Trae 经 `/v1/responses` 的工具往返全面恢复
  （RunCommand / Read / Glob / Grep / SearchReplace / AskUserQuestion 连续
  投递成功），重启后零 499 / 零超时告警。

---

## [v0.3.41] - 2026-10-13

### 修复
`/v1/responses` 工具调用链路两处缺陷（实测复现：历史重放型客户端 MCP 工具
调不通，回填请求挂死）：

- **无锚点工具结果回填被整体跳过（核心缺陷）**：回填分支此前要求
  `previous_response_id` 存在（`prev != nil`）。历史重放型客户端（实测
  CodeBuddy / hertz UA）不发任何锚点，回填请求靠历史归属路由到同一会话，
  却被挡在回填之外——`function_call_output` 被 Flatten 成新 prompt 发给还
  停在 MCP `tools/call` 上的上游会话：请求挂死（实测 150s 无响应），挂起
  调用直到 90s/30m 超时才中断，工具调用永远完不成。现对齐 chat 路径：
  回填分支移到历史差分之前、条件只看「会话上有挂起调用且 input 携带工具
  结果」，锚点与历史重放两种客户端都能回填。
- **回填轮不快照历史**：投递成功后未 `setLast` / `persistConv`，下一轮
  全量重放的差分把 function_call / tool result 当增量重发（数据回放）。
  现与 chat 的回填路径一致，投递前快照。
- **v0.3.40 的 instructions 注入方式回退重构**：注入进消息序列会让
  instructions 参与 Diff / 前缀匹配——instructions 每次请求变化的客户端会
  前缀失配 → 每请求新建会话（实测：MCP 重注册风暴、上下文反复丢失、挂起
  的工具调用随旧会话蒸发）。现统一在 planText 层按需注入（首轮 / 变更 /
  会话重建），不进消息序列；锚点链的「同值续轮不重复注入」语义保留。

### 验证
- `go vet` + `go test ./...` 全绿；新增
  `TestResponsesToolResultNoAnchorContinuation`（无锚点回填端到端：投递
  到挂起调用、不新建 session）。
- 180 实测复现闭环：修复前无锚点回填请求挂死 150s；修复后同一请求正常
  返回（`responses tool results delivered: answered=1/1`），模型基于工具
  结果给出最终回答。带 `previous_response_id` 的锚点回填回归通过。

---

## [v0.3.40] - 2026-10-13

### 修复
`/v1/responses` 接口标准化第二轮审查，补齐四处规范/健壮性缺口：

- **`instructions` 只回显、从不注入 prompt（功能性缺陷）**：chat 的系统提示在
  `messages[0]`，Responses 客户端（实测 Trae / Codex）放在 `instructions` 字段；
  此前该字段只在响应对象里回显，模型完全看不到客户端系统提示。现按客户端锚定
  方式注入：
  - 无锚点回退链：恒注入到消息序列，交给 Diff 去重（与 chat 的 system 消息
    同语义：不变→只发增量，变更→DiffReset 重开会话）；
  - 锚点链（`previous_response_id` / `X-Session-ID`）：input 只是增量，在
    planText 层按需注入（首轮 / instructions 变更 / 会话重建），同值续轮不重复
    灌入；注入发生在消息层之前，不影响 `terminatedReplay` 的差分比较。
  - 会话状态新增 instructions 快照（`Conversation.snapshotInstructions`）。
- **`function_call_output.output` 非字符串形态导致整包 input 解析失败**：规范定义
  为 string，但有客户端发 `[{type:"output_text",...}]` 或 `{"text":...}`；此前按
  string 硬解析，任一项形状不符整个 input 数组反序列化失败 → 400。现逐项归一化
  （`responsesItemOutputText`：string / 数组 / 对象 / 兜底原样序列化）。
- **`input` 单对象形态 400**：官方为 string 或数组，手拼客户端可能发单个 item
  对象，现兜底解析。
- **`web_search` / `web_search_preview` 同时声明时重复注册**：现去重为单个
  `web_search` 工具。

### 验证
- `go vet` + `go test ./...` 全绿；新增 5 项回归：
  `TestResponsesInputStructuredToolOutput` / `TestResponsesInputSingleObject` /
  `TestResponsesWebSearchDedup` / `TestPrependSystemMsg` /
  `TestResponsesInstructionsAnchoredChain`（锚点链首轮注入、同值续轮去重、
  变更重注入三条断言）。

---

## [v0.3.39] - 2026-10-12

### 修复
按 OpenAI 官方 OpenAPI 规范（openai-openapi v2.3.0）逐字段核对 `/v1/responses`
的 SSE 事件与 Response 对象，补齐严格解析器（实测 Trae 的
`custom_responses_compatible`）判为"llm response empty stream"的几处不合规：

- **SSE 事件缺规范必填字段 `item_id`**：`response.content_part.added` /
  `content_part.done` / `output_text.delta` / `output_text.done` /
  `reasoning_summary_part.added` / `reasoning_summary_part.done` /
  `reasoning_summary_text.delta` / `reasoning_summary_text.done` 此前都只发
  `output_index`，没有 `item_id`。规范里这些事件的 `required` 含 `item_id`，
  客户端据此把增量归到具体输出项；缺失即无法归属，严格解析器整条流判空。
  现全部补上对应的 `item_id`。
- **`response.output_text.delta` / `output_text.done` 缺 `logprobs`**：规范
  `required` 含 `logprobs`（官方流为空数组），现发 `"logprobs":[]`。
- **`created` 后未发 `response.in_progress`**：OpenAI 官方流在 `response.created`
  之后紧跟 `response.in_progress`，部分严格解析器据此判断"流已开始"。现按规范补发。
- **可选字段输出 `null`**：`previous_response_id` / `instructions` / `error`
  没有 `omitempty`，为 nil 时输出 `"xxx":null`。OpenAI 官方不输出这些空字段，
  严格解析器按官方格式校验会判为响应异常。现加 `omitempty`。

### 新增
- **`responses_spec_test.go` 回归套件**，把规范要求锁进 CI，防止再次回归：
  - `TestResponsesSSEEventFieldsMatchSpec`：逐事件核对 `required` 字段
    （`item_id` / `logprobs` / `sequence_number` / `type` 等），流式推理项
    与纯文本两条路径都覆盖。
  - `TestResponsesSSENoNullOrphanFields`：断言事件里不出现 `"output":null`、
    `"tools":null`、`"previous_response_id":null`、`"instructions":null`、
    `"error":null`、`"usage":null`。
  - `TestResponsesSSESequenceMonotonic`：断言 `sequence_number` 自 0 起严格递增。
- `responses_null_test.go` 增补：Response 对象可选字段为空时必须省略而非 `null`。

### 移除
- 临时排障脚手架（`debug_sse.go`）及其开关 `BRIDGE_DEBUG_SSE_DIR`
  （请求/SSE 落盘）与 `BRIDGE_DEBUG_NO_REASONING`（屏蔽 reasoning 项）。
  根因已定位并修复，脚手架不再需要；两者从未写入 `janus.env.example` / README。

### 验证
- `go test ./...` 全绿；新增规范回归套件 3 项均通过。
- 用 OpenAI 官方 OpenAPI 规范脚本校验真实抓取的 SSE 流：13 个事件、
  0 个缺必填字段；`janus` 与 `opencode-go/deepseek-v4.1-flash` 两条模型路径
  事件序列一致（仅 delta 数量不同）。
- **Trae 连通性测试通过**（"测试并添加"成功返回）。

---

## [v0.3.38] - 2026-10-12

### 新增
- **HTTP 访问日志**（`accesslog.go`）：记录每个进入 mux 的请求
  `method path status dur bytes ip ua`，跳过 `/healthz`、`/metrics`、`/ui` 与静态资源。
  排查"客户端报 llm response empty stream 但服务端看着正常"这类问题时，没有访问日志
  就看不到客户端到底发了什么、拿到什么状态码和字节（此前只能靠临时 debug 日志）。
  流式端点的 `dur` 是"直到流结束"的总耗时，正好用来识别客户端提前断流。

### 修复
- **`/v1/responses` 回显的 `model` 是上游模型名**（如 `opencode-go/deepseek-v4.1-flash`），
  而客户端请求的是别名（如 `janus`）。严格校验回显一致性的客户端（实测 Trae 的
  custom_responses_compatible）会因此判为响应异常。现按已有的 `BRIDGE_MODEL_ECHO=request`
  回显请求里的原始模型名；默认值 `real` 行为不变。

### 验证
- 带 tools + system + `hertz` UA 的 Trae 同构请求：`response.created` 无 `output`/`tools`
  为 `null` 的字段，事件序列完整到 `response.completed`；`model` 回显 `janus`。
- 访问日志实测捕获 Trae 的三次 POST：全部 `status=200`，字节 17814/16844/6723。

---

## [v0.3.37] - 2026-10-12

### 修复
- **Responses 对象的 `output` / `tools` 序列化为 `null`**：`newResponsesResponse` 的骨架
  和流式结束时的 `response.completed` 用的是 nil 切片，JSON 输出 `"output":null` /
  `"tools":null`，不符合 OpenAI Responses 规范（这两个字段是数组）。严格解析的客户端
  （Trae 的 custom_responses_compatible 等）会因此判定流异常，表现为"llm response empty stream"。
  现骨架与最终对象的 `output` / `tools` 均输出空数组 `[]`，并加回归测试
  `TestResponsesObjectArraysNotNull` 锁定。

### 验证
- `response.created` 与 `response.completed` 中无 `"output":null` / `"tools":null`；
  事件序列 created → output_item.added → content_part → text.delta → completed 完整。

---

## [v0.3.36] - 2026-10-12

### 修复
- **`/v1/responses` 无锚点请求每轮开冷链，会话完全无记忆**：客户端不发
  `previous_response_id` 和 `X-Session-ID` 时（实测 CodeBuddy copilot 后端两者都不发，
  全量历史重发型），回退分支按 `resp:<随机 id>` 建链——**每个请求都是冷启动会话**，
  模型毫无上文，reasoning 空转不出正文，客户端表现为"LLM 空流"并无限重试
  （实测 40 秒连开 13 个 session）。现回退分支与 Chat 同源：`conversationKey`
  历史归属 + 桶内前缀匹配 + **差分增量发送**（全量重灌会把上游 session 撑爆）；
  `previous_response_id` / `X-Session-ID` 两条显式锚点路径行为不变。
- **responses 路径从不写 `conv.setLast` 历史快照**：`lastMessages` 永远为空——历史
  归属链的差分恒为 DiffReset（每轮 reset + 全量重发 + 新 session），`terminatedReplay`
  防重放在该路径也从未生效。现与 chat 同款在 prompt 成功后 `setLast` + `persistConv`。
  修复过程中还发现差分必须先于 `ensureSession` 计算：放后面的话新建会话会被
  DiffReset 的 `resetSession` 清掉，prompt 落到空 sid 上，执行器永远等不到终态
  （单测实测挂死）。

### 验证
- 两轮无锚点连续对话：第二轮复用同一 session（session created 计数 1）、仅发增量、
  模型记得首轮上下文；`janus/<role>@auto` 角色名经 CF 全链路 200。

---

## [v0.3.35] - 2026-10-12

### 修复
- **`janus/<role>[@auto|:variant]` 角色形式模型名报 404，客户端表现为"LLM 空流"**：
  `isJanusAlias` 只认裸名 `janus`，§4.2 设计的角色形式落到通用解析器，被当成
  `provider="janus"` 直接 404 `unknown provider "janus"`——客户端（/v1/responses 等）
  把错误体渲染成空流，模型完全不可用（实测裸 `janus`/`default` 正常、所有角色形式
  全挂）。现在角色形式统一识别为 janus 虚拟模型，解析到 web 选定的默认模型，
  Chat / Responses / Anthropic 三条路径同时生效；角色档位独立路由仍按 §4.2 属
  P4 规划。

---

## [v0.3.34] - 2026-10-12

### 修复
- **idle 会话工具调用不再中断当前会话**：MCP 请求不携带调用方 session id，无法确定调用
  出自哪个上游会话。此前按「当前绑定」中断上游——垂死 agent 的尾期调用被算到正在服务的
  会话头上（实测 2 小时内 3 次误杀活会话）；且回合以 tool_calls 交给客户端后，agent 停在
  挂起调用上等回填，本来就没有 waiter，同样会被误判成孤儿。现在 idle 会话的工具调用
  **只拒绝、不中断**：孤儿 agent 由其他路径兜底终止——幽灵调用超时中断其宿主会话（v0.3.33）、
  工具桥换绑清理、terminated 重建时的 DeleteSession，以及模型收到连续报错后自行放弃。

### 运维
- 117 环境同步调整两项配置（代码默认值）：`BRIDGE_TOOL_CALL_WAIT` 5m→30m——长命令
  （build/install）执行期间客户端明明活着，5 分钟超时被误判为"客户端离开"，触发中断 +
  会话重置，2 小时内 9 个 session 全是这么烧掉的；`BRIDGE_STREAM_IDLE_TIMEOUT` 10m→180s
  ——provider 挂起时 3 分钟内报错，客户端自动重试基本无感。

---

## [v0.3.33] - 2026-10-12

### 修复
- **幽灵工具调用超时"只丢弃"导致孤儿 agent 狂奔**：客户端终止输出后，上游 agent 停在等工具
  结果的挂起调用上；90s（长等待 30m）超时后，v0.3.31 的防误杀分支对 `callSid ≠ 当前绑定`
  的幽灵调用**只丢弃、不中断**——agent 收到超时错误结果后并不会停，手头的任务照常执行：
  实测无人观看地跑完整个实现任务（42 条消息、改代码、跑测试、更新文档），还与正在服务的
  新会话抢同一座工具桥——结果按工具名误配、idle 守卫把活会话当孤儿中断，用户看到的就是
  "发了新指令还在执行旧任务"。现在幽灵调用超时后**中断它自己所属的旧会话**（宿主已被
  替换/遗弃，中断绝不碰当前绑定的新会话）；有 waiters（并发 bucket-mate 在飞，CallSid
  可能盖错）时保守不中断，交给既有 idle 拒绝路径兜底。

---

## [v0.3.32] - 2026-10-10

### 新增
- **会话首轮注入「行为约定」：用户表示已修复即停止排查**：用户中途说「好了我修复了」之后，
  agent 仍会把手头排队的验证计划执行完——继续跑 h3/DNS/查库等一串检查，基于过期状态给出
  误导结论（实测发生过）。现在新会话首轮随 prompt 注入一条行为约定：用户表示问题
  「已修复/已解决/已自行处理」时视为该问题已关闭，立即停止为它安排的排查、验证或检查动作
  （含已计划的），简要确认后等待用户下一步指示，仅当用户明确要求验证时才继续。
  - 首轮说明合并为 `firstTurnNote`（路径映射 + 行为约定），Chat / Responses / Anthropic
    四处发 prompt 入口（含附件降级重发路径）统一走合集；
  - 行为约定对所有会话生效，不依赖 mode B 路径改写规则；与路径映射一样只注入一次随会话留存，
    不逐轮重复，**无需任何新配置**。已在途的旧会话首轮已过，不受影响。

---

## [v0.3.31] - 2026-10-10

### 修复
- **同名工具并发调用互相覆盖导致整轮卡死**：工具桥的挂起表此前以 **MCP 工具名**为键，
  agent 在一次助手消息里并行调用同一工具（实测两次 `execute_command` / 两个 `search_file`）
  时，后到的 park 会**覆盖**先到的 —— 被覆盖的调用既不会回给客户端也永远不会 complete，
  其 MCP JSON-RPC 挂死，上游 agent 卡在 tools/call 上等不到第二个结果，客户端无限
  「等待模型响应」。现在挂起表改按唯一 **callID** 为键，并发同名调用各自挂起、按序回给客户端。
- **旧回合的幽灵调用超时误杀新会话**：客户端断连/换主题后，旧回合遗留的挂起调用在工具桥里
  等到超时（`BRIDGE_TOOL_CALL_WAIT`），随后按「当前会话」中断上游 —— 把正在服务的新会话杀掉
  （实测：换主题后恰好在 5 分钟超时点整轮卡死）。现在挂起调用记录 park 时的上游 sessionID
  （`CallSid`）：超时只中断确实属于当前会话的调用，幽灵调用只丢弃；工具桥换绑新会话时
  旧调用立刻以错误收尾。
- **DB 恢复的会话继承引发跨会话干扰**：scope 模式下同一 key 桶里的新话题/子代理会话会
  无条件从库中恢复主会话的 sessionID —— 轻则 claim 反复失配、一轮请求连建最多 8 个会话把桶
  挤爆（孤儿删除风暴，实测同一 session 连删 7 次 404），重则子代理 DiffReset 时把主会话
  正在使用的上游 session 删掉。现在只有恢复出的历史确为本次请求历史的前缀时才续链
  （`restorableHistory`），其余一律重开干净会话。

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
