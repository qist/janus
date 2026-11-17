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
| POST | `/api/v1/work-items/{id}/claim` | claim | `{"role","plan","lease_seconds"}` → 租约 + 有界上下文 |
| POST | `/api/v1/work-items/{id}/heartbeat` | claim | `{"epoch","extend_seconds"}` |
| POST | `/api/v1/work-items/{id}/release` | claim | `{"epoch"}` |
| POST | `/api/v1/work-items/{id}/submit` | submit | `{"epoch","idempotency_key","evidence"}` |
| POST | `/api/v1/work-items/{id}/verify` | verify | `{"epoch"}` → verifier 复跑 checks |
| POST | `/api/v1/work-items/{id}/verdict` | verify | `{"epoch","verdict","reasons","checks"}` |
| POST | `/api/v1/work-items/{id}/complete` | integrate | `{"epoch","idempotency_key"}` |
| POST | `/api/v1/work-items/{id}/authorize` | authorize | 敏感操作授权，allow 时返回单次能力令牌 |
| GET | `/api/v1/usage` | read | 用量汇总（按 key/group/工作项/provider/model/账号/时间过滤） |

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

## MCP

同一个进程同时提供 MCP：`POST /mcp/{token}`，或 `POST /mcp` 加 `X-API-Key`。
JSON-RPC 2.0，支持 `initialize`、`ping`、`tools/list`、`tools/call`；
`notifications/*` 返回 202 无 body。

工具与 HTTP 端点一一对应，规则、scope、错误码完全一致：

```
work_list  work_get  work_handoff  work_create
work_claim work_heartbeat work_release
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

## 目录

见 [docs/STRUCTURE.md](docs/STRUCTURE.md)。
