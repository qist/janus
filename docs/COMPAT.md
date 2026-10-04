# 兼容性实测记录

本文件记录 Janus 对 OpenAI 协议的实现程度，以及对接上游 OpenCode 时踩到的坑。
结论均来自真实请求，不是推测。

环境：OpenCode server v2.0.22、OpenAI Python SDK **3.24.0**、bridge 版本见 git 历史。

> ⚠️ **时效提醒**：本文件早期用 `opencode/fledge-alpha-free` 等 `*-free` 模型做验证。
> OpenCode 后来把免费档限定为"仅官方客户端可用"，经 API/桥调用会 **403
> `free tier can only be used from within OpenCode`**（Janus 映射为
> `403 free_tier_restricted`）。现在请用 `opencode-go/*` 订阅模型，或以带
> `provider/` 前缀的自有 provider 模型复现。

## 1. 端到端兼容矩阵

`compat_test.py`，全部用 **OpenAI 官方 SDK** 发请求：

```
BRIDGE_API_KEY=sk-bridge-dev .venv/bin/python compat_test.py

===== 22 passed, 0 failed =====
```

| 组 | 断言 | 结果 |
|---|---|---|
| `/v1/models` | `models.list()` 返回 `object=list` 且非空（68 个真实模型 + 1 个虚拟 `default`） | ✅ |
| | 首条为虚拟模型 `default`，解析为上游当前默认 | ✅ |
| | id 形如 `provider/model` | ✅ |
| | `models.retrieve(id)` 可用（含 `default`） | ✅ |
| 非流式 | `choices[0].message.role == "assistant"` | ✅ |
| | `content` 为字符串 | ✅ |
| | `finish_reason` ∈ {stop,length,tool_calls,content_filter} | ✅ |
| | `usage.total_tokens > 0` 且 `prompt + completion == total` | ✅ |
| | `id` 以 `chatcmpl-` 开头、`object=chat.completion`、`model` 回显 | ✅ |
| 流式 | 首个 chunk 带 `delta.role=assistant` | ✅ |
| | `delta.content` 可拼接出完整答案 | ✅ |
| | 恰好一个 chunk 带 `finish_reason` | ✅ |
| | `stream_options.include_usage` 产出 usage chunk | ✅ |
| 多轮 | 第 2 轮能记起第 1 轮给的信息（同会话） | ✅ |
| 推理 | `reasoning_content` 字段存在且不污染 `content` | ✅ |
| 错误 | 未知模型 → 404 `NotFoundError` | ✅ |
| | 空 `messages` → 400 `BadRequestError` | ✅ |
| | 错误 API key → 401 `AuthenticationError` | ✅ |
| 图片 | `content` 数组 + `image_url`（data URI）能送达模型 | ✅ |
| 并发 | 三个会话同时流式，内容不串 | ✅ |
| 思考强度 | `reasoning_effort` = low/max/medium/minimal 均被上游接受 | ✅ |
| | 未知档位名不报错（桥忽略） | ✅ |
| 模型扩展 | 每个模型都有 `context_length` / `max_output_tokens` | ✅ |
| | 有 `supported_reasoning_efforts`（可用档位列表） | ✅ |

### SDK 实际解析到的字段

SDK 3.24.0 会读取并按类型校验这些字段，全部通过：

- 非流式：`id` / `object` / `created` / `model` / `choices[].index` /
  `choices[].message.{role,content}` / `choices[].finish_reason` /
  `usage.{prompt_tokens,completion_tokens,total_tokens}` /
  `usage.prompt_tokens_details.cached_tokens` /
  `usage.completion_tokens_details.reasoning_tokens`
- 流式：`choices[].delta.{role,content,reasoning_content}` /
  `choices[].finish_reason`（仅在末 chunk 非 null） /
  `usage`（仅 `include_usage` 时的独立 chunk）

`reasoning_content` 不是 OpenAI 官方字段，SDK 会以未知字段保留，可通过
`message.reasoning_content` 读到（DeepSeek 系客户端的常见约定）。

## 2. 上游行为实测（踩坑记录）

| 现象 | 实测结果 | 桥的应对 |
|---|---|---|
| `GET /api/model` 不带 `directory` | 返回**空数组** | 始终带 `directory` |
| 创建会话缺 `location.directory` | 400 `Missing key at ["location"]["directory"]` | 请求体显式带 `location` |
| 匿名 struct 字段没打 json tag | 序列化成 `Directory` 而非 `directory` | 所有内联 struct 都写 tag |
| `/api/experimental/generate` 用于免费模型 | 拒绝：`"OpenCode's free tier can only be used from within OpenCode"` | 主路径改走 agent 会话 |
| `/api/event` | **全站广播**，无 session 过滤参数 | 单连接 + 进程内按 `sessionID` fan-out |
| 会话执行中再发 prompt | 返回 200 但静默排队 | 每个会话内串行化；`delivery: queue` |
| `/api/experimental/session/{id}/wait` | **阻塞**直到 agent loop 空闲，返回 204（实测 4.0s） | 作为终态兜底信号 |
| `session.time.idle` 时间戳 | 执行结束后才更新，可判定"本轮到没到" | 空闲轮询作为主终态判据 |
| 图片附件传 `https://` URL | 400 `Unsupported attachment URI` | 先下载转 data URI 再传 |
| 图片附件传 `data:` URI | 接受，解析成 `source={type:inline}` | data URI 直传 |
| `session.step.ended.tokens` | 每步的 input/output/reasoning/cache.read | 逐段累加为本次请求的 usage |
| `session.usage.updated` | **会话累计值** | 忽略，避免与 step 累加重复计数 |
| `session.tool.input.started` | 带 `name`（如 bash/glob/read） | 工具注释里显示工具名 |
| `Model.Ref.variant` | 就是思考强度档位（`variant.settings.reasoningEffort`） | `reasoning_effort` 映射到它 |
| 可用 variant 取值 | `none/minimal/low/medium/high/xhigh/max/thinking`（因模型而异，10 个模型完全没有 variant） | `/v1/models` 用 `supported_reasoning_efforts` 暴露 |
| `POST /api/session/{id}/model` 切 variant | 204，**原地切换、上下文保留** | 改 `reasoning_effort` 不会重开会话 |
| prompt 接口的输出上限参数 | **不存在**（`Compatibility.maxTokensField` 只是内部配置） | `max_tokens` 接受但不生效 |
| 上游重启 | **端口和密码都换**（如 2809 → 61573） | `discover.go` 扫 `/proc` 自动发现并热切换 |
| 上游偶发空回复 | 观测到一次 91s 后 `content`/`reasoning` 全空、`finish=stop`、`usage=0` | 空回复不入缓存；重复请求退回重跑；并且**明确报错**而不是静默返回空（见下） |
| 免费模型被高频调用 | 连续多次后开始出现 ~90s 空回复；付费的 `opencode-go/*` 无此现象 | 无法从桥侧消除；桥改为返回 `empty_completion` 错误让客户端可重试 |

### 上游默认模型（实测）

`GET /api/model/default` 当前返回（全局一致，**不随 directory 变化** —— 实测
`/path/to/project`、`/root`、`/path/to/other` 结果相同）：

```json
{"providerID":"opencode","id":"fledge-alpha-free","name":"Fledge Alpha Free",
 "package":"@opencode/ai/providers/openai-compatible",
 "capabilities":{"tools":true,"input":["text","image"],"output":["text"]},
 "variants":[{"id":"low"},{"id":"high"},{"id":"max"}]}
```

即 `opencode/fledge-alpha-free`。名字里的 "alpha"/"free" 看着像占位符，
但它是**真实的上游默认**，实测可正常对话、可收图片、支持工具。

尽管它是真的，**客户端仍建议填 `default`**：默认值随时可能被上游改掉，
写死就得跟着改配置。桥把 `default` / `auto` / 空字符串都实时解析到上游默认值。

模型总数 68（`opencode` 39 + `opencode-go` 29），`/v1/models` 返回 69 条
（多一条虚拟的 `default`）。

### prompt_tokens 偏大是正常的
简单提问也会看到 `prompt_tokens ≈ 5800`。这是 OpenCode `build` agent 的
**系统提示词 + 工具定义**本身的开销，不是桥的 bug。实测从上游 `/message`
直接读到的 `tokens.input` 与桥上报的一致（5819 vs 5825）。

若客户端按 token 计费/截断，请把这一点考虑进去；换更小的 agent 可降低基线。

## 3. 与 OpenAI 的语义差异

| 项 | OpenAI | 本桥 | 影响 |
|---|---|---|---|
| 会话 | 无状态 | 有状态 agent 会话，按历史前缀自动关联 | 客户端行为不变，但**上下文能跨请求延续** |
| `tools` / `tool_choice` | 客户端驱动工具调用 | **不转发**（Phase 3） | 需要客户端自带工具的 agent 场景暂不支持 |
| agent 自己的工具调用 | — | 以 `<opencode-tool>` 注释插进 `content` | 可用 `BRIDGE_TOOL_ANNOTATIONS=false` 关闭 |
| `reasoning_effort` | 官方值 minimal/low/medium/high | 映射到上游 variant，额外支持 `none`/`xhigh`/`max` | 上游档位因模型而异，见 `/v1/models` |
| `max_tokens` / `max_completion_tokens` | 限制输出长度 | **接受但不生效** | 上游 prompt 接口无此参数，上限由模型决定 |
| `temperature` / `top_p` / `n` / `stop` | 支持 | 静默忽略 | JSON 兼容，不会报错 |
| `logprobs` | 支持 | 不返回 | 省略该字段 |
| `reasoning_content` | 无 | 有（上游推理过程） | 未知字段，多数客户端会忽略 |
| `cached_tokens` | prompt 的子集 | 上游按步上报，可能 > `prompt_tokens` | 仅统计展示用途 |
| 图片 | data URI + http(s) | data URI + 可下载的 http(s) | `file://` 不支持 |
| `/v1/models` 字段 | 4 个标准字段 | 附加 `context_length` / `max_output_tokens` / `supported_reasoning_efforts` / `input_modalities` | 标准客户端忽略 |
| `/v1/usage` | 无此端点 | 扩展端点，返回余额与用量 | 客户端不会主动调，主要给人看 |

### 空回复（empty completion）

上游在压力下会返回**完全空**的回复：`content` 与 `reasoning` 都为空、
`finish=stop`、`usage` 全 0，耗时可达 ~90s。实测免费模型（`opencode/*-free`）
在短时间内被调用较多次后容易触发；付费的 `opencode-go/*` 未见此现象。

桥的处理：

1. **不入缓存** —— 否则后续完全相同的请求命中 `DiffNone`，会原样回放这个空结果，
   看起来像"桥坏了，怎么重试都没用"。
2. **退回重跑** —— `DiffNone` 遇到空缓存时，重发最后一条 user 消息而不是返回空。
3. **明确报错** —— 空回复不再作为 200 成功返回：
   - 非流式：HTTP 502 `{"error":{"message":"upstream returned an empty completion...","code":"empty_completion"}}`
   - 流式：HTTP 200 已发出，改用**流内 error 事件**（`data: {"error":{...}}` 后跟 `[DONE]`）。
   - 耗时超过 30s 时错误信息会带上时长并提示 `likely rate-limited`。

客户端收到 `empty_completion` 应当重试；这通常是上游限流/卡顿。

### 上游失败原因透传（余额不足等）

上游把失败原因写在 assistant 消息的 `error` 字段里。实测（账号余额耗尽）：

```json
{"type":"provider.quota",
 "message":"Upstream request failed: Insufficient account funds",
 "status":402,
 "response":{"body":"{\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream request failed: Insufficient account funds\"}}"}}
```

桥的映射（`mapUpstreamFailure`）：

| 上游 `type` / status | 返回给客户端 |
|---|---|
| `provider.quota` / 402 / message 含 `insufficient` | **429** `type=insufficient_quota` `code=insufficient_quota` |
| `provider.rate_limit` / 429 | 429 `rate_limit_error` / `rate_limit_exceeded` |
| 401 / `auth` | 502 `api_error` / `upstream_auth_error` |
| 其他 | 502 `api_error` / `upstream_error` |

按 OpenAI 惯例，余额不足走 **429 + `insufficient_quota`**（而不是 402）：
SDK 对这个组合有成熟处理，会提示充值而不是无脑重试。

> ⚠️ 修这个时发现的连带 bug：上游以 `outcome=failed` 收尾、但 `execution.failed`
> 事件没被收到时，桥只按 `time.idle` 判断"本轮结束"，会误报 `succeeded`，
> 于是错误被显示成"空回复"。现在会读 session 的 `outcome`，`failed` 就报 failed。

### 余额不足时哪些模型还能用（实测）

账号余额 `$0` 时：

| 模型 | 结果 |
|---|---|
| `opencode/fledge-alpha-free` 等 `*-free` | ❌ 403 `free tier can only be used from within OpenCode`（**已过时**：免费档现仅限官方客户端，经 API 一律 403） |
| `opencode-go/*`（`gpt-6-luna`、`deepseek-v4.1-flash`…） | ✅ 正常（订阅额度，当前推荐） |
| `opencode/claude-sonnet-5-5`、`opencode/gpt-6.1-sol` 等付费档 | ❌ 429 `insufficient_quota` |

所以"用哪个模型"要先看余额与档位：现在实际可用的基本是 `opencode-go/*`。
用 `GET /v1/usage` 看余额，用 `GET /v1/models?provider=opencode-go` 列可用的。

### Responses API（实测）

| 能力 | 结果 |
|---|---|
| `POST /v1/responses` 非流式 | ✅ `resp_…` / `object=response` / `status=completed` / `output_text` |
| `output[]` | ✅ `[reasoning, message]`，`message.content[].type=output_text` |
| `usage` | ✅ `input_tokens` / `output_tokens` / `*_details` |
| 流式 | ✅ `response.created` → `response.reasoning_summary_text.delta` → `response.output_text.delta` → `response.completed` |
| `previous_response_id` | ✅ 第二轮答出上一轮暗号（`RED-7`），上下文跨响应延续 |
| `GET /v1/responses/{id}` | ✅ |
| `DELETE /v1/responses/{id}` | ✅ 返回 `response.deleted`，再 GET 得 404 |
| 未知 `previous_response_id` | ✅ 404 `previous_response_not_found` |
| 空 `input` | ✅ 400 `invalid_input` |

实测非流式响应：

```json
{"id":"resp_666b7a28b436ccffd61a8fb21e1298f9","object":"response","status":"completed",
 "output_text":"ok",
 "output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"…"}]},
           {"type":"message","role":"assistant","status":"completed",
            "content":[{"type":"output_text","text":"ok","annotations":[]}]}],
 "usage":{"input_tokens":43,"output_tokens":3,"total_tokens":46,
          "input_tokens_details":{"cached_tokens":4864},
          "output_tokens_details":{"reasoning_tokens":16}}}
```

## 4. 用量与余额查询（实测）
「剩余用量」分两层，OpenCode 把它们放在了不同地方：

| 层 | 数据 | 位置 | 需要凭据 |
|---|---|---|---|
| 本地 | 已消耗（tokens + cost，按模型） | `GET /api/experimental/session/stats` | 否（server Basic auth） |
| 云端 | **余额 / 可用额度** | `GET https://opencode.ai/console/api/billing/status` | 是（OAuth） |
| 云端 | 汇总/分模型/按天消耗 | `.../usage/summary` `.../usage/models` `.../usage/cost-by-day` | 是 |

### 凭据在哪

桌面端把 OAuth 凭据存在 SQLite：

```
表 credential, integration_id='opencode', active=1
value = {"type":"oauth","access":"st_…","refresh":"rt_…","expires":<ms>,
         "metadata":{"server":"https://opencode.ai/console",
                     "accountID":"user_…","email":"…",
                     "orgID":"org_…","orgName":"Personal"}}
```

`access` 即 Bearer token；请求还要带 `x-opencode-org-id: <orgID>`。

### 实测端点

```bash
TOKEN=$(sqlite3 -readonly ~/.local/share/opencode/opencode.db \
  "select json_extract(value,'\$.access') from credential where integration_id='opencode' and active=1 limit 1;")
ORG=$(sqlite3 -readonly ~/.local/share/opencode/opencode.db \
  "select json_extract(value,'\$.metadata.orgID') from credential where integration_id='opencode' and active=1 limit 1;")

curl -s https://opencode.ai/console/api/billing/status \
  -H "Authorization: Bearer $TOKEN" -H "x-opencode-org-id: $ORG"
# {"billingMode":"prepaid","mode":"pay-as-you-go","balanceMicroCents":"0",
#  "creditLimitMicroCents":null,"availableMicroCents":"0",
#  "canPurchaseCredits":true,"canEnableAutoRecharge":true}
```

实测返回（2026-10-03）：

```jsonc
// billing/status
{"billingMode":"prepaid","mode":"pay-as-you-go","balanceMicroCents":"0",
 "creditLimitMicroCents":null,"availableMicroCents":"0",
 "canPurchaseCredits":true,"canEnableAutoRecharge":true}

// usage/summary
{"totalRequests":"4084","totalInputTokens":"16570229","totalOutputTokens":"3005418",
 "totalCacheReadTokens":"1550468473","totalCacheWrite5mTokens":"75186",
 "totalCostMicroCents":"856750577","services":[]}

// usage/models（节选）
{"items":[{"model":"deepseek-v4.1-flash","provider":"opencode-go",
           "totalRequests":"3729","totalCostMicroCents":"855278353"},
          {"model":"fledge-alpha-free","provider":"opencode",
           "totalRequests":"82","totalCostMicroCents":"0"}]}
```

单位：`*MicroCents` = 1e-8 USD。856750577 → **$8.5675**。

### 关键结论

1. **没有"按模型的剩余额度"**。额度是账号级的（`balance` / `available`）；
   `usage/models` 只告诉你每个模型**已花**多少。想要按模型限额得配 console 的
   budget（`/api/budgets/*`），实测当前账号 `budgets/org` 为 `null`（未设）。
2. **免费模型 cost 恒为 0**：`fledge-alpha-free`、`mimo-v2.6-flash-free` 等
   实测 `totalCostMicroCents: "0"`，走它们不花钱。
3. **付费消耗集中在 `opencode-go/deepseek-v4.1-flash`**（实测 $8.55 / 共 $8.57）。
4. **`balanceMicroCents` 为 0 但付费模型仍能调用** —— 说明扣费不在预付余额里
   （可能走绑定的支付方式或另有额度）。要确认真实扣费口径，建议看 console 的
   Billing 页面；API 层面的 `ledger` 实测为空数组。

### 走不通的路径（记录以避免重复踩）

| 尝试 | 结果 |
|---|---|
| `GET {provider baseURL origin}/v1/credits`（CLI `getCredits()` 的做法） | **404**。openai-compatible provider 的 baseURL 是 `https://opencode.ai/inference/openai/v1`，origin 下没有 `/v1/credits` |
| `GET opencode.ai/api/billing`、`/api/usage` | 404（那是官网 SPA 的 fallback） |
| `GET opencode.ai/console/v1/credits` | 200 但返回 SPA HTML 外壳 |
| `opencode auth list` / `auth export` | 挂起 / 空输出（desktop 模式下凭据在 DB 里） |
| `.../internal/orgs/{orgId}/go/status` | **403 Forbidden**（内部管理接口，用户 token 无权限） |
| `openai.ai/api/budgets/org` | `null`（未设预算） |

> `/console/api/budgets/users/status` 实测返回
> `{"limitMicroCents":null,"spentMicroCents":"0","exceeded":false,"resetsAt":"2026-11-01"}`，
> 即当前用户无预算上限。

## 5. 客户端适配建议

给 Trae / CodeBuddy 这类客户端接 bridge 时：

| 配置项 | 建议值 |
|---|---|
| Base URL | `http://<host>:2810/v1` |
| API Key | `BRIDGE_API_KEY` 的值 |
| Model | **`default`**（推荐）/ `auto` / 留空 —— 均解析为上游当前默认模型；也可填任意 `/v1/models` 里的 `provider/model` |
| 流式 | 开（默认）；关也能用 |
| `stream_options.include_usage` | 开（有 usage chunk） |
| 上下文发送 | **发完整历史**，桥会自己算增量 |
| `X-Session-ID` 请求头 | 可选。多对话客户端建议带，可让会话映射更准 |

### 关于 X-Session-ID

不带也能正常工作（桥用 `hash(system + directory)` 分桶 + 历史前缀匹配，
不相关话题会自动分开）。但若客户端能带一个稳定的对话 ID，映射会更精确、
更省一次会话创建。社交媒体/多标签页客户端建议带上。

### 健康检查

客户端若需要探活，用 `GET /healthz`（无需 API key），返回：

```json
{"status":"ok","upstream":"http://127.0.0.1:61573"}
```

上游不通时返回 503 且 `status=upstream_unreachable`。

## 6. 复现方式

```bash
# 单测（含竞态检测）
CGO_ENABLED=1 go test -race ./...

# 兼容矩阵（需要先起 bridge）
python3 -m venv .venv
.venv/bin/pip install 'openai>=3'
BRIDGE_URL=http://127.0.0.1:2810/v1 \
BRIDGE_API_KEY=sk-bridge-dev \
BRIDGE_MODEL=opencode/fledge-alpha-free \
.venv/bin/python compat_test.py
```

可选环境变量：`BRIDGE_URL`、`BRIDGE_API_KEY`、`BRIDGE_MODEL`。
