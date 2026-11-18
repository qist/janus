# Janus

Janus 是面向多 Agent 交付的控制面：模型网关 + Agent Runtime + 自认领工作板 + 审计授权。

**状态：v2 重写中。** 旧 v0.3.x 的 OpenCode 兼容桥已归档到 tag `legacy-v0.3.56`，不进入新代码。

## 文档

- [设计基线](docs/REFACTOR.md)
- [目录规范](docs/STRUCTURE.md)

## 构建

```bash
make build
./build/janus --version
```

## 运行

```bash
export JANUS_API_KEY='至少 16 字节的开发密钥'
./build/janus -addr 127.0.0.1:8080 -db ./data/janus.db
```

没有 `JANUS_API_KEY` 时进程拒绝启动——服务端不会以匿名方式对外。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `JANUS_API_KEY` | 无（必填） | 开发用 API key，绑定到 `JANUS_AGENT_ID` |
| `JANUS_AGENT_ID` | `admin` | 该 key 对应的 agent 身份 |
| `JANUS_ADDR` | `127.0.0.1:8080` | 监听地址 |
| `JANUS_DB` | `janus.db` | SQLite 路径 |
| `JANUS_WORKSPACE` | 空 | 若设置，`/verify` 在该目录跑契约 checks |
| `JANUS_AUDITORS` | 空 | 审计模型 JSON 数组；不设则敏感操作一律 deny |
| `JANUS_MODELS` | 空 | `/v1` 代理暴露的模型/别名 JSON 数组 |
| `JANUS_CAPABILITY_KEY` | 空 | 能力令牌签名密钥（≥32 字节） |

## HTTP API

所有请求带 `X-API-Key`（或 `Authorization: Bearer <key>`）。响应统一 `application/json`，错误体为
`{"error":{"code":"...","message":"..."}}`。

| 方法 | 路径 | Scope | 说明 |
|---|---|---|---|
| GET | `/api/v1/healthz` | 无 | 存活检查 |
| GET | `/api/v1/work-items?status=&workspace=` | read | 列表 |
| POST | `/api/v1/work-items` | write | 建项，body `{"item":{...}}` |
| GET | `/api/v1/work-items/{id}` | read | 项 + 租约 + 胶囊 + 评审 |
| GET | `/api/v1/work-items/{id}/handoff` | read | 只读取交接包，不拿租约 |
| POST | `/api/v1/work-items/{id}/claim` | claim | `{"role","plan","lease_seconds","model_id","model_version","tool_mode","task_class"}` → 租约 + 有界上下文 |
| POST | `/api/v1/work-items/{id}/assign` | admin | 强制派工：指定 agent，绕过等级/依赖/路径锁，留审计 |
| POST | `/api/v1/work-items/{id}/heartbeat` | claim | `{"epoch","extend_seconds"}` |
| POST | `/api/v1/work-items/{id}/release` | claim | `{"epoch"}` |
| POST | `/api/v1/work-items/{id}/submit` | submit | `{"epoch","idempotency_key","evidence"}` |
| POST | `/api/v1/work-items/{id}/verify` | verify | `{"epoch"}` → verifier 复跑 checks |
| POST | `/api/v1/work-items/{id}/verdict` | verify | `{"epoch","verdict","reasons","checks","failure_owner"}`；reject 必须归因 |
| POST | `/api/v1/work-items/{id}/complete` | integrate | `{"epoch","idempotency_key"}` |
| POST | `/api/v1/work-items/{id}/authorize` | authorize | 敏感操作授权，allow 时返回单次能力令牌 |
| POST | `/api/v1/work-items/{id}/cancel` | write | 取消工作项并释放活跃租约（幂等） |
| GET | `/api/v1/usage` | read | 用量汇总（按 key/group/工作项/provider/model/账号/时间过滤） |
| GET | `/api/v1/scores` | read | 模型等级与分数（只读） |
| POST | `/api/v1/goals` | write | 建目标，异步拆项，返回 `202 + goal_id` |
| GET | `/api/v1/goals` | read | 目标列表（`limit` 1–200） |
| GET | `/api/v1/goals/{id}` | read | `goal + progress`（total/done/blocked/percent） |
| GET | `/api/v1/goals/{id}/events` | read | SSE 进度流 |
| POST | `/api/v1/goals/{id}/cancel` | write | 取消目标（幂等） |

约定：

- 调用者身份只来自 API key，请求体不接受 `agent_id`，未知字段直接 400。
- `claim` 返回 `claim` + `pack`（≤2000 token 的交接包）；`pack.text` 是可直接塞进 prompt 的纯文本。
- 拿不到租约 `epoch` 的调用返回 409（`stale_epoch` / `lease_expired`），不是 403。
- 403 只用于：缺少 scope、非租约持有者（`not_lease_holder`）、自审（`self_review`）。

示例：

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/work-items \
  -H "X-API-Key: $JANUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"item":{"id":"w1","workspace_id":"ws","title":"fix claim race",
       "contract":{"goal":"one claim wins","paths":["internal/work"]}}}'

curl -s -X POST http://127.0.0.1:8080/api/v1/work-items/w1/claim \
  -H "X-API-Key: $JANUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"role":"worker","plan":"implement","lease_seconds":1200}'
```

## 控制台 /ui

React 19 + TypeScript + Vite + Tailwind 的 SPA（hash 路由），构建产物 embed 在二进制里；
`GET /` 302 到 `/ui/`。架构与取舍见 [前端架构](docs/FRONTEND.md)。

- 概览、目标（创建 / 进度 / 取消）、工作项（筛选 / 租约 / 证据输出 / 评审 / 强制派工 / 取消）、
  模型、用量、评分；
- 登录即输入 API key，存本机 `localStorage`，服务端无会话；401 自动回登录；
- 页面静态资源公开，数据全部经 `/api/v1` 且带 key。

```bash
make ui        # 重新构建并更新 internal/ui/dist（dist 随仓库提交，go build 不需要 Node）
make ui-dev    # 本地 Vite 开发，/api 与 /v1 代理到 127.0.0.1:8080
```

模型页目前只读：模型与别名来自 `JANUS_MODELS`。provider / 凭据的在线管理（含 OAuth 登录）
需要后端 CRUD 与加密存储，方案见 `docs/FRONTEND.md` §7。

## 目标与进度

`POST /api/v1/goals` 接一段意图，拆成工作项后立即返回 `202`：

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/goals \
  -H "X-API-Key: $JANUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"title":"fix claim race","brief":"two agents claim at once","workspace_id":"ws"}'
# 202 {"goal_id":"goal_2bcc...","status":"planning"}
```

- 规划在后台进行，`GET /goals/{id}` 随时给出 `goal + progress`（`total / done / blocked / percent`）。
- 默认 planner 是 `SinglePlanner`：一个 goal → 一个工作项。这里不假装聪明——真正的拆项需要模型调用，等 planner 接入网关后再替换。
- 全部工作项 `done` → goal 自动 `done`；出现 `blocked` 项 → goal `blocked`；规划出错 → `failed` 并保留原因。
- SSE：`GET /goals/{id}/events` 每 2 秒推一条 `event: progress`，终态自动结束，最长 10 分钟；客户端断开即停。
- `POST /goals/{id}/cancel` 只停 goal，**不动已经在板上的工作项**——它们有自己的租约和评审记录。

状态：`planning / active / blocked / done / cancelled / failed`。

## MCP

同一个进程同时提供 MCP：`POST /mcp/{token}`，或 `POST /mcp` 加 `X-API-Key`。
JSON-RPC 2.0，支持 `initialize`、`ping`、`tools/list`、`tools/call`；
`notifications/*` 返回 202 无 body。

工具与 HTTP 端点一一对应，规则、scope、错误码完全一致：

```
work_list  work_get  work_handoff  work_create
work_claim work_assign work_heartbeat work_release
work_submit work_verify work_verdict work_complete work_authorize
```

`tools/call` 的失败是**结果**不是协议错误：返回
`{"content":[{"type":"text","text":"{\"error\":{\"code\",\"message\"}}" }],"isError":true}`，
模型能看到原因并改正参数；只有鉴权、方法不存在、参数无法解析才走 JSON-RPC error。

鉴权对所有方法生效，没有 key 连 `initialize` 也不通。注意令牌在路径里会进入访问日志，
生产更推荐 `X-API-Key`。

```bash
curl -s -X POST "http://127.0.0.1:8080/mcp/$JANUS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"work_list","arguments":{}}}'
```

## /v1 模型代理

对客户端暴露 OpenAI 线格式，复用同一批 key、账号池和用量账本：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models` | 列出暴露的模型与别名 |
| POST | `/v1/chat/completions` | 非流式 / `stream:true` 流式透传 |

鉴权用 `Authorization: Bearer <API key>`，需要 `chat` scope。上游 key 由 janus 注入，客户端看不到。

模型与别名由 `JANUS_MODELS` 配置，每项就是一个 OpenAI 兼容端点：

```bash
export JANUS_MODELS='[
  {"name":"openai","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini",
   "aliases":["fast"],"api_key_envs":["OPENAI_KEY_1","OPENAI_KEY_2"]}
]'
```

- 请求里的 `model` 可以是别名；转发时改写成真实模型名，其余字段原样透传（tools、temperature、stream_options 都不动）。
- 流式是**边收边发**的 SSE 直通，不缓冲整段响应；`stream_options.include_usage` 的 usage 会被解析出来记账。
- 账号相关失败自动换 key（同上一节），全部耗尽返回 `503 all_accounts_exhausted` 并带 `Retry-After`。
- 上游错误 → `502 upstream_error`，错误体保持 OpenAI 形状：
  `{"error":{"message","type":"janus_error","code"}}`。
- 每次成功调用写一条用量账，可通过 `/api/v1/usage?key=...` 对账。

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $JANUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}'
```

### 强制派工

`work_assign`（admin only）把卡住的项直接交给指定 agent：

- **绕过**：模型等级、attempts 上限、`depends_on`、路径锁——这些本来是防止自动认领出错的，人工强制就是那个兜底；
- **仍然拒绝**：项上已有活跃租约（不能出现两个持有者）、`done` / `cancelled`；
- 租约 `forced=true`，事件日志记 `assigned` 且 actor 是强制派工的 admin；
- agent 可以照常 `work_release` 交回（`forced` 租约不锁死）。

```bash
curl -s -X POST http://127.0.0.1:8080/api/v1/work-items/w1/assign \
  -H "X-API-Key: $JANUS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"agent_id":"agent-forced","role":"worker","plan":"resolve the blocker","model_id":"m1"}'
```

`blocked` 项被派工后回到 `running`，与普通认领返回同样的 `claim + pack`。

## 敏感操作授权

`work_authorize` / `POST .../authorize` 是「模型不得口头批准」的落点：

```
action → policy（确定性分类 + 路径/命令规则）
       → audit（0/1/2/3 个独立审计器，critical 为 2-of-3）
       → allow: 60s 单次能力令牌
       → deny / needs_human: 不签令牌
```

- 分类按 `REFACTOR §13.3`：`fs.write`、`exec`、`net.egress`、`secret.read`、`secret.write`、
  `git.push`、`deploy`、`db.migrate`、`auth`。
- 硬拒绝不经模型：`rm -rf /`、`--no-preserve-root`、`mkfs`、写裸设备、fork bomb、
  以及任何逃出 workspace 的路径（绝对路径、`..`、`~`）。
- 契约路径外的写入自动升级为 `fs.write`；碰 `.env`/私钥升为 `secret.*`；
  改 `internal/auth` 升为 `auth`。
- 审计失败、超时、非法输出、置信度低于 0.6、审计器数量不足 → **一律 deny**，不降级放行。
- 审计器与 worker 同模型时直接拒绝（`auditor.not_independent`），不给「自己批自己」留缝。
- 令牌绑定 `work_item_id + lease_epoch + action_hash + paths`，60 秒有效、用一次即失效；
  执行器用 `auth.Authority.Verify` 校验，重放返回 `ErrReplay`。
- 决策按 `action_hash + policy_version + contract_version + evidence` 缓存；缓存命中仍会现签新令牌。

审计模型通过 `JANUS_AUDITORS` 配置，每个元素是一个 OpenAI 兼容端点：

```bash
export JANUS_AUDITORS='[
  {"name":"openai","base_url":"https://api.openai.com/v1","api_key_env":"OPENAI_API_KEY","model":"gpt-4o-mini"},
  {"name":"deepseek","base_url":"https://api.deepseek.com/v1","api_key_env":"DEEPSEEK_API_KEY","model":"deepseek-chat"}
]'
```

- 配置几个就并发/顺序跑几个；`high` 需要 2 个、`critical` 需要 3 个，配置不足直接 deny。
- 只支持 `POST {base_url}/chat/completions`（OpenAI、DeepSeek、Moonshot、vLLM、Ollama 都兼容）。
- `api_key_env` 比 `api_key` 安全：密钥留在环境里，不进配置文件。
- 不配置 = **敏感操作全部 deny**，这是刻意的 fail-closed，不是 bug。普通读写与单测不经过审计，直接 allow，也不签令牌。

令牌签名密钥：

| 变量 | 说明 |
|---|---|
| `JANUS_CAPABILITY_KEY` | ≥32 字节；生产必须设置，多实例要一致 |
| 未设置时 | 由 `JANUS_API_KEY` 派生的进程内密钥，重启后旧令牌失效 |

### 账号池

同一个审计模型可以挂多把 key：`JANUS_AUDITORS` 的条目加 `api_key_envs`。

```json
{"name":"openai","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini",
 "api_key_envs":["OPENAI_KEY_1","OPENAI_KEY_2"]}
```

- 只在**账号相关**的失败时换下一把：`429 insufficient_quota` / `402`（额度）、`429`（限流）、
  `401/403`（失效）。`5xx` 和 `400` 不换账号——那不是账号的问题。
- 换 key 不换模型，单次请求最多试 3 把，避免热循环。
- 冷却与状态：限流默认 60s（尊重 `Retry-After`），`401/403` 直接停用，额度耗尽等到 `ResetsAt`；
  重置时间未知时保持不可用，直到人工/成功调用恢复。
- **不对健康账号做轮询**：切账号会丢 prompt cache，损失大于均衡收益，这是有意的。
- 账号事件 append-only：`selected / exhausted / rate_limited / invalid / probe_failed / reset`，供对账。
- key 只在内存里，`Account.APIKey` 带 `json:"-"`，不会被序列化进日志或配置。

### 用量

每次**成功**调用写一条账：provider / credential / model / prompt / completion tokens；
失败调用不记账。

```bash
curl -s "http://127.0.0.1:8080/api/v1/usage?provider=openai&since=1794900000" \
  -H "X-API-Key: $JANUS_API_KEY"
# {"entries":1,"prompt_tokens":12,"completion_tokens":6,"cached_tokens":0,"cost_usd":0}
```

`cost_usd` 由调用方在知道价格表时填入；账本本身只保证 append-only 与可聚合。

## 打分与准入

认领 Worker 时必须带 `model_id`（以及可选的 `model_version / tool_mode / task_class`），租约把这份身份存进
`work_claims`，评审和合并都记到**干活的模型**头上，而不是记到当时的 verifier / integrator。

| tier | 能认领 |
|---|---|
| `probation`（新 key 默认） | 仅 low 风险、预算 ≤ 50%、非高优、非敏感 |
| `active` | 普通项；不能接敏感 / critical |
| `trusted` | 全部，含 auth / 支付 / 迁移 |
| `suspended` / `retired` | 不可自认领 |

- 分数 = `0.50×Wilson下界 + 0.15×一次通过 + 0.15×可靠性 + 0.12×成本 + 0.08×周期`，零样本为 0。
- 样本 ≥ 10 时，Wilson < 0.80 / 一次通过 < 0.60 / 可靠性 < 0.85 → 直接 `probation`。
- 硬闸门：伪造证据、安全或数据事故 → 立即 `suspended`；连续 3 次 model 归因 reject → `suspended`；
  连续 2 次 → `probation`；租约过期率 > 20%（≥10 次认领）→ `probation`。
- **reject 必须带 `failure_owner`**（`model|harness|client|task|account|infra`）。只有 `model` 进模型分，
  其余只留事件——否则一次网络抖动就会冤枉模型。
- 等级变化全部写 append-only 事件，带分数增量；admin 可 `pin / unpin / reset / reinstate / retire`。

```bash
# 未带 model_id
{"error":{"code":"model_required","message":"model_id is required when scoring is enabled"}}

# 新模型接中风险项
{"error":{"code":"tier_denied","message":"model tier does not allow this claim: probation may only claim low-risk items"}}
```

`GET /api/v1/scores` 返回当前所有 `(agent, model, task_class, tool_mode, model_version)` 的分数与等级。

## 目录

见 [docs/STRUCTURE.md](docs/STRUCTURE.md)。
