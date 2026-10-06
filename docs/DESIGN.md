# Janus 设计文档

> 目标：让 Trae / CodeBuddy 等只支持 **OpenAI 接口**的客户端，接入运行在 `0.0.0.0:2809` 的 OpenCode server，
> 并尽可能保留 OpenCode 的 **agent 会话能力**。

- 状态：设计稿 v1（待评审）
- 作者：OpenCode agent
- 日期：2026-10-03
- 实现语言：Go（`/usr/local/go/bin/go`，go1.23.4，已在 PATH 之外，需显式加入）

---

## 1. 结论先行

**OpenCode serve 不能直接被 Trae / CodeBuddy 使用。**

实测 `http://<host>:2809/openapi.json` 返回 **116 个路径，全部是 `/api/*` 自有协议**，
没有 `/v1/chat/completions`、`/v1/models` 等任何 OpenAI 端点。二进制中出现的 `chat/completions`
全部是 OpenCode **作为客户端**调用上游模型 API 的代码，不是它对外暴露的路由。

因此必须写一个 **OpenAI 兼容适配层（bridge）**，把 OpenAI 协议翻译成 OpenCode 的 session 协议。

---

## 2. 现状调研（全部为实测结果）

### 2.1 认证

OpenCode server 开启了 Basic 认证：

| 项 | 值 |
|---|---|
| 用户名 | 固定 `opencode`（源码 `ServerAuthConfig.configLayer` 硬编码） |
| 密码 | 环境变量 `OPENCODE_SERVER_PASSWORD`，**每次启动随机生成** |
| 传递方式 | `Authorization: Basic base64("opencode:<密码>")` |
| 等价方式 | `?auth_token=<同样的 base64>` |

观察到的两次运行：端口 `2809` → `61573`，密码 `3c9f…` → `db54…`。
**结论：不能硬编码，必须自动发现**（见 §5.3.1）。

验证：

```bash
B64=$(printf 'opencode:%s' "$OPENCODE_SERVER_PASSWORD" | base64 -w0)
curl -s "http://127.0.0.1:$PORT/api/info" -H "Authorization: Basic $B64"
# {"version":"2.0.22","pid":708,"urls":["http://192.168.100.128:2809"],...}
```

密码错误时返回 `401 {"_tag":"UnauthorizedError","message":"Authentication required"}`。

### 2.2 关键端点清单

| 方法 | 路径 | 用途 | 实测要点 |
|---|---|---|---|
| GET | `/api/info` | 健康检查 | 返回 version / pid / urls |
| GET | `/api/model?directory=<dir>` | **模型列表** | 参数名是 `directory`，**不是** `location`；不传或传 `location` 会返回空数组或 400 |
| GET | `/api/model/default` | 默认模型 | 当前为 `opencode/fledge-alpha-free` |
| GET | `/api/agent` | agent 列表 | `build`（默认，权限全开）、`plan` 等 |
| GET | `/api/project` | 项目列表 | 已有 `/path/to/project`、`/path/to/other`、`/root` 等 |
| POST | `/api/session` | **创建会话** | 必须用 `{"location":{"directory":"..."}}` 指定工作目录；可带 `model`、`agent`、`title` |
| POST | `/api/session/{sid}/prompt` | **发送消息** | body `{text, files?, delivery?: steer\|queue, resume?}`；返回 user 消息对象；busy 时也是 200 |
| POST | `/api/experimental/session/{sid}/wait` | 等待会话空闲 | 返回极快，需配合事件判断 |
| GET | `/api/session/{sid}/message` | **读取消息** | 游标分页；assistant 消息含 `content[]`（`text`/`reasoning`/`tool`）、`finish`、`tokens`、`cost` |
| GET | `/api/event` | **全局 SSE 事件流** | `text/event-stream`，无过滤参数，包含所有 location / session |
| POST | `/api/experimental/generate` | 一次性无状态生成 | body `{prompt, model?}` → `{data:{text}}` |
| POST | `/api/session/{sid}/generate` | 基于会话上下文的临时生成 | 不写入历史 |
| POST | `/api/session/{sid}/model` | 切换模型 | body `{model:{providerID,id,variant?}}` |
| POST | `/api/session/{sid}/agent` | 切换 agent | body `{agent:"build"}` |
| POST | `/api/session/{sid}/interrupt` | 中断执行 | 支持 `?resume=true` |
| POST | `/api/session/{sid}/compact` | 压缩上下文 | 长会话必需 |
| DELETE | `/api/session/{sid}` | 删除会话 | 用于清理 |
| PUT | `/api/experimental/mcp/{server}` | **动态注册 MCP server** | Phase 3 函数调用透传的关键 |

### 2.3 事件流（流式输出的唯一低延迟来源）

`GET /api/event` 返回 SSE，每条形如：

```
data: {"id":"evt_…","created":1791029263069,"type":"session.text.delta",
       "location":{"directory":"/root"},
       "data":{"sessionID":"ses_…","assistantMessageID":"msg_…","ordinal":0,"delta":"1\n2\n3"},
       "durable":{"aggregateID":"ses_…","seq":18,"version":1}}

: heartbeat
```

与一次补全相关的事件序列（实测）：

```
session.execution.started
session.step.started          → data.agent / data.model / data.assistantMessageID
session.reasoning.started / .delta / .ended     ← 思维链
session.text.started / .delta / .ended          ← 正式输出（delta 增量）
session.tool.called / .input.started / .success / .failed   ← agent 工具调用
session.step.ended            → data.finish = stop|tool-calls|length|error
                                data.tokens = {input,output,reasoning,cache}
session.usage.updated         → data.cost / data.tokens（累计）
session.execution.succeeded | .failed | .interrupted
```

**事件是全局广播的**（一次抓流时同时抓到了另一个 worktree 里不相关会话的 delta），
bridge 必须按 `data.sessionID` 自行分发。

### 2.4 实测发现的坑

1. **`/api/experimental/generate` 对 `opencode/*` 免费模型被拒**：
   `{"_tag":"ServiceUnavailableError","message":"OpenCode's free tier can only be used from within OpenCode"}`。
   同一请求换成 `opencode-go/deepseek-v4.1-flash` 正常返回 `{"data":{"text":"PONG2"}}`（2.1s）。
   → **无状态生成模式只对部分 provider 可用**，agent 会话模式不受此限制（实测 `opencode/fledge-alpha-free` 在 session 中正常出字）。
2. **`/api/session` 传 `projectID`/`directory` 字段无效**，会话会落在 `/root`；必须传 `location.directory`。
3. **`/api/model` 无参调用返回 `data:[]`**，必须带 `directory`。
4. `/api/event` 无法按 session 过滤，且有 `: heartbeat` 注释行需容忍。
5. `prompt` 在会话 busy 时仍返回 200（`delivery` 决定是插队 `steer` 还是排队 `queue`），**不会**返回 409，需自己保证串行。
6. OpenCode 的 `prompt` **不接受** `temperature` / `max_tokens` / `top_p` 等采样参数；只有 `variant`（`low|high|max` reasoning effort）可通过切换模型设置。

---

## 3. 目标与非目标

### 目标

- 暴露标准 OpenAI 端点：`/v1/models`、`/v1/chat/completions`（流式 + 非流式）。
- Trae / CodeBuddy 里填一个 Base URL + API Key 即可使用，**零改动**。
- 保留 OpenCode agent 能力：模型可以在会话里调用 bash / 读写文件 / 联网，多轮上下文连续。
- 流式延迟接近原生（首个 token 毫秒级，依赖 SSE 转发而非轮询）。
- 最大兼容：OpenAI 的 `stream`、`stream_options.include_usage`、`finish_reason`、`usage`、`id/created/object` 字段齐全。

### 非目标（明确排除）

- 不实现 embeddings / images / audio / batches / assistants 等非对话端点（返回标准 404 错误体）。
- 不做 OpenCode → OpenAI 的反向代理鉴权托管（密码仍由 bridge 环境变量持有）。
- 不修改 OpenCode 本体。

---

## 4. 总体架构

```
 Trae / CodeBuddy                     Janus (Go)                OpenCode server :2809
┌──────────────────┐   HTTPS/HTTP    ┌──────────────────────────────┐  Basic Auth  ┌─────────────────────┐
│  OpenAI SDK 配置  │ ──────────────▶ │  HTTP server  :2810          │ ───────────▶ │ /api/model          │
│  base=/v1        │  Bearer sk-xxx  │                              │              │ /api/session        │
│                  │                 │  ┌─ ModelsHandler ─────────┐ │              │ /prompt             │
│  POST            │                 │  │ /v1/models              │ │              │ /message            │
│  /chat/completio-│                 │  └─────────────────────────┘ │              │ /interrupt          │
│  ns  (stream)    │                 │  ┌─ ChatHandler ───────────┐ │              │ /experimental/mcp   │
│                  │ ◀────────────── │  │ 会话亲和 + 历史重放       │ │              └─────────────────────┘
│  SSE chunks      │   text/event-   │  │ 协议翻译 (OpenAI↔OCP)    │ │                    ▲
│                  │   stream        │  └───────┬────────────────┘ │                    │
└──────────────────┘                 │          │                  │              GET /api/event (SSE)
                                     │  ┌───────▼────────────────┐ │                    │
                                     │  │ EventBus               │ │◀───────────────────┘
                                     │  │ 单条长连 + 断线重连       │ │   全局事件，按 sessionID 分发
                                     │  │ 按 sessionID fan-out    │ │
                                     │  └────────────────────────┘ │
                                     │  ┌────────────────────────┐ │
                                     │  │ SessionStore           │ │
                                     │  │ 对话指纹 → ses_xxx 映射   │ │
                                     │  │ 历史快照 / TTL 回收       │ │
                                     │  └────────────────────────┘ │
                                     └──────────────────────────────┘
```

### 4.1 为什么用「单条 EventBus + fan-out」

`/api/event` 没有过滤参数，是全局广播。两种选择：

- **每请求开一条 SSE**：N 个并发对话 = N 条全局流，每条都收到全站事件，浪费且容易触发限流。
- **单条长连 + 内部分发**（选定）：进程启动即连，断线指数退避重连；每个在飞请求向 EventBus 注册
  `sessionID → chan Event`，收到事件后非阻塞投递，无人订阅的事件直接丢弃。

重连间隙可能丢事件，见 §5.5 的对账兜底。

---

## 5. 核心设计

### 5.1 端点与兼容面

| OpenAI 端点 | 实现 | 说明 |
|---|---|---|
| `GET /v1/models` | ✅ | 转换自 `GET /api/model?directory=` |
| `GET /v1/models/{id}` | ✅ | 内存查找 |
| `POST /v1/chat/completions` | ✅ | 流式 + 非流式，**核心** |
| `POST /v1/completions` | ✅（降级） | 映射为单轮 chat，非流式 |
| `POST /v1/embeddings` | ❌ | `404 {"error":{...,"type":"invalid_request_error"}}` |
| `POST /v1/images/*`、`/v1/audio/*` | ❌ | 同上 |
| `GET /healthz` | ✅ | 探活，不做鉴权 |

> 说明：所有未实现端点返回 **OpenAI 格式的错误 JSON**（不是 Go 默认 404 文本），
> 避免客户端解析崩掉——这是"最大兼容性"的关键细节。

### 5.2 模型命名

OpenAI 的 `model` 字段是自由字符串，需要映射回 `Model.Ref{providerID,id,variant}`：

```
opencode/fledge-alpha-free              → providerID=opencode, id=fledge-alpha-free
opencode-go/deepseek-v4.1-flash         → providerID=opencode-go, id=deepseek-v4.1-flash
opencode/claude-sonnet-5-5:high         → …, variant=high
fledge-alpha-free                       → 在已知列表中唯一匹配则采用，否则 404
```

规则：

1. 按 `/` 拆成 `provider/id`，再按 `:` 拆出可选 `variant`。
2. 不含 `/` 时，在 `/api/model` 结果里做唯一匹配（大小写不敏感）；多匹配返回 404 并附带候选列表。
3. `/v1/models` 返回的 `id` 使用**完整 `provider/id` 形式**，保证客户端回传时无歧义。
4. `GET /api/model` 缓存 60s（模型列表变化不频繁），`/v1/models?refresh=1` 强刷。

**模型别名**：客户端可填 `default` / `auto` / 留空，桥会实时解析为上游
`GET /api/model/default` 的当前值（`ModelCache.Default` 带缓存）。
`/v1/models` 首条固定输出虚拟模型 `default`，让客户端下拉框里就有"用上游默认"
这个选项 —— 避免把 `opencode/fledge-alpha-free` 这类具体名字写进客户端配置，
上游换默认时无需改配置。

**思考强度（`reasoning_effort`）**：上游模型的 `variant` 实测就是思考强度档位
（`variant.settings.reasoningEffort = <档位>`），可用值实测为
`none/minimal/low/medium/high/xhigh/max/thinking`（因模型而异）。

桥把 OpenAI 标准字段 `reasoning_effort` 映射到 variant（`PickVariant`）：

| 输入 | 结果 |
|---|---|
| 空 / `default` / `auto` | 不指定 variant，用模型默认 |
| 与某 variant id 完全一致 | 直接用（支持 `xhigh`/`max` 等扩展档位） |
| 别名（`min`/`mid`/`extra-high`/`maximum`…） | 归一化后匹配 |
| 模型只有 `none`/`thinking` | 非 `none` 一律 `thinking` |
| 其余 | 按强度取最接近的，并列取更弱 |
| 模型无 variants / 档位名不认识 | 忽略该参数并 WARN |

模型名后缀 `provider/id:variant` 优先级高于 `reasoning_effort`（更具体）。

切换档位走 `POST /api/session/{id}/model` **原地切换**，不重开会话 ——
实测切换后 session 的 `model.variant` 正确变化且上下文保留。
因此 `reasoning_effort` 被纳入 `Conversation.model` 的比较：客户端改档位时
桥会感知并同步上游。

回显给客户端的 `model` 名**刻意不带 variant**：思考强度是独立参数，
混进模型名会让客户端误以为换了模型。

**上下文与输出上限**：`/v1/models` 在标准的 4 个字段之外，附加
`context_length` / `max_output_tokens` / `supported_reasoning_efforts` /
`input_modalities`。这些是 OpenRouter/LiteLLM/vLLM 的通行扩展，标准客户端忽略。

**模型列表过滤**（`handleListModels`，用 `ModelFilter`）：

| 查询参数 | 作用 |
|---|---|
| `provider=opencode-go` | 只保留该 provider |
| `tools=true` | 只保留支持工具调用的 |
| `modality=image` | 只保留支持该输入模态的 |
| `min_context=200000` | 只保留上下文 >= N 的 |
| `refresh=1` | 强制刷新上游缓存 |

有任一过滤条件时不注入虚拟 `default` 条目，避免污染过滤结果。

**模型详情**（`GET /v1/models/{id}` → `ModelDetail`）：在列表字段外再给
`name` / `family` / `status` / `enabled` / `tool_calling` / `capabilities` /
`variants`（含底层 `settings`）/ `pricing`（USD / 百万 token）。
列表保持轻量，详情才给全量 —— 否则 68 个模型的 `variants`+`pricing` 会让
`/v1/models` 体积翻好几倍。

> `max_tokens` / `max_completion_tokens` **接受但不生效**：OpenCode 的 prompt
> 接口没有输出上限参数（`Model.Compatibility.maxTokensField` 只是内部配置），
> 上限由模型自身决定。桥不报错也不假装生效。

### 5.3 会话亲和与历史重放（**最关键的设计**）

OpenAI 协议是**无状态**的：每次请求都带完整 `messages[]`。
OpenCode session 是**有状态**的：历史存在服务端，`prompt` 只发增量。

需要一个映射层。**注意：这个映射不是 key → 单会话，而是 key → 候选桶。**

**为什么分桶**（实测教训）：指纹只覆盖 `(system, user, directory)`，而真实客户端
常常**不带 system 消息**。此时所有对话共享同一个 key。如果严格一一映射，
两个不相关话题会互相挤掉对方的会话 —— 实测 `compat_test.py` 每个用例都在
触发 `history reset` 并重建会话。

改为分桶后，key 只是"分桶依据"，桶内按历史前缀挑会话：

```
ConversationKey ──► [ Conversation, Conversation, ... ]   // 按活跃度排序
                    每个 Conversation = { sessionID, lastMessages[], model, agent }
```

**ConversationKey 排序优先级**：

1. 请求头 `X-Session-ID`（显式指定）
2. 请求头 `X-OpenCode-Session`（别名）
3. 指纹 `sha256(首条 system + user + directory)` 的前 8 字节 hex
   （`user` 是 OpenAI 标准字段，标识终端用户；不带它会让不同用户共用一个桶）

**选桶 + 前缀匹配**（`Store.Acquire`）：

```
LCP = longestCommonPrefix(stored, incoming)   // 逐条比较 role/content/tool_calls

只接受：LCP == len(stored)          // stored 是 incoming 的严格前缀
若多个候选命中，取 LCP 最长者
没有任何候选命中 → 新建一个 Conversation，放进桶首
```

**为什么只认严格前缀**：这样 delta 永远是纯增量，上游 session 里绝不会混入
与客户端不一致的历史。客户端改写/删减历史时（LCP < len(stored)），宁可新开
一个会话丢上下文，也不污染已有会话。

**DiffNone（完全重复的请求）**：缓存 `lastResponse` 直接回放，毫秒级。

> ⚠️ **实测踩坑**：上游偶发返回空回复（观测到一次 91s 后 `content` 与
> `reasoning` 全空、`finish=stop`）。如果把这个空结果当正常回答缓存，之后
> **每个完全相同的请求都会命中 DiffNone 并原样回放空结果**，表现为"桥坏了"
> 且怎么重试都没用。
>
> 因此：**空回复不入缓存**（`responseHasContent`），且 DiffNone 遇到空缓存时
> 不返回空，而是**退回重跑最后一条 user 消息**（`lastUserTurn`）。

**并发复核**：空历史的会话"匹配一切"，并发首请求会同时选中同一个，然后在
`c.mu` 上排队，后来者才发现前缀对不上。因此 `Acquire` 在拿到 `c.mu` 之后会
**复核一次前缀匹配**，不匹配就解锁并回到桶里重挑（最多 8 次，之后强制新建）。
实测修复前并发首请求会多绕一轮 DiffReset，修复后 `history reset` 归零。

**delta 序列化**：OpenCode 的 `prompt.text` 是纯字符串，需要把 OpenAI 的多条消息压平：

```
[System]
<system content>

[User]
<user content>

[Assistant]
<assistant content>

[User]
<本次新增>
```

- 单条 `user` 增量不打角色标签，读起来更自然（`FlattenDelta`）。
- `role: "tool"` 序列化为 `[Tool: <name>]\n<content>`。
- `role: system` 出现在中间时按普通消息处理。
- 本桥注入的 `<opencode-tool>` 注释在压平时被 `stripToolAnnotations` 剥掉，
  不会回灌上游造成上下文膨胀。

**会话创建**：

```jsonc
POST /api/session
{ "title": "<首条用户消息前 40 字>",
  "location": {"directory": "<配置的默认工作目录>"},
  "agent": "build",
  "model": {"providerID": "…", "id": "…", "variant": "…"} }
```

**模型一致性**：请求 `model` 与已绑定会话的 `model` 不一致时，调用
`POST /api/session/{sid}/model` 切换，而不是重开会话（保留上下文更符合客户端预期）。

**会话生命周期**：全局上限 `BRIDGE_MAX_CONVERSATIONS`（默认 256），
单 key 候选上限 8，均按 LRU 淘汰。被淘汰的会话进入 `orphans` channel，
由 janitor 异步 `DELETE /api/session/{sid}`；`orphans` 满则丢弃，靠 TTL 兜底。
`BRIDGE_SESSION_TTL`（默认 30m）内无人访问也会被 janitor 回收。

### 5.3.1 上游端点自动发现

**实测问题**：OpenCode 桌面端每次重启都会换随机端口和随机密码
（观察到的 2809 → 61573，密码 UUID 也变了）。任何写死的连接配置都撑不过
一次重启，桥会在上游重启后彻底失效。

**方案**（`discover.go`）：

1. 扫 `/proc/<pid>/cmdline`，找 basename 为 `opencode` 且带 `serve` 子命令的进程
2. 从 `--hostname` / `--port` 拼候选地址（`0.0.0.0`/`::` 归一成 `127.0.0.1`）
3. 从 `/proc/<pid>/environ` 读 `OPENCODE_SERVER_PASSWORD`
4. 逐个用 Basic 认证探活 `/api/info`，第一个 2xx 即采用

启动时发现一次；`Upstream` 的 `base/auth` 由 RWMutex 保护，支持运行期热替换。
`WatchUpstream` 每 10s 探活，连续失败约 30s 后重新发现，探到新端点就
`SetEndpoint` 并让 EventBus 主动断开重连（见 §5.5）。

`OPENCODE_URL` 默认 `auto`；显式配置则不做覆盖，只探活告警。

> 依赖 Linux `/proc` 与同用户权限。其他平台请显式配置端点。

### 5.4 流式转换（`stream: true`）

```
1. 计算 delta、确保会话就绪、向 EventBus 订阅 sessionID     ← 必须在发 prompt 之前完成
2. POST /api/session/{sid}/prompt  {"text": delta, "delivery": "queue"}
3. 转换器读取 chan Event，逐条写 SSE：

   session.reasoning.delta    → chunk.choices[0].delta.reasoning_content = data.delta
   session.text.delta         → chunk.choices[0].delta.content = data.delta
   session.tool.called        → 见 §5.7（Phase 1 转成 content 里的说明文本）
   session.step.ended         → 记录 finish / tokens，本步暂不关闭流
   session.execution.succeeded→ finish_reason 映射 + 最终 usage + data:[DONE]
   session.execution.failed   → 先输出 error chunk，再 [DONE]
   session.execution.interrupted → finish_reason="stop"

4. 连接断开 / 客户端断流 → POST /api/session/{sid}/interrupt
```

**finish_reason 映射**（OpenCode → OpenAI）：

| OpenCode `finish` | OpenAI `finish_reason` |
|---|---|
| `stop` | `stop` |
| `length` | `length` |
| `tool-calls` | `tool_calls`（Phase 1 无 client 工具时退化为 `stop`） |
| `content-filter` | `content_filter` |
| `error` / `unknown` | `stop` + error chunk |

**字段细节**（兼容性陷阱高发区）：

- 每个 chunk 必带 `id`（`chatcmpl-<32 hex>`，整流复用）、`object:"chat.completion.chunk"`、`created`、`model`、`choices:[{index:0,...}]`。
  随机部分用 **`crypto/rand` 的 128 位**（`newID()`），不用时间/PID/自增拼的伪随机：
  这个值会返回给客户端，可能被当作幂等键、日志关联键或缓存键，可预测或高并发下
  碰撞都不合适。取不到熵时退化为「时间+进程+递增」，保证唯一性、牺牲不可预测性。
- 首个 chunk 发 `delta:{"role":"assistant","content":""}`。
- `stream_options.include_usage=true` 时，在 `[DONE]` 前额外发一个 `choices:[]` 的 usage chunk。
- `content` 为 `null` 时用 `""`（部分 SDK 严格校验）。
- `usage` 结构：`{prompt_tokens, completion_tokens, total_tokens, prompt_tokens_details:{cached_tokens}, completion_tokens_details:{reasoning_tokens}}`。
  来源 `session.step.ended.data.tokens`：`input→prompt_tokens`、`output→completion_tokens`、`cache.read→cached_tokens`、`reasoning→reasoning_tokens`。
- 推理字段兼容：OpenCode 的 reasoning 字段名随 provider 变化（实测见 `state.reasoningField="reasoning_content"`），
  bridge 统一输出 `delta.reasoning_content`，同时在非流式响应里同时给 `reasoning_content` 与 `reasoning` 双字段。

**SSE 写出**：`Content-Type: text/event-stream; charset=utf-8`、`Cache-Control: no-cache`、
`Connection: keep-alive`、`X-Accel-Buffering: no`；定期写 `: ping` 注释行防中间层超时。

### 5.5 事件断线与对账

EventBus 断线、或上游切端点重启（§5.3.1）期间都可能漏事件。兜底是**四路信号并行**：

1. **SSE 事件**：低延迟正文/推理增量（主路径）。
2. **空闲轮询**：每 `idlePollInterval`（默认 1s）`GET /api/session/{sid}`，
   若 `time.idle >= 本次 prompt 时间` 则判定本轮结束。这是最可靠的终态判据。
3. **`/wait` 兜底**：另起协程调 `POST /api/experimental/session/{sid}/wait`，
   它会**阻塞到 agent loop 空闲**（实测 4.0s 后返回 204）。作为事件流彻底失效时的兜底；
   返回后再用 `time.idle` 复核一次，避免"prompt 尚未开始执行"时误判。
4. **定期对账**：每 `reconcileInterval`（默认 3s）`GET /api/session/{sid}/message?order=asc`，
   把上游完整文本与已发出的 `serverText` 比较：
   - 上游是已发内容的前缀延长 → 补发差额
   - 两者相同 → 不动
   - 上游反而更短 → 不动（已发出的撤不回来）
   - 分叉 → 补未覆盖的尾部并打 WARN

**关键实现细节**：对账必须拿**上游纯文本**（`serverText`）比较，不能用
"最终输出文本"。因为工具注释会插进输出，让输出文本比上游文本长，按长度切片会
直接 panic（实测 `slice bounds out of range [126:5]`）。所以 `executor` 维护两个
builder：`text`（含注释，回给客户端）与 `serverText`（纯上游文本，仅用于对账）。

**端点热切换**：`EventBus.Reconnect()` 通过取消当前连接的 context 立即断开 SSE，
并且这次断开不计入失败退避（`consumeReconnect` 标记）。

### 5.6 非流式（`stream: false`）

复用同一条执行链，只是不写 SSE：

```
订阅事件 → prompt → 阻塞直到执行终态（事件 / idle 轮询 / wait 兜底）
→ 对账补齐 → 从 executor 汇总 text/reasoning/finish/usage
→ 单个 JSON 响应
```

超时（默认 600s）返回 504 并调用 `interrupt` 防止会话继续空转；
客户端断连则同样 `interrupt`，避免继续烧 token。

### 5.7 工具调用（分阶段）

OpenCode 的 agent 自己会调用 **OpenCode 的工具**（bash/edit/read/webfetch…），这些与客户端声明的
OpenAI `tools` 是两套东西。

**Phase 1（当前）**：客户端 `tools` 参数被接受但不透传。agent 执行工具时，把工具活动以注释形式
插入输出流：

```
<opencode-tool> bash: ls -la /path/to/project</opencode-tool>
```

这样 Trae/CodeBuddy 能看到"模型确实做了事"，但**无法由客户端执行函数**。
注释在重放历史时被剥掉（§5.3），不会回灌上游污染上下文。
`BRIDGE_TOOL_ANNOTATIONS=false` 可完全关闭（客户端自己做 agent 循环时应关闭）。

**实测细节**：工具名来自 `session.tool.input.started` 事件的 `data.name`；
`session.tool.input.ended` 携带完整入参，用于补充注释内容。

**Phase 3（函数调用透传，推荐架构）**：利用 OpenCode 支持动态注册 MCP server 的能力
（`PUT /api/experimental/mcp/{server}`）：

```
bridge 暴露一个 Streamable HTTP MCP server（内嵌在 :2810 的 /mcp）
  ├─ tools/list  → 返回"当前请求声明的 OpenAI tools"
  └─ tools/call  → 收到 OpenCode 的工具调用
                    ↓
      bridge 把该调用挂起，登记 pendingCall{callID, name, args}
                    ↓
      请求 #1 的 SSE 输出 chunk: finish_reason="tool_calls",
              delta.tool_calls=[{id:index,name,arguments}]
      请求 #1 结束
                    ↓
      客户端按 OpenAI 协议发起请求 #2，携带 role="tool" 结果
                    ↓
      bridge 解析结果 → 应答挂起的 MCP tools/call → OpenCode agent 继续
                    ↓
      请求 #2 注册为该会话新的流订阅者，继续转发后续 delta
```

该方案的优点是**完全符合 OpenAI 的两轮工具调用协议**，且不改 OpenCode。风险点：
MCP `tools/list` 的调用时机是否覆盖"每次 step"需要做 spike 验证；不行则退回 JSON-in-text 解析方案
（在 system 里要求模型输出约定格式的 JSON，bridge 解析后转成 `tool_calls`）。

**注册名带工具指纹**：实测 OpenCode 对"同名 MCP server 重新 PUT"**不会**重拉 `tools/list`，
模型会一直看到旧工具（同一会话里客户端换了 tools 就会串）。因此实际注册名是
`<base>-<工具集指纹前 8 位>`（`mcpRegName`）：工具集一变就是"新 server"，OpenCode 必然
重新连接、重列工具；紧接着删掉上一个名字，删不掉就交给 janitor 扫残留。
`ToolBridge.Alias` 让 janitor 的 `byName` 认得指纹名；`Register` **不**把基名放进 `byName`
（否则 OpenCode 里残留的旧基名会被误判为活跃、永远清不掉）。

**防串会话**：OpenCode 会把同一 location 下注册的**所有** MCP server 都暴露给每个
session，模型可能引用到别的会话的 server。那种调用落到一个当前没有在飞请求的会话上，
若照常挂起就会一直等到 `BRIDGE_TOOL_CALL_WAIT`（5 分钟）才报错。因此 `tools/call`
先 `waitForWaiter(3s)`：目标会话没有在飞请求（没有 executor 注册 watch）就直接返回
`isError`，立即失败而不是挂死。

**一轮结束即释放**：为从源头减少"别的会话看到本会话工具"，一轮请求结束、且没有
待回填的工具结果时，`releaseToolsIfIdle` 立即 `RemoveMCP` 注销本会话的 server；
下一轮用到时再注册（工具集不变则指纹名相同）。否则空闲 server 会一直挂到
`BRIDGE_SESSION_TTL`（默认 30m），期间被同 location 的其它会话的模型看到甚至调用。

### 5.7.1 权限请求自动应答（`BRIDGE_PERMISSION_REPLY`）

agent 调自己的工具（shell/read…）访问**会话目录之外**时，OpenCode 会先发
`permission.asked`（如 `action=external_directory, resources=["/srv/data/*"]`），
等审批。headless 桥没有 UI 去点，不应答的话工具会一直挂到客户端超时——
表现为"模型超过 N 秒没有返回数据"。

桥对**自己创建/服务过的 session**自动应答（`once`/`always`/`reject`，默认 `once`）：

- 事件流里收到 `permission.asked` → 立即 `POST /api/session/{sid}/permission/{id}/reply`
- 定时扫描（5s）兜底：agent 会在两次客户端请求之间的空档异步发起权限请求，
  单靠事件流可能落空档，所以按 `GET /api/permission/request` 再扫一遍

判定"属于自己的 session"用 Server 的 `owned` 集合（`markOwnedSession` 在
`ensureSession` 时登记），**不是**"当前有没有在飞请求"——后者会漏掉空档期的请求。
desktop 端等其他会话一律不碰。

### 5.7.2 中心部署 / 客户端执行（orchestrator agent）

默认 agent 是 `build`，它会用 OpenCode 的**内置工具**（`shell`/`read`/`edit`…），
这些在**桥所在的机器**上执行 —— 于是"代码在哪台机器，agent 就得部署在哪台机器"。

要改成**桥只部署一处、工具在客户端执行**，桥需要一个"只允许客户端 MCP 工具、
禁用内置工具"的 agent，并让桥用它（`BRIDGE_AGENT=orchestrator`）。

**该 agent 由 janus 自动生成并注入**（`opencode_config.go` → `OPENCODE_CONFIG_CONTENT`，
优先级高于全局/项目配置），**不需要手写** `~/.config/opencode/opencode.jsonc`：

```jsonc
{
  "agents": {
    "orchestrator": {
      "mode": "primary",
      "permissions": [
        { "action": "*",       "resource": "*", "effect": "deny"  },  // 禁掉所有内置工具
        { "action": "execute", "resource": "*", "effect": "allow" },  // Code Mode 必需
        { "action": "ob-*",    "resource": "*", "effect": "allow" }   // 只放行工具桥的客户端工具
      ]
    }
  }
}
```

**为什么是白名单而不是逐个 deny**：黑名单会随 OpenCode 版本改工具名而失效 —— 实测
v2 就漏过 `execute`/`search` 之类的新动作，导致 orchestrator 仍能跑主机工具。
`execute` 必须放行（V2 里客户端 MCP 工具挂在 Code Mode 的 `execute` 下，deny 掉它
模型一个 MCP 工具都看不到）；`ob-*` 是 janus 工具桥注册的 MCP server 名前缀
（`mcpNamePrefix`）。内置工具被 `*` deny 覆盖。

**前提**：注入只对 **janus 自己拉起的** OpenCode 生效。若本机另有 OpenCode 在跑，
默认会复用它、注入不生效 —— 设 `OPENCODE_REUSE_EXTERNAL=false` 让 janus 总是自管上游。

代价：完全依赖客户端声明的工具集，且每次工具调用多一轮 MCP 往返。
`question` 被 `*` deny 覆盖 —— 桥只自动应答权限，没人应答交互式提问，会挂住。

### 5.7.3 图片附件

OpenAI 的 `content` 数组形态（`image_url`）已支持；Responses 的 `input_image`
（`image_url` 为字符串或 `{url}`）也已解析并转成同一套内部 `ContentPart`。
关键实测结论：**上游只接受 `data:` URI**，传 `https://` 会得到
400 `Unsupported attachment URI`。

因此 `attach.go` 的处理：

| 输入 | 处理 |
|---|---|
| `data:image/...;base64,...` | 校验 base64 合法性后直传 `files[].uri` |
| `http(s)://...` | 下载（20s 超时 / 12MB 上限）→ 转 data URI |
| `file://` 等 | 不支持，进失败列表 |

**模型能力门控**（"模型支持就接收"）：`filterAttachmentsByModel` 按模型
`capabilities.input` 过滤，纯文本模型收到图片/PDF 会丢弃并在 prompt 里说明，
而不是硬塞导致上游报错。

单次 prompt 最多 8 个附件。超限/下载失败/校验失败的 URL 进失败列表，由
`attachFailureNote` 以文字补进 prompt：

```
[注意] 以下图片未能附带，无法查看：
- https://example.com/a.png
```

这一步是必要的：实测不做提示时，模型会自信地回答"未看到图片"，用户无从知道
是桥丢了附件。另外若带附件发 prompt 失败，会**降级重发一次纯文本**，保住对话本身。

### 5.8 认证与安全

**对客户端**（Trae/CodeBuddy）：

- 校验 `Authorization: Bearer <BRIDGE_API_KEY>`。
- 未配置 `BRIDGE_API_KEY` 时接受任意 Bearer（方便内网快速跑通），但会在日志打印 WARN。
- 额外接受 `x-api-key`（部分客户端用这个头）。
- `GET /healthz` 免鉴权（供探活）。

**对 OpenCode**：

- 认证凭据来自显式配置或自动发现（§5.3.1）；**密码永不写入日志、永不返回给客户端**。
- 日志对 `Authorization`、`x-api-key` 做脱敏。
- CORS：`BRIDGE_CORS_ORIGIN` 为空时回显请求方 Origin，支持 `OPTIONS` 预检。

**网络**：

- bridge 默认监听 `0.0.0.0:2810`。⚠️ 暴露到 0.0.0.0 意味着**任何能访问该机的人都能用你的模型额度**，
  `BRIDGE_API_KEY` 必须设置；若仅本机 Trae 使用，建议监听 `127.0.0.1`。
- OpenCode 本身当前已监听 `0.0.0.0:2809`，其 Basic 密码等同于全权令牌，建议同样收敛到 127.0.0.1 或改用反代。

### 5.9 错误映射

| 场景 | HTTP | OpenAI error.type |
|---|---|---|
| 缺少/错误 API Key | 401 | `invalid_request_error` |
| 未知 model | 404 | `invalid_request_error` |
| OpenCode 认证失败 | 502 | `api_error`（不透传上游 401 细节） |
| OpenCode 不可达 | 503 | `api_error` |
| 上游配额/余额不足 | 429 | `rate_limit_error` |
| 执行超时 | 504 | `api_error` |
| 未实现端点 | 404 | `invalid_request_error` |
| 请求体解析失败 | 400 | `invalid_request_error` |

错误体统一：

```json
{"error":{"message":"…","type":"…","param":null,"code":"…"}}
```

### 5.10 并发与资源

- 同一 ConversationKey 的请求串行化：进程内 `per-session mutex`；若发现上游会话仍在执行
  （`/api/session/{sid}` 的 `outcome` 或事件状态），新请求用 `delivery:"queue"` 排队。
- EventBus fan-out channel 带缓冲（64），满了丢弃最旧 delta 并置 `degraded` 标记，收尾时用对账补齐。
- SessionStore TTL 默认 30min 无活动即 `DELETE /api/session/{sid}`；可配置 `BRIDGE_KEEP_SESSIONS=1` 保留。
- 优雅停机：收到 SIGTERM 后停止接受新请求，`interrupt` 所有在飞会话，关闭 EventBus，最多等 10s。

### 5.11 客户端"能力协商"细节

为提升 Trae / CodeBuddy 的成功率：

- 响应加 `Access-Control-Allow-Origin`（可配，对应 OpenCode 的 `--cors` 机制）。
- 支持 `OPTIONS` 预检。
- `POST` body 允许 `application/json; charset=utf-8`（有客户端会带 charset）。
- 忽略并容忍客户端发来的 `n`、`logprobs`、`presence_penalty` 等 OpenCode 不支持的字段（不报错）。
- `max_tokens` 尝试映射：若请求值 < 模型 `limit.output`，通过 `session.model` 的 variant 或
  直接忽略（Phase 1 忽略，Phase 2 评估是否值得注入 system 提示约束输出长度）。

---

### 5.12 用量与余额（`GET /v1/usage`）

OpenAI 没有对应端点，这是本桥的扩展。**已消耗**在本地 server
（`/api/experimental/session/stats`），**余额**在 console API，需要 OAuth 凭据。

实现（`usage.go`）：

```
GET /v1/usage（bridge API key 鉴权）
  └─ UsageClient.Report(ctx)   // 30s 缓存
       ├─ readCredential()      // sqlite3 -readonly 读 credential 表
       └─ 并发 4 个 console 请求
            ├─ /billing/status      → 余额 / 可用 / 计费模式
            ├─ /usage/summary       → 总消耗
            ├─ /usage/models        → 按模型
            └─ /usage/cost-by-day   → 按天
```

**为什么用 `sqlite3` CLI 而不是 Go 驱动**：本桥坚持零第三方依赖（stdlib only），
而 `sqlite3` 在目标环境是常装的。取不到凭据时端点返回 503 并给出可读原因，
**不影响对话功能**。

**单位**：console 的金额字段是 `*MicroCents`（1 USD = 1e8），且以**字符串**
形式返回（如 `"856750577"`），所以自定义了 `microCents` 与 `flexInt` 两个
`UnmarshalJSON`，兼容字符串/数字两种形式。

**关键结论**：额度是**账号级**的，不存在"按模型的剩余额度"；`/usage/models`
给的是各模型**已消耗**。要按模型限额需用 console 的 budget，实测当前账号未配置。
详见 `COMPAT.md §4`。

### 5.14 Responses API（`/v1/responses`）

与 Chat Completions **共用同一套会话/执行/工具内核**（Store + executor + ToolBridge），
只换输入输出形状（`responses.go` / `responses_stream.go`）：

```
instructions + input[]            ← Responses 形状
      ↓ 转换
Canonical messages / ToolSpec
      ↓ 复用
Store + ensureSession + executor + ToolBridge
      ↓ 转换
output[]（reasoning / message / function_call）
```

**`previous_response_id` 的实现**：把响应链映射到独立会话键 `resp:<id>`，
用 `Store.AcquireKey`（**不做历史前缀匹配**）取那一个会话，每轮只把新增 `input`
作为 prompt 发出。原因：Responses 是"无状态客户端 + 有状态服务端"语义，
`input` 只含增量，套 Chat 的 Diff 前缀匹配会误判成新会话。

**流式**：事件带 `event:` 名与 `sequence_number`：
`response.created` → `response.reasoning_summary_text.delta` / `response.output_text.delta` →
工具调用时：`response.output_item.added` → `response.function_call_arguments.delta` →
`response.function_call_arguments.done` → `response.output_item.done`（并行时 `output_index` 递增）
→ `response.completed`。空回复发 `response.failed`。兼容性矩阵见 `tests/compat_matrix.py`。

**响应存储**：`responseStore`（内存 + TTL），供 `GET` / `DELETE` / `previous_response_id` 使用。
配置 `BRIDGE_DB` 后用 `github.com/qist/sqlite`（+gorm）写穿到本地 SQLite，并额外持久化
"会话键 → 上游 sessionID（+ model/agent/dir）"与 **Chat 历史快照** —— **进程重启后
`previous_response_id` 续链，以及 Chat 的历史前缀匹配都仍有效**（见 `db.go`、
`TestResponsesChainSurvivesRestart`、`TestChatHistorySurvivesRestart`）。留空/`memory`/`off` 时为纯内存。

**顺带修的两个健壮性问题**：
- `wait` 在"会话本就空闲"时立刻返回，原实现会立刻重挂 → 变成每秒上千次打上游的忙等。
  现在重挂前退避 500ms。
- `Config` 零值危险：`MaxBodyBytes=0` 让所有 POST 读到 EOF；`RequestTimeout=0`
  让 `context.WithTimeout(ctx, 0)` 立刻超时。`NewServer` 现在统一 `applyDefaults()`。

#### Session 映射与隔离（Chat / Responses 共用内核）

统一的三层映射：

```
OpenAI 会话 / response chain
        ↓ ConversationKey（Chat）/ previous_response_id（Responses）
Janus Conversation（Store：历史快照 + 上游 sessionID + 工具上下文）
        ↓ ensureSession
OpenCode session
```

- **Chat**：`key = 显式头(X-Session-ID / X-OpenCode-Session) > hash(system + user + directory)`，
  桶内再按"历史前缀严格匹配"挑会话（见 5.3）。
- **Responses**：`key = resp:<id>`；`previous_response_id` 直接复用上一条响应的 `convKey`，
  即同一个 Janus Conversation / 同一个 OpenCode session，每轮只发新增 `input`。
- 两种 API **共用同一个 Store + executor + ToolBridge**，只是键空间不同（`x:` / `f:` / `resp:`）。

**并行 / 串行工具调用**：默认并行——agent 同一轮发起的多个调用一次性回给客户端。
请求带 `parallel_tool_calls:false` 时，executor 每轮只 `takeOldest()` 一个（其余留在
会话里），客户端回填后逐个释放，实现串行语义；两个 API 都支持。

隔离保证（均有测试）：

| 场景 | 期望行为 | 测试 |
|---|---|---|
| 并发首请求、内容不同 | 各自独立会话 | `TestStoreConcurrentFirstRequestsGetDistinctSessions` |
| 同分桶、内容不同 | 独立会话 | `TestStoreSameBucketDifferentContentIsolated` |
| 同话题多轮 | 命中同一会话（续接） | `TestStoreConcurrentSameConversation` |
| Responses 续链 | 复用同一 OpenCode session、只发增量 | `TestResponsesPreviousResponseReusesSession` |
| 工具结果续链 | 回填挂起的 MCP 调用、不把 output 当 prompt 重发 | `TestResponsesToolResultContinuation` |

工具调用的 OpenAI 语义：一轮可返回多个并行 `tool_calls`（流式 `index` 0/1/2…，`tool_call_id`
原样保留），客户端按 `tool_call_id` 回填 `role:"tool"` 结果（`toolResultsFromMessages`）。

**残留风险（已知限制）**：既没有显式会话头、也没有 `user` 字段，且两个 client 的首条历史
**完全相同**时，会被视为同一条会话线 —— 这正是"多轮自动续接"所依赖的匹配，无法同时满足
"内容相同也要隔离"。规避：客户端带上 `user` 字段，或 `X-Session-ID` / `X-OpenCode-Session` 头。

### 5.15 Anthropic Messages API（`/v1/messages`）

让 Claude Code / Anthropic SDK 直接使用 Janus。与 Chat/Responses **共用同一套
Store + executor + ToolBridge**，`anthropic.go` / `anthropic_stream.go` 只做形状转换：

| Anthropic | 内部 |
|---|---|
| `system`（string / []block） | 前置一条 `role:system` 消息 |
| user content `text` / `image` | 文本 + `ContentPart`（image_url） |
| user content `tool_result` | `role:"tool"` 消息（`tool_call_id` = `tool_use_id`） |
| assistant content `tool_use` | assistant 的 `tool_calls`（`input` → `arguments`） |
| `tools[].input_schema` | `ToolSpec.parameters` |
| `stop_reason` | `tool_use` / `max_tokens` / `end_turn` ↔ `finish_reason` |

- 流式：`message_start → content_block_start → content_block_delta`（`text_delta` /
  `input_json_delta`）`→ content_block_stop → message_delta → message_stop`
- 鉴权 `x-api-key`（也接受 Bearer）；`anthropic-version`/`anthropic-beta` 忽略
- 接口同时挂在 `/v1/messages` 与 `/anthropic/v1/messages`（对齐 DeepSeek 的 `/anthropic` 约定）
- **模型路由（档位）**：CC 只用 `claude-opus*`/`claude-sonnet*`/`claude-haiku*` 等少数档位名，
  因此**无需逐模型映射**。解析优先级：`BRIDGE_MODEL_MAP`（精确/前缀 `*`）→ 请求本身可解析 →
  `BRIDGE_DEFAULT_MODEL`（多数用户只配这一个，所有档位走它）→ 按档位启发式自动挑选
  （排除经 API 会 403 的 `opencode/*` 免费额度；opus 挑强、haiku 挑便宜快）。见 `cmd_models.go`
- **映射可见性 / 选型**：`janus models` 列出全部可用模型（价格/上下文/能力/可用性）并打印三档
  当前映射；`GET /anthropic/v1/models`（或带 `anthropic-version` 头的 `/v1/models`）返回 Claude Code
  模型选择器认的 Anthropic 格式，`display_name` 里带上实际映射目标
- **服务端工具 `web_search`**：CC 声明 `{"type":"web_search_20250305"}` 时，Janus 把它作为
  “桥内部执行”的工具（`toolSession.serverTools`）暴露给 agent：agent 调用它 → `handleMCP`
  直接执行（调用上游 `/api/websearch`）并把结果回喂 agent，**不**作为 `tool_use` 甩回 CC。
  这部分在 MCP `tools/call` 里先于“防串会话”守卫拦截。开关 `BRIDGE_WEBSEARCH_ENABLED`
- **会话锚点**：`x-claude-code-session-id` 头自动作为会话键（不同 CC 会话隔离）
- `POST /v1/messages/count_tokens` 提供粗略 token 估算（Claude Code 会调用）
- 开关 `BRIDGE_ANTHROPIC_ENABLED`（默认 true）

### 5.16 最近请求与面板（`GET /v1/requests`、`/ui`）

`GET /v1/requests?since=<ms>&limit=<n>`（bridge API key 鉴权）从官方 console 的
`/request-logs` 拉逐条推理记录，换算成便于展示/对账的结构（`input_tokens` 不含命中；
`prompt = input + cache_read`；命中率 = `cache_read / prompt`）。`/ui` 有一页
「最近请求」，默认 30s 自动刷新。

- **短缓存**：同一 `limit` 在 **5s** 内复用上次结果，避免 UI 自动刷新/重复调用每次
  都打上游（该接口可能被限速）。窗口参数变化很快也命中（只按 `limit` 缓存）。
- **取消不算错**：调用方（浏览器）断开导致的 `context.Canceled` 降为 Debug，不再刷
  WARN；真超时是 `context deadline exceeded`（handler 超时 20s）。
- 开关 `BRIDGE_USAGE_ENABLED`。

### 5.17 会话重置时清理 MCP 注册

`resetSession`（历史不匹配等触发）除了丢弃上游 session，还会**立即 `RemoveMCP`** 旧的
工具桥 server。原因：OpenCode 会把**同一 location 下注册的所有 MCP server 暴露给每个
session**，残留的旧 server 会被 agent 调用并得到
`bridge: this tool server is stale (session was reset); no tools available`。
以前只靠 janitor 周期兜底，中间有空窗；现在重置即删，janitor 仍兜底。

### 5.18 运行时默认模型与虚拟模型 `janus`

`/v1/models` 暴露两个虚拟模型，客户端可直接选：

| 虚拟模型 | 解析 |
|---|---|
| **`janus`** | 面板里选定的默认模型 → 回落 `BRIDGE_DEFAULT_MODEL` → 上游默认 |
| `default` | 上游 / `BRIDGE_DEFAULT_MODEL` 默认（旧语义） |

- **存储**：运行时选择存在 SQLite 的 `settings` 表（key=`default_model`，值 `provider/id[:variant]`）；`Server` 内存里也持一份（`runtimeDefaultModel`）。
- **接口**：`GET /v1/settings` 读（附带只读的 `configured_default_model`）；`POST /v1/settings {"default_model":"…"}` 写（空=清除；写前用 `ResolveModel` 校验存在）。
- **解析**：`resolveModel` 里 `janus` → `resolveJanusModel`（运行时 → 配置 → 上游）；`default`/`auto`/空 → `resolveDefaultModel`（配置 → 上游）。真实 `provider/id` 走透传。
- **档位**：面板选的思考档位拼进 `default_model` 的 `:variant` 后缀（`ResolveModel` 已支持），随默认模型一起生效——让无法传 `reasoning_effort` 的客户端也能固定档位。
- **切换时机**：`ensureSession` 每次请求比较解析出的 `ref` 与 `conv.model`，不同即 `POST /api/session/{id}/model` **原地切换**（不重开会话、上下文保留）。所以面板改完，**下一条请求**即生效，无需新会话。
- **面板**：`/ui`「模型」页每行「设为 janus」+「思考档位」按钮组（点选即设、高亮当前）；当前默认行不显示「设为 janus」。

## 6. 配置

配置来源优先级：**真实环境变量 > 配置文件 > 内置默认值**。
这样既能用文件固化常用配置，又能用环境变量临时覆盖（如 `BRIDGE_LOG_LEVEL=debug ./janus`）。

配置文件是简单的 `KEY=VALUE` 文本，模板见 `janus.env.example`：

```bash
cp janus.env.example janus.env   # run.sh 会自动做这一步
$EDITOR janus.env
./scripts/run.sh
```

查找顺序：`BRIDGE_CONFIG` 显式指定 → 可执行文件同目录 `janus.env` →
当前目录 `janus.env`。文件语法错误会让桥启动失败（不静默用错配置）。

解析规则：`#` 开头是注释；支持 `export KEY=VALUE`；值两端成对引号会被去掉；
`KEY=value  # 尾注释` 支持（仅未加引号时）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_CONFIG` | 空 | 配置文件路径 |
| `BRIDGE_ADDR` | `0.0.0.0:2810` | 监听地址 |
| `OPENCODE_URL` | `auto` | 上游地址；`auto` = 自动发现（§5.3.1） |
| `OPENCODE_AUTOSTART` | `true` | `auto` 且没找到在跑的 OpenCode 时，由本桥拉起一个 |
| `OPENCODE_REUSE_EXTERNAL` | `true` | 是否复用已在跑的外部 OpenCode；`false`=总是自己拉起（才能注入生成的 agent 配置，§5.7.2） |
| `OPENCODE_BIN` | 空 | 显式指定 `opencode` 可执行文件 |
| `OPENCODE_USERNAME` | `opencode` | Basic 用户名（固定） |
| `OPENCODE_PASSWORD` | —（auto 模式下自动获取） | 即 `OPENCODE_SERVER_PASSWORD` |
| `BRIDGE_API_KEY` | 空 | 对客户端的鉴权；空=接受任意 Bearer（WARN） |
| `BRIDGE_DIRECTORY` | `/path/to/project` | 会话默认工作目录 |
| `BRIDGE_AGENT` | `build` | 默认 agent |
| `BRIDGE_SESSION_TTL` | `30m` | 会话空闲回收时间 |
| `BRIDGE_REQUEST_TIMEOUT` | `600s` | 单次补全超时 |
| `BRIDGE_RECONCILE_INTERVAL` | `3s` | 事件流对账间隔 |
| `BRIDGE_IDLE_POLL_INTERVAL` | `1s` | 空闲轮询间隔（终态主判据） |
| `BRIDGE_PROMPT_GRACE` | `2s` | prompt 后多久才信任 idle 信号 |
| `BRIDGE_STREAM_HEARTBEAT` | `15s` | SSE 心跳间隔 |
| `BRIDGE_TOOL_ANNOTATIONS` | `true` | 是否注入 `<opencode-tool>` 注释 |
| `BRIDGE_PERMISSION_REPLY` | `once` | 自动应答 OpenCode 权限请求：`once|always|reject|off` |
| `BRIDGE_MAX_CONVERSATIONS` | `256` | 全局会话上限 |
| `BRIDGE_CORS_ORIGIN` | 空 | 允许的浏览器来源（空=回显） |
| `BRIDGE_LOG_LEVEL` | `info` | `debug|info|warn|error` |

配置文件 `bridge.json` **未实现**（当前全部走环境变量，避免两套来源歧义）。

---

## 7. 分阶段计划

| 阶段 | 内容 | 状态 |
|---|---|---|
| **Phase 1 — MVP** | `/v1/models`、`/v1/chat/completions`（流式+非流式）、会话分桶与历史重放、EventBus、错误映射、API Key | ✅ 完成 |
| **Phase 2 — 硬化** | 四级终态对账、超时与 interrupt、会话 TTL 与 orphans 回收、并发复核、上游自动发现与热切换、图片附件、优雅停机、CORS | ✅ 完成 |
| **Phase 3 — 工具调用** | 内置 MCP server 暴露客户端 tools、挂起/回填闭环、并行调用 | ✅ 完成（2026-10-04 端到端实测通过） |
| **Phase 4 — Responses API** | `POST/GET/DELETE /v1/responses`、`previous_response_id`、流式事件、工具调用 | ✅ 完成 |
| **Phase 5 — 增强** | `/v1/completions` 降级（已做）、速率限制、指标 `/metrics`、`max_tokens` 尽力映射 | ⬜ 未开始 |

---

## 8. 测试计划

**单元**（`core_test.go`，共 60+ 用例）：
历史差分（追加/一致/改写/截断/system 变化）、分桶前缀匹配、并发首请求隔离、
模型名解析（限定名/variant/裸名/歧义/未知 provider）、finish_reason 与 usage 映射、
附件提取（data URI/http/非法 base64/超限/上限）、上游发现参数解析、content 多形态解析。

**竞态**：`CGO_ENABLED=1 go test -race ./...` 全绿。

**端到端**（`compat_test.py`，OpenAI 官方 SDK）：22 项断言见 `COMPAT.md`，全部通过。

**已验证的真实场景**：
1. `/v1/models` 返回 68 个模型，`id` 形如 `opencode/xxx` ✅
2. 流式：role chunk → reasoning → content → finish_reason → usage → `[DONE]` ✅
3. 非流式：`object:"chat.completion"`，content 非空 ✅
4. 多轮：同一 key 连续两次，第二次只发增量且能引用前文 ✅
5. 不相关话题（无 system，同指纹 key）：各占独立会话，互不踢 ✅
6. 并发：三个会话同时流式，内容不串 ✅
7. agent 执行工具：注释注入 + 对账不 panic ✅
8. 图片：data URI 送达模型 ✅
9. 上游重启换端口换密码：自动发现并热切换 ✅
10. 超大工具输出导致的对账分叉：补尾 + WARN，不崩 ✅

**待做**：Trae 与 CodeBuddy 各配一次真实客户端，记录其实际调用的字段（决定 Phase 4 优先级）。

---

## 9. 风险与已知限制

| 风险 | 影响 | 缓解 |
|---|---|---|
| `/api/event` 全局广播、无过滤 | 高并发下 CPU/带宽放大 | 单连接 fan-out；必要时评估轮询降级 |
| 事件流断连丢事件 | 流式输出缺尾 | §5.5 四级对账 |
| OpenCode 不支持采样参数 | `temperature`/`max_tokens` 被忽略 | 文档明示（COMPAT.md §3） |
| Phase 1 不支持客户端 `tools` | Trae 的 agent 模式功能受限 | 尽早做 Phase 3 spike |
| `prompt` busy 时静默排队 | 可能出现"发了没反应" | bridge 侧强制串行 + 超时 interrupt |
| bridge 监听 0.0.0.0 + 弱 Key | 额度被盗用 | 必须设 `BRIDGE_API_KEY`，或收敛到 127.0.0.1 |
| OpenCode server 自身也监听 0.0.0.0 | Basic 密码即全权令牌 | 建议同步收敛 |
| 免费模型在无状态接口被拒 | `generate` 模式部分模型不可用 | 主路径走 agent 会话 |
| **上游端口/密码频繁变化** | 桥静默失效 | §5.3.1 自动发现 + 热切换（仅 Linux） |
| 会话映射在内存 | bridge 重启丢上下文 | 文档明示；客户端会重新积累（回答仍正确） |
| `prompt_tokens` 基线约 5800 | 客户端按 token 计费时偏高 | agent 系统提示开销，非 bug；可换更小 agent |
| 图片只认 data URI | 客户端直传 https 会失败 | 桥侧下载转码；失败时文字提示 |

---

## 10. 附录：最小可用验证脚本

```bash
# 0. 启动（自动发现上游，无需端口/密码）
BRIDGE_API_KEY=sk-bridge BRIDGE_DIRECTORY=/path/to/project ./scripts/run.sh

# 1. 模型列表
curl -s http://127.0.0.1:2810/v1/models -H "Authorization: Bearer sk-bridge" | head -c 300

# 2. 流式对话
curl -N http://127.0.0.1:2810/v1/chat/completions \
  -H "Authorization: Bearer sk-bridge" -H "Content-Type: application/json" \
  -d '{"model":"opencode/fledge-alpha-free","stream":true,
       "messages":[{"role":"user","content":"用一句话介绍你自己"}]}'

# 3. 多轮（同 X-Session-ID）
curl -N http://127.0.0.1:2810/v1/chat/completions \
  -H "Authorization: Bearer sk-bridge" -H "X-Session-ID: demo-1" \
  -H "Content-Type: application/json" \
  -d '{"model":"opencode/fledge-alpha-free","stream":true,
       "messages":[{"role":"user","content":"我叫小明"},{"role":"assistant","content":"你好小明"},
                   {"role":"user","content":"我叫什么名字？"}]}'
```
