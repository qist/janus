# 13 维审计报告

对照 13 个维度逐条审代码 + 跑运行时验证。✅ = 已实现并实测通过，
⚠️ = 实现但有限制，❌ = 未实现。**本轮修掉的问题单独标 🔧。**

审计方式：读代码 + 对真实上游打请求 + `-race` 编译的二进制跑并发/断连压测 +
OpenAI 官方 SDK 兼容矩阵。

---

## ① OpenAI API 兼容性 ⚠️

**已实现**

| 端点 | 说明 |
|---|---|
| `GET /healthz` | 免鉴权探活 |
| `GET /v1/models` | 列表，支持 `provider` / `tools` / `modality` / `min_context` / `refresh` 过滤 |
| `GET /v1/models/{id}` | 完整详情（含 `variants` 与 `pricing`），`{id...}` 通配以支持 `provider/model` |
| `POST /v1/chat/completions` | 流式 + 非流式 |
| `POST /v1/completions` | 旧版补全，降级为单轮 chat |
| `GET /v1/usage` | 扩展端点（余额 + 用量） |

**实测**：OpenAI Python SDK 3.24.0，22 项断言全过（见 `COMPAT.md §1`）。
覆盖 `stream_options.include_usage`、`finish_reason`、`content` 三形态（string/array/null）、
错误语义 404/400/401。

**限制**

- `tools` / `tool_choice` **解析但不转发**（见 ④）。
- `temperature` / `top_p` / `n` / `stop` / `logprobs` 静默忽略。
- `max_tokens` / `max_completion_tokens` 接受但不生效（上游无此参数）。
- 非标准扩展字段：`reasoning_content`、`context_length`、`max_output_tokens`、
  `supported_reasoning_efforts`、`input_modalities`。

🔧 **修复**：未知端点的错误信息原本写死"implements /v1/models and /v1/chat/completions"，
端点已经增加到 6 个，提示过时 → 改为列出全部。

---

## ② OpenCode API 正确性 ✅（有一处已修）

所有上游调用集中 `upstream.go`，逐个实测过：

| 桥的调用 | 上游端点 | 实测要点 |
|---|---|---|
| `ListModels` | `GET /api/model?directory=` | 参数名必须是 `directory`（不是 `location`） |
| `DefaultModel` | `GET /api/model/default` | 全局一致，不随 directory 变 |
| `CreateSession` | `POST /api/session` | 必须带 `location.directory`；内联 struct 必须打 json tag |
| `SetModel` / `SetAgent` | `POST /api/session/{sid}/model|agent` | variant 原地切换，上下文保留 |
| `Prompt` | `POST /api/session/{sid}/prompt` | `delivery: queue`；只接受纯文本 + `files` |
| `ListMessages` | `GET /api/session/{sid}/message` | `order=asc&limit=N` |
| `WaitUntilIdle` | `POST /api/experimental/session/{sid}/wait` | 阻塞到空闲；**不能套短超时** |
| `Interrupt` | `POST /api/session/{sid}/interrupt` | 断连/超时时调用 |
| `GetSession` | `GET /api/session/{sid}` | 读 `time.idle` 与 `outcome` 判终态 |
| `DeleteSession` | `DELETE /api/session/{sid}` | janitor 回收 |

🔧 **修复**：`do()` 把「`out` 非 nil 但响应体为空」当成功返回 —— 上游偶发
200 + 空 body 时会把零值/空列表写进缓存。实测后果：**模型列表整个"消失"**
（客户端随后报 `unknown provider "opencode"`）。现在空 body 直接报错。

**上游自身的 bug（非桥问题）**：`fledge-alpha-free` 尝试工具调用时返回
`provider.invalid-output: "OpenAI Chat tool call delta is missing id or name"`，
任务直接失败。已记录，桥侧无法修。

---

## ③ SSE / Streaming ✅（两处已修）

- chunk 格式：首个 chunk 带 `delta.role=assistant`，末尾单个 `finish_reason`，
  `include_usage` 时追加 `choices:[]` 的 usage chunk，最后 `data: [DONE]`。
- 头部：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、
  `Connection: keep-alive`、`X-Accel-Buffering: no`。
- 流内错误：HTTP 200 已发出后出错，发 `data: {"error":{...}}` + `[DONE]`，
  而不是给一个空内容的"成功"。

🔧 **修复 1（严重）**：**ResponseWriter use-after-return 数据竞争**。
客户端断开后 handler 先返回，但 `runEvents` 协程仍在 `sw.deltaText()` 写
`http.ResponseWriter`，而 `net/http` 正在 `finishRequest` 收尾同一个连接。
`-race` 实测抓到：

```
WARNING: DATA RACE
Read at ... by goroutine 38:
  net/http.(*chunkWriter).close() / server.go:413
Previous write at ... by goroutine 110:
  main.(*sseWriter).writeChunk() / sse.go:73
  main.(*executor).addReasoning() / chat.go:139
```

修法：`sseWriter.markClosed()`，handler 用 `defer sw.markClosed()` 保证返回前
置位；与 `writeChunk` 共用同一把锁，置位后不可能再有写入在飞。
修复后同样的压测（12 并发 + 一半中途断连）**0 次 race**。

🔧 **修复 2**：**没有心跳**。agent 跑工具时可能几分钟没有增量，会被中间代理/
客户端读超时掐断。新增 `BRIDGE_STREAM_HEARTBEAT`（默认 15s），写 `: ping` 注释行。

---

## ④ Tool Calling ❌

**未实现**（Phase 3）。客户端声明的 `tools` 被解析但**不透传**，
所以客户端拿不到 `tool_calls`，只能拿到纯文本。

缓解：OpenCode agent 自己的工具活动以 `<opencode-tool>` 注释插进 `content`
（可用 `BRIDGE_TOOL_ANNOTATIONS=false` 关闭），注释在重放历史时会被剥掉。

**影响**：Trae/CodeBuddy 的 agent 模式会退化。需要在意的客户端应当：
要么只用普通对话，要么等 Phase 3 的 MCP 透传（设计见 `DESIGN.md §5.7`）。

---

## ⑤ Reasoning ✅

- `reasoning_effort`（OpenAI 标准字段）→ 上游 `variant`，含别名归一、
  就近取档、布尔档位（`none`/`thinking`）兼容（`PickVariant`）。
- 输出 `delta.reasoning_content`（非流式为 `message.reasoning_content`）。
- 模型名后缀 `provider/id:variant` 优先于 `reasoning_effort`。
- 切档走 `SetModel` **原地切换**，不重开会话。
- 对账时 reasoning 按"上游更长才补尾部"处理，避免重复。

**限制**：`reasoning_content` 不是 OpenAI 官方字段，标准客户端会忽略；
需要展示思考过程的客户端（DeepSeek 系）才读得到。

---

## ⑥ Session ✅

```
OpenAI 每轮完整 messages[]
   ↓ key = X-Session-ID 或 sha256(system + user + directory)
   ↓ 桶内严格历史前缀匹配（并发下拿到锁后复核一次）
OpenCode session ses_…   ← 上下文存这里，prompt 只发增量
```

- 实测 3 轮对话只建一次会话，后续 delta 47B/57B/61B。
- 不相关话题各占独立会话，互不干扰。
- 历史被改写/截断 → 另起会话，绝不污染。
- 完全相同的请求 → `DiffNone` 回放缓存；空缓存则**退回重跑**最后一条 user 消息。
- 会话有 TTL（默认 30m）、全局上限、单 key 候选上限，LRU 淘汰 + janitor 删上游。

🔧 **改进**：把 OpenAI 标准的 `user` 字段纳入指纹。不带它的话，同一目录、
同一系统提示词的不同终端用户会共用桶，桶无谓膨胀。

---

## ⑦ 并发 / Race ✅（两处已修，实测 0 race）

- `-race` 单测全绿。
- `-race` 编译的二进制 + 12 并发流式 + 一半中途断连压测 → **0 次 DATA RACE**。

🔧 **修复 1**：见 ③ 的 ResponseWriter use-after-return。
🔧 **修复 2**：`EventBus.subs` **无界内存泄漏** —— 订阅取消后只把切片清空、
不删 map 键，每个用过的 sessionID 都留一个空切片，长期运行 map 持续增长。
现在空桶会 `delete`。

已有保障：`Store.Acquire` 先放 `s.mu` 再拿 `c.mu`（避免慢请求卡住整个 store）；
`stateMu` 独立保护被无锁读取的字段；`cancel()` 与 `dispatch` 共用同一把锁。

---

## ⑧ Context Cancel ✅（一处已修）

实测：客户端 3 秒后断开 → 桥立即 `client disconnected, interrupting` →
上游会话 `outcome=interrupted`，正在执行的 `sleep 45` 被切断。

🔧 **修复**：`wait` 兜底协程原本用 `context.Background()`，请求结束后仍会存活
到 `RequestTimeout`（600s）—— 每次断连泄漏一个协程 + 一个上游请求。
改为派生自请求的 `ctx`，请求结束即取消。

---

## ⑨ Usage ⚠️

- `input→prompt_tokens`、`output→completion_tokens`、`cache.read→cached_tokens`、
  `reasoning→reasoning_tokens`；逐 step 累加。
- `session.usage.updated` 是**会话累计值**，刻意忽略以免重复计数。

**上游怪癖（已在 `COMPAT.md` 记录）**：

- `cached_tokens` 可能 **大于** `prompt_tokens`（上游按会话累计上报缓存命中）。
- `prompt_tokens` 基线约 5800（`build` agent 的系统提示 + 工具定义），
  简单提问也会看到这个量级。
- `reasoning_tokens` 是 `completion_tokens` 的子集（符合 OpenAI 语义）。

---

## ⑩ Responses API ❌

**未实现**。`POST /v1/responses` 返回 404（提示已列出实际实现的端点）。

**影响**：较新的客户端/SDK 默认走 `openai-responses` 协议
（上游 OpenCode 自己调 `/inference/go/openai/v1/responses` 就是这条路）。
只认 Responses API 的客户端目前无法使用本桥。

**建议**：若目标客户端会用它，值得做一版最小实现
（`input`/`instructions`/`stream` → `output[].content[].output_text`）。
请求/响应形状与 chat 差异较大，不是简单别名。

---

## ⑪ 错误码 ✅（三处已修）

两套映射：

- `mapUpstreamError`：HTTP 层错误（401/403→502、404→502、409→409、
  429→429、5xx→502、超时→504、连不上→503）。
- `mapUpstreamFailure`：上游写在 assistant `error` 字段里的失败原因
  （`provider.quota`/402→**429 `insufficient_quota`**、`provider.rate_limit`/429→429
  `rate_limit_error`、401→502 `upstream_auth_error`、其他→502 `upstream_error`）。

🔧 **修复 1**：上游以 `outcome=failed` 收尾、但 `execution.failed` 事件没收到时，
桥只按 `time.idle` 判终态，**误报 `succeeded`** —— 于是"余额不足"被显示成
"空回复"。现在读 session 的 `outcome`，`failed` 就报 failed。

🔧 **修复 2**：空回复/上游失败原本一律返回笼统的 `empty_completion`。
现在透传上游真实原因（实测返回 `429 insufficient_quota` +
`Upstream request failed: Insufficient account funds (status 402)`）。

🔧 **修复 3**：连接错误信息里的上游地址原本读 `os.Getenv("OPENCODE_URL")` ——
auto 发现模式下该变量是空的，提示变成 "cannot reach opencode server at "。
改为读实时 endpoint。

---

## ⑫ 安全 ⚠️

**已做**

- 客户端鉴权：`Authorization: Bearer` 或 `x-api-key`，`subtle.ConstantTimeCompare` 常时比较。
- 上游凭据只用于出站 Basic 头，**永不返回给客户端**、不写日志。
- 日志脱敏（`Redact`）；`/healthz` 免鉴权但只回上游连通性。
- 请求体上限 8 MiB。
- `<opencode-tool>` 注释在重放时剥除，防止通过工具输出注入上游上下文。
- CORS `OPTIONS` 预检；`X-Request-Id` 透传/生成。

**缺口**

- ✅ **速率限制已实现**（`ratelimit.go`，令牌桶）：
  - 按 API key 分桶，无 key 时按客户端 IP；另有可选全局桶防"多 key 绕过"
  - 超限返回 429 `rate_limit_exceeded` + `Retry-After` + `x-ratelimit-*`
  - `/healthz`、`/mcp/*`、`OPTIONS` 豁免（MCP 被限流会打断工具闭环）
  - `BRIDGE_TRUST_PROXY` 默认 false，避免伪造 `X-Forwarded-For` 绕过
  - 只做请求准入，不掐已建立的 SSE 流
- ✅ **CORS 默认已收紧**：未配置 `BRIDGE_CORS_ORIGIN` 时**不发任何 CORS 头**
  （= 拒绝跨源）。`*` 允许任意源但不带 credentials；白名单才带 credentials。
  被拒的预检返回 403 而不是静默无头，便于排查。
- ⚠️ **读凭据库**：`/v1/usage` 用 `sqlite3 -readonly` 读 OpenCode 的 `credential`
  表（含 OAuth access token）。这是本机同用户权限，但确实扩大了暴露面；
  不需要余额功能就设 `BRIDGE_USAGE_ENABLED=false`。
- ⚠️ 自动发现依赖 Linux `/proc` 可读与同用户权限（仅影响便利性，不影响安全）。
- ✅ **`/metrics` 已实现**（Prometheus 文本格式，零依赖）：请求量/延迟、限流、
  上游错误、工具注册与调用、活跃流、会话数、token。默认需鉴权。
- ⚠️ 仍无审计日志（谁在什么时候调了什么）。

---

## ⑬ 性能 ⚠️（未见明显瓶颈，但缺压测）

**已做**

- `/api/event` 是**全站广播**，所以只开**一条** SSE 长连接，进程内按 `sessionID`
  fan-out（`dispatch` 用非阻塞发送，缓冲 512，满则丢最旧）。
- 模型列表缓存 60s、默认模型缓存、用量报告缓存 30s。
- 会话 LRU 上限（全局 256 / 单 key 8）+ 空桶删除。
- `newID` 用 `crypto/rand`（128 bit）—— 每次响应一次，开销可忽略。
- 上游 http.Client 连接池复用；短操作 30s 兜底超时、prompt 120s、wait 不设。

**缺口 / 待办**

- ⚠️ 没有做**并发压测**（QPS / 长连接数 / 内存增长曲线），只有功能性并发测试。
- ⚠️ 每个请求一个 `reconcile` ticker + 一个 `wait` 协程；高并发下协程数
  与请求数同阶（可接受，但值得观察）。
- ⚠️ `conversation.lastMessages` 保存完整历史，长对话内存随轮数线性增长
  （被会话数上限约束，但没有单会话长度上限）。
- ⚠️ SSE 无背压：客户端读得慢时由 `net/http` 缓冲，没有主动限流。

---

## 附：本轮修复清单

| # | 问题 | 位置 | 严重度 |
|---|---|---|---|
| 1 | ResponseWriter use-after-return 数据竞争 | `sse.go` / `complete.go` | 高（`-race` 实测） |
| 2 | 上游空 body 被当成功 → 模型列表消失 | `upstream.go` / `models.go` | 高（实测 404） |
| 3 | `outcome=failed` 误报 `succeeded` | `chat.go` | 高（掩盖真实错误） |
| 4 | 空回复被缓存并回放 / 不报错 | `complete.go` / `chat.go` | 高 |
| 5 | `EventBus.subs` 无界内存泄漏 | `eventbus.go` | 中 |
| 6 | `wait` 协程请求结束后泄漏最长 600s | `chat.go` | 中 |
| 7 | 上游失败原因不透传（笼统 empty_completion） | `complete.go` / `chat.go` | 中 |
| 8 | 连接错误消息用错的上游地址 | `sse.go` | 低 |
| 9 | 未知端点提示过时 | `server.go` | 低 |
| 10 | 无 SSE 心跳（长工具任务会被掐断） | `complete.go` | 中 |
| 11 | 无 `X-Request-Id` | `server.go` | 低 |
| 12 | `user` 未纳入会话指纹 | `store.go` | 低 |
| 13 | `newID` 用自制 LCG（可预测） | `sse.go` | 中（安全） |

单测从 88 增至 114，`-race` 全绿。
