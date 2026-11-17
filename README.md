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
work_submit work_verify work_verdict work_complete
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

## 目录

见 [docs/STRUCTURE.md](docs/STRUCTURE.md)。
