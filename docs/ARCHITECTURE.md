# Janus 架构演进：控制平面 + Agent 生态

> 状态：讨论稿（未实现部分均为规划）
> 日期：2026-10-05
> 关系：本文描述 janus 的**目标架构与路线**；当前已实现细节见 [`DESIGN.md`](./DESIGN.md)。

---

## 0. 定位

一句话：

> **OpenCode（及各类 harness）负责「怎么做」，Janus 负责「谁可以怎么用」。**

Janus 是**控制平面**，不是 agent runtime：

| Janus 做 | Janus 不做 |
|---|---|
| 协议网关（OpenAI / Anthropic） | 不实现 agent 循环 |
| 会话网关（映射 / 生命周期） | 不实现工具 runtime |
| Agent 网关（选 harness / 注入配置） | 不实现 MCP runtime |
| 模型访问层（目录 / 网关 / 路由） | 不实现模型推理 |
| 派单 / 调度 / 门禁 | 不实现沙箱（下沉 OS） |
| 权限策略 | — |
| 工具桥（MCP） | — |
| 事件 / 审计 / 计量 | — |

**核心原则：标准件优先，只自定义产品面。**

| 能力 | 标准 |
|---|---|
| Agent 控制 | **ACP**（Agent Client Protocol） |
| 工具 | **MCP** |
| 身份（用户） | **OIDC / OAuth2** |
| 推理协议 | **OpenAI / Anthropic** |
| 交付物 | **git** |
| Janus 自定义 | 派单 / Role / 虚拟模型（唯一的产品面） |

---

## 1. 分层架构

```
                       ┌────────────────── Janus（控制平面）──────────────────┐
用户 / 客户端 ────────►│ 协议网关 │ 会话网关 │ Agent 网关 │ 派单 │ 权限 │ 审计 │
                       │         （OpenAI / Anthropic 标准协议）              │
                       └───────┬──────────────────────────────────┬──────────┘
                    （推理路径）│                                  │（Agent 路径）
                               ▼                                  ▼
                    ┌─────────────────────┐            ┌───────────────────────┐
                    │  模型访问层          │            │  ACP Agent（可插拔）   │
                    │  provider 网关/目录  │            │  OpenCode / Claude Code │
                    │  OpenRouter/LiteLLM  │            │  / Codex / OpenHands …  │
                    │  翻译网关(双协议)    │            └───────────┬───────────┘
                    └──────────┬──────────┘                        │
                               ▼                                   ▼
                          各家模型 / 本地                    工具执行（fs/shell/MCP）
```

**两个关键解耦：**

1. **模型访问 ≠ OpenCode**：模型目录与鉴权由 Janus 自己拥有（当前是从 OpenCode 拿，需改）。
2. **Agent 执行 ≠ OpenCode**：通过 ACP，harness 可替换。

---

## 2. 执行模式（Execution Mode）

三个模式，语义固定，作为**每个 role 的属性**（全局默认 + 每 role 覆盖）：

| 模式 | 语义 | 工具执行位置 | 现状对应 |
|---|---|---|---|
| `native` | harness 用自带工具 | Janus 主机 / 会话目录 | A：`build` + `TOOL_CALLING=false` |
| `remote-tools` | 只用客户端声明的工具，走 MCP 工具桥 | 客户端 | B：`orchestrator` + `TOOL_CALLING=true` |
| `none` | 无工具，纯文本推理 | 无（可不经 harness） | C：`orchestrator` + `TOOL_CALLING=false` |

**mode B 的协议边界（重要）**：MCP 只存在于 **Janus ↔ harness** 之间（Janus 当 MCP server，harness 当 MCP client）；对客户端始终是**标准 `tool_calls` / `tool_use`**。客户端无需懂 MCP。

```
harness（orchestrator，无内置工具）
   │ MCP tools/call
   ▼
Janus（工具桥）
   │ 标准 tool_calls / tool_use
   ▼
客户端执行 → 回填标准 tool 结果 → Janus 回填 MCP → harness 继续
```

---

## 3. Agent 层（harness）

### 3.1 harness 可插拔
- 控制协议：**ACP**（`initialize → authenticate → session/new|load|resume|prompt|update|cancel`）。
- 候选 harness：OpenCode（默认）、Claude Code、Codex、OpenHands、Goose……
- **不重写 agent runtime**；OpenCode 只是「可替换的一个后端」。

### 3.2 harness ↔ model：默认配对 + 兼容矩阵 + 可覆盖
harness 与 model **解耦**，但存在原生配对与协议约束（不是硬绑定）：

| harness | 默认模型 | 兼容协议 | auth |
|---|---|---|---|
| OpenCode | 可配 | 任意 | provider 自定义 |
| Claude Code | `claude-opus-5-5` | Anthropic Messages（含兼容端点） | OAuth(Anthropic) / apikey |
| Codex | `gpt-5.6-sol` | OpenAI Responses / Chat | OAuth(OpenAI) / apikey |
| direct（Janus 自有） | 可配 | 任意 | Janus provider |

Role 只写 `{harness, model}`，启动时做**兼容性校验**；不兼容报错或告警 + `allow_incompatible` 强制覆盖。

### 3.3 harness 用外部模型的方式
harness 自己管模型配置，Janus **注入配置**：

| harness | 方式 |
|---|---|
| OpenCode | 任意 provider（75+，含本地） |
| Claude Code | `ANTHROPIC_BASE_URL` + token → Anthropic 兼容端点 |
| Codex | `~/.codex/config.toml` `model_providers` + `wire_api` |
| 通用 | **Janus 翻译网关**（同时暴露 Anthropic + OpenAI 双协议），所有 harness 指向它 |

### 3.4 客户端执行拓扑
| 拓扑 | harness 在哪 | 工具在哪 | Janus 下发 |
|---|---|---|---|
| 1（现有 mode B） | Janus | 客户端 | **tool_call** |
| 2（IDE 全本地） | 客户端 | 客户端 | **task**（需新增 client worker） |

拓扑 2 需要客户端跑一个 **Janus worker**（连 Janus → 领 id → 收任务 → 跑本地 harness → 回流事件）。任务体 `{task_id, harness, prompt, workspace, mode, permissions, budget}`；target = `{client_id, harness}`。

---

## 4. 模型访问层

**目标：不依赖 OpenCode Console。**

- **provider 抽象**：`{type: openrouter|openai|anthropic|litellm|opencode|local, base_url, auth}`。
- **模型目录**：聚合公开注册表（models.dev）+ 各 provider `/models`，落 DB。
- **翻译网关**：LiteLLM / new-api / one-api / OpenRouter，同时暴露 Anthropic + OpenAI 协议；统一鉴权、限流、计量。
- **用户自带 key**：加密存储（AES-GCM，主密钥来自文件/env）。
- **订阅式 OAuth**（Codex/Claude）为**可选附加**，不作主干（见 §10 硬约束）。

---

## 5. Role 注册表与虚拟模型

一个 role 绑定一整套策略（不只是模型）：

```
role: backend
  harness      acp:opencode
  model        <从模型目录选>
  variant      high
  mode         native
  permissions  { write: [/opt/proj], deny: [/etc, /root] }
  budget       200k tokens / 30m
  workspace    worktree-per-task
```

- 客户端只认虚拟模型 **`janus/<role>`**（provider=janus，model=role）。
- 真实模型名客户端**指定不了**（`BRIDGE_ALLOW_RAW_MODEL` 仅调试）。
- `/v1/models` 暴露虚拟模型列表。
- 未配置的 role → 回落 `BRIDGE_DEFAULT_MODEL`。

**意义**：客户端只选 role，一次选择就定死 **模型 + 执行模式 + 权限 + 预算**。

---

## 6. 权限策略

- **服务端裁决**：复用 harness 的权限事件（如 OpenCode `permission.asked`，`action=external_directory`）+ 会话 workspace，按 `allowed_paths / deny_paths / tools` 判 allow/deny。
- **观测**：只进 Janus 日志 / `/ui`（运维审计），**不推给客户端**（标准协议无此通道）。
- **硬边界**：
  - 目录 / 文件：可管（harness 权限事件 + workspace）。
  - 工具：可管（禁用工具 / 拒绝权限）。
  - **网络 / 命令：管不了**——一旦 shell 放行，只能靠 **OS 级隔离**（独立 user / cgroup / netns / 防火墙）。
- **不做**：客户端权限审批（无标准通道）；不在 Janus 里造沙箱。

---

## 7. 对话与上下文

### 7.1 存储不一致：不翻译存储，只映射会话
- **Janus canonical transcript**：客户端看到的那条对话线，只存 `role / content / tool_calls / 结果摘要`；不存 reasoning、文件快照、harness 内部噪音。
- **harness 原生 session**：各自存储，Janus 只记 `{harness, session_id}`。
- 适配靠 **ACP**（`session/new|load|resume|prompt|update|cancel`）。
- **硬限制**：会话**不能跨 harness 迁移**。默认**一条对话绑一个 harness**；切换 = 新 session + 重放 canonical。

### 7.2 Token 控制
| 手段 | 说明 |
|---|---|
| **增量发送** | harness 会话有状态，只发新增（最大头；现有 `Diff` 即此） |
| **Prompt caching** | 稳定前缀（system + 早期历史）可缓存；保持前缀稳定 |
| **压缩 / 摘要** | 超阈值把旧轮次摘要，保留最近 N 轮原文 |
| **工具输出截断 / 摘要** | 文件 / 命令输出是最大 token 源 |
| **不预加载仓库** | 让 agent 按需 `grep/read` |
| **减少会话重建** | 重建 = 重放 = 烧钱；持久化 session id |
| **成本分层路由** | 简单轮次便宜模型，难轮次强模型 |
| **预算上限** | per-conversation / per-job，超限摘要或停 |
| **子代理只回摘要** | 探索在子代理上下文，主线拿结论 |

**最贵的两个动作：重发全量、重建会话。** 压住这俩，费用就下来。

### 7.3 有状态对话 API（生态优势）
自建 API 可做**有状态对话**：客户端只发**新消息**（非全量），彻底消除「客户端重发历史」的浪费。这是标准 OpenAI 客户端给不了的。

---

## 8. 任务派单（artifact-gated）

### 8.1 依赖是「交付物」，不是「角色顺序」
- 阶段启动充要条件：**输入 artifact 已冻结 + 已批准**。
- 阶段完成定义：**输出 artifact 已产出 + 通过验收**。
- **冻结（freeze）** 是防乱套的根：下游不能回头改上游决定。

### 8.2 角色流水线（默认串行 + 门禁）
| 阶段 | mode | 输入 | 输出 | 门禁 |
|---|---|---|---|---|
| product | none | 产品目标 | PRD / 范围 | 人工 |
| design | none | PRD | 设计文档 + UI 稿 | 人工 |
| architecture | none | 设计 | 接口 / 数据模型 | 人工 |
| backend | native | 接口 | 代码 | 测试 |
| frontend | native / remote-tools | UI 稿 + 接口 | 代码 | 测试 |
| audit | none / 只读 | 冻结代码 | 审计报告 | 自动 + 人工 |
| bugfix | native | 审计报告 | 修复 | 测试 |
| qa | native / 只读 | 修复 | 验收报告 | 人工 |

### 8.3 并发规则
- **只读阶段可并行**（多审计维度、多分析）。
- **写阶段默认串行**；仅 artifact 不相交时并行（backend ∥ frontend，前提接口已冻结），各自 **worktree 隔离**。
- 铁律：**同一 artifact 只有一个 writer。**

### 8.4 对象
```
Goal   产品目标
Stage  阶段：{role, mode, inputs[], outputs[], gate}
Task   派单单元：{task_id, target{client_id,harness}, workspace, budget, status, events}
Gate   门禁：人工 / 自动（测试、审计）
```
- dispatcher = **artifact-gated DAG 状态机**，默认单条关键路径单飞。
- 每个阶段 = 一个 **durable job**；完成 + 门禁通过 → 推进下一个。

---

## 9. 平台层（多用户）

### 9.1 配置入库
- **文件 bootstrap**：监听地址、DB DSN、加密主密钥、初始 admin（启动前必需）。
- **DB**：providers、roles（虚拟模型）、users、api_keys、budgets、permissions。

### 9.2 身份
- **OIDC**（Janus 当 Relying Party；IdP 用 Authentik / Keycloak / Zitadel / Google / GitHub）。
- 程序化客户端：**Janus 签发 API key**（`sk-janus-…`，绑定用户）。
- 两条腿都要：浏览器 OIDC + API key。

### 9.3 隔离
| 方案 | 说明 | 代价 |
|---|---|---|
| 逻辑隔离 | 共享一个 harness，Janus 按 user 打标 | 便宜，隔离弱 |
| **每用户 / 每租户一个 harness 实例** | Janus 管生命周期，按 DB 生成 config/auth | 重，但一步解决 provider 配置 + 鉴权 + 隔离 |

**推荐后者**（扩展现有 autostart 能力）。

### 9.4 provider 登录
- **不要 Janus 自己实现各厂商 OAuth**。
- 走 harness 自身登录（`opencode auth login`）或 **ACP `authenticate`**；Janus 只透出 URL / device code。
- API：`POST /v1/providers/{id}/login` → `{url, user_code}`；`GET .../login/status`。

---

## 10. 可行性与硬约束

| # | 能力 | 可行性 | 依据 / 风险 |
|---|---|---|---|
| 1 | 工具结果注释、终止后重开会话 | ✅ 已完成 | — |
| 2 | Execution Mode 收敛命名 | ✅ 易 | 配置枚举 |
| 3 | Role 注册表 + `janus/<role>` | 🟡 中 | DB + 解析 + `/v1/models` |
| 4 | 权限策略（服务端裁决） | 🟡 中 | 复用权限事件；硬隔离靠 OS |
| 5 | 自建模型网关 + 目录 | 🟡 中 | OpenRouter / LiteLLM |
| 6 | 配置入库 + 密钥加密 | 🟡 中 | 保留文件 bootstrap |
| 7 | OIDC 用户登录 | 🟡 中 | 标准库 |
| 8 | API key 多用户 | 🟡 中 | 标准 |
| 9 | 用户自带 provider key | 🟡 中 | 加密存储 |
| 10 | 每用户 harness 实例隔离 | 🟡 重 | 已有 autostart 基础 |
| 11 | ACP 适配层 | 🟡 中 | ACP v1 稳定但年轻 |
| 12 | 翻译网关（双协议） | 🟡 中 | 现成软件 |
| 13 | canonical + session 映射 | 🟡 中 | 已有雏形 |
| 14 | 省 token（增量/缓存/压缩/截断） | 🟡 中 | 标准手段 |
| 15 | **异步 Job（durable）** | 🟡→🔴 大 | 派单的前置 |
| 16 | 任务派单（artifact-gated DAG） | 🔴 难 | 先串行 + 人工门禁 |
| 17 | **全自动产品开发（无人）** | 🔴 不现实 | 必须留人工检查点 |
| 18 | 会话跨 harness 迁移 | ⛔ 不可行 | harness session 不透明 |
| 19 | 订阅 OAuth 多用户共享 | 🔴 高风险 | 厂商封禁 / 会失效 |
| 20 | Janus 拦网络 / 命令 | ⛔ 不可能 | 靠 OS |
| 21 | 客户端权限审批（标准协议） | ⛔ 无通道 | OpenAI/Anthropic 无此通道 |
| 22 | 重写 agent runtime | ⛔ 不做 | 违背定位 |

**三条硬约束别硬碰**：会话不能跨 harness（#18）、网络/命令隔离靠 OS（#20）、订阅 OAuth 别做主干（#19）。

---

## 11. 路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| **P0** | Role 注册表 + `janus/<role>` 虚拟模型 + API key；配置「文件 + DB 覆盖」 | 让客户端只选 role；模型可集中配置 |
| **P1** | 自建模型网关 + 目录（摆脱 Console）；权限策略（服务端裁决 + 审计） | 模型访问层独立；权限可管 |
| **P2** | **异步 Job**（durable、events / cancel / 续订） | 派单的地基 |
| **P3** | ACP 适配层 + 每用户 harness 实例；OIDC 登录 | harness 可插拔；多用户 |
| **P4** | 派单（先串行 + 人工门禁），再逐步加 artifact 依赖图 | 产品研发流水线 |

每阶段独立可交付，且可回退。

---

## 12. 现状对照

**已实现（见 DESIGN.md）**：OpenAI/Anthropic/Responses 三套协议、会话分桶与历史重放、工具桥（MCP）、权限自动应答、上游自动发现与托管、`/v1/usage`、`/v1/requests` + `/ui`、持久化、工具结果注释、终止后重开会话。

**本路线新增**：模型访问层、Role/虚拟模型、权限策略、异步 Job、ACP、平台层（DB/OIDC/多用户）、派单。

---

## 13. 非目标（明确排除）

- 不重写 agent / 工具 / MCP / 会话 runtime。
- 不实现模型推理。
- 不在 Janus 内造沙箱（下沉 OS）。
- 不为客户端做权限审批（无标准通道）。
- 不以订阅式 OAuth 作为多用户主干。
