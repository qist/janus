# Janus 架构演进：控制平面 + Agent 生态

> 状态：讨论稿（未实现部分均为规划）
> 日期：2026-10-08
> 关系：本文描述 janus 的**目标架构与路线**；当前已实现细节见 [`DESIGN.md`](./DESIGN.md)。
> 2026-10-08 更新：新增**多 Agent 编排**章节（§6，Agent Registry / Router / DAG）与定位升级。

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
| 派单 / 调度 / 门禁 | 不实现模型推理 |
| Agent 编排（Registry / Router / DAG） | 不实现沙箱（下沉 OS） |
| 权限策略 | — |
| 工具桥（MCP） | — |
| 事件 / 审计 / 计量 | — |

**定位升级（2026-10-08）：Agent Gateway + Agent Runtime，而不是单纯的「OpenAI API 转换层」。**

类比：**Kubernetes 管理 Container，Janus 管理 Agent。** IDE（Trae / Cursor / CodeBuddy /
VSCode）永远只看到一个模型端点（`/v1/chat/completions`），下游可以是**整个 Agent 团队**
（Architect / Planner / Coder / Tester / Security / Reviewer）。

对客户端的竞争力不是「又一个写代码 Agent」（那条赛道 Claude / OpenAI 很强），而是
**AI Agent Operating System for IDE**：

- 一个 API 接入所有 IDE；
- 多模型混合 + 多角色协作；
- 企业权限控制 + 审计日志；
- Agent 生命周期管理（Registry / Router / DAG Scheduler）。

已有能力（session key、MCP bridge、权限隔离、Async Job、`/v1/requests` 审计）都是这一层的
**基础件**，不是终点；补齐 **Agent Registry + Intent Router + DAG Scheduler** 之后，
Janus 从「接口转换器」升级为真正的 **Agent Runtime**（详见 §6）。

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

**目标：不依赖 OpenCode Console；provider 与模型是「数据」，不是「文档/配置文件」。**

- **provider 抽象**：`{type: openrouter|openai|anthropic|litellm|opencode|local, base_url, auth}`。
  **auth 不止 api-key**：
  - `api-key`：BYOK（openrouter / openai / anthropic / litellm / 各网关 / 本地）；
  - `oauth-subscription`：订阅制授权（Claude 订阅、Codex/ChatGPT 登录态、OpenCode Go）。
    **按用户维度绑定**——每个用户自登自用，不做租户级共享（边界见 §11 #19）；
  - `free-tier`：免费档（如 OpenCode 免费额度，自带限流 / 地域限制）；
  - `none`：本地 / 无需鉴权。
- **模型自由创建**：providers / models 落 sqlite，用户可经 API / UI 自行增删改
  （base_url、auth 类型、模型列表、定价 / 限流），**不写死在配置文档里**。
- **模型目录**：先用公开注册表（models.dev）+ 各 provider `/models` 初始化，之后全凭用户编辑。
- **翻译网关**：LiteLLM / new-api / one-api / OpenRouter，同时暴露 Anthropic + OpenAI 协议；
  统一鉴权、限流、计量。
- **用户凭据中心**：api-key 与订阅登录态**统一加密存储**（AES-GCM，主密钥来自文件/env bootstrap），
  按用户隔离——谁的 key 谁用、谁的订阅谁用。
- **订阅式 OAuth** 由用户在自己账号下完成（`POST /v1/providers/{id}/login` 透出 URL / device code），
  不透传、不缓存到租户级。

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
一个 role 也可绑定**多 Agent 团队**（内部按 §6 编排，客户端无感知）。

---

## 6. 多 Agent 编排（Registry / Router / DAG）

> Router 决定「**调谁**」，Registry 决定「**谁知道什么**」，DAG / 派单决定「**谁先谁后、门禁在哪**」。
> 对外（IDE）永远只有一个 /v1 端点、一堆 `janus/<role>` 虚拟模型；多 Agent 全部在 Janus 内部消化。

### 6.1 Agent Profile：角色不是「模型后缀」，是一整套契约

```
agent:
  name: security-reviewer
  model: gpt-6
  system_prompt: "…"
  tools: [git, filesystem, grep]
  permissions:
    filesystem: { write: false }
  memory: security-memory        # 独立记忆域（§6.5）
  mode: remote-tools             # 复用 §2 执行模式
```

与 §5 role 的关系：**role 是客户端可选面（`janus/<role>` 虚拟模型），agent 是内部执行面**；
一个 role 可绑一个 agent，也可绑一个 agent 团队（team = 一个 DAG）。

### 6.2 Agent Registry：能力自描述 + Agent 市场

- 每个 agent **自描述** `skills / input / output` 契约，Janus 落 `agents` 表
  （id、name、skills、model、tools、permission、status）。
- 路由按能力匹配：`需要检查 SQL 注入` → `vulnerability_scan` → `security-agent`。
- **Agent 市场**：`janus agent install security-guru` 安装第三方 agent → 自动注册 skills →
  Router 自动可见（类似 IDE 插件）。比「把角色写死在代码里」可演化得多。

### 6.3 Agent Router：怎么知道调谁（四条路线，推荐混用）

| 方案 | 机制 | 优点 | 缺点 / 适用 |
|---|---|---|---|
| 1 显式指定 | 客户端/用户 `@architect`，或请求带 `agent:"architect"` | 准确、零成本 | 用户要懂角色名；适合工单 / IDE 高级模式 |
| 2 **意图 Router Agent（推荐主干）** | 专用 router 不干活，只输出 `{agents:[{name, reason}]}` | 通用、可扩展 | 多一跳、一点延迟/token |
| 3 阶段状态机 | 复杂任务按 plan→architect→code→test→review 阶段推进 | 稳定、可预期 | 不适用于一次性的小请求 |
| 4 能力自描述匹配 | Router 先查 Registry，按 skills 命中即路由 | 精确、可审计 | 依赖注册质量 |

**明确不做**：`strings.Contains(prompt, "设计")` 这类硬编码意图匹配——新角色一多就会废。

### 6.4 Agent 通信（内部协议）

```
AgentMessage { from, to, task_id, type: plan|code|test|report|review, payload }
```

- 走 Janus 内部（现有事件总线 + durable queue 的雏形）；**对外仍是标准 `tool_calls`**，
  客户端不用懂 MCP / AgentMessage。
- 只传**结论与交付物引用**（短引用/摘要），不传整段上下文——子 agent 的探索留在自己的
  上下文，主线只拿结论（省 token，见 §8.2 Token 控制）。

### 6.5 Agent 记忆（隔离）

- 每个 agent 独立记忆域（`architect-memory` / `coder-memory` / `security-memory`…），不混。
- 记忆挂 `agent + task/conversation` 维度，DB 落 KV/向量；可清、可导出，避免「串记忆」。

### 6.6 与任务派单（§9）的关系

Router 产出「谁参与」→ 派单引擎把 agents 摊成 **DAG 节点**（非线性、可并行）：

```
        Architect
           |
     +-----+------+
     |            |
   Coder      Security
     |
   Tester
     |
   Review
```

DAG 的边 = **artifact 依赖 + 门禁**（沿用 §9 artifact-gated 规则：只读可并行、同一 artifact
单 writer、门禁通过才推进）。

### 6.7 交付顺序（与 §12 路线图对齐）

1. **Agent Registry**：`agents` 表 + `janus/agent/*` 管理 API + 市场安装；
2. **Intent Router**：默认走方案 2 意图路由，方案 1/4 作旁路；
3. **DAG Scheduler**：扩展现有派单，agents 列表 → DAG 节点 → 门禁推进。

这三个补齐，Janus 就从「接口转换器」升级为真正的 **Agent Runtime**。

---

## 7. 权限策略

- **服务端裁决**：复用 harness 的权限事件（如 OpenCode `permission.asked`，`action=external_directory`）+ 会话 workspace，按 `allowed_paths / deny_paths / tools` 判 allow/deny。
- **观测**：只进 Janus 日志 / `/ui`（运维审计），**不推给客户端**（标准协议无此通道）。
- **硬边界**：
  - 目录 / 文件：可管（harness 权限事件 + workspace）。
  - 工具：可管（禁用工具 / 拒绝权限）。
  - **网络 / 命令：管不了**——一旦 shell 放行，只能靠 **OS 级隔离**（独立 user / cgroup / netns / 防火墙）。
- **不做**：客户端权限审批（无标准通道）；不在 Janus 里造沙箱。

---

## 8. 对话与上下文

### 8.1 存储不一致：不翻译存储，只映射会话
- **Janus canonical transcript**：客户端看到的那条对话线，只存 `role / content / tool_calls / 结果摘要`；不存 reasoning、文件快照、harness 内部噪音。
- **harness 原生 session**：各自存储，Janus 只记 `{harness, session_id}`。
- 适配靠 **ACP**（`session/new|load|resume|prompt|update|cancel`）。
- **硬限制**：会话**不能跨 harness 迁移**。默认**一条对话绑一个 harness**；切换 = 新 session + 重放 canonical。

### 8.2 Token 控制
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

### 8.3 有状态对话 API（生态优势）
自建 API 可做**有状态对话**：客户端只发**新消息**（非全量），彻底消除「客户端重发历史」的浪费。这是标准 OpenAI 客户端给不了的。

---

## 9. 任务派单（artifact-gated）

### 9.1 依赖是「交付物」，不是「角色顺序」
- 阶段启动充要条件：**输入 artifact 已冻结 + 已批准**。
- 阶段完成定义：**输出 artifact 已产出 + 通过验收**。
- **冻结（freeze）** 是防乱套的根：下游不能回头改上游决定。

### 9.2 角色流水线（默认串行 + 门禁）
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

### 9.3 并发规则
- **只读阶段可并行**（多审计维度、多分析）。
- **写阶段默认串行**；仅 artifact 不相交时并行（backend ∥ frontend，前提接口已冻结），各自 **worktree 隔离**。
- 铁律：**同一 artifact 只有一个 writer。**

### 9.4 对象
```
Goal   产品目标
Stage  阶段：{role, mode, inputs[], outputs[], gate}
Task   派单单元：{task_id, target{client_id,harness}, workspace, budget, status, events}
Gate   门禁：人工 / 自动（测试、审计）
```
- dispatcher = **artifact-gated DAG 状态机**，默认单条关键路径单飞。
- 每个阶段 = 一个 **durable job**；完成 + 门禁通过 → 推进下一个。

---

## 10. 平台层（多用户）

### 10.1 配置入库（SQLite，config-as-data）
- **数据库 = SQLite**（延续现有 `BRIDGE_DB`：单文件嵌入式、WAL、零外部依赖）；不引 PostgreSQL 等外部服务。
- **配置都是数据**：providers、models（§4）、roles、agents（§6）、users、api_keys、budgets、permissions
  全部落 sqlite，经 API / UI 管理——**运行配置不写进文档 / 配置文件**。
- **文件 / env 只做 bootstrap**：监听地址、DB 路径、加密主密钥、初始 admin（启动前必需的最小集）。

### 10.2 身份与用户自建 key
- **OIDC**（Janus 当 Relying Party；IdP 用 Authentik / Keycloak / Zitadel / Google / GitHub）。
- **用户自助创建 API key**：Web / API 里生成 `sk-janus-…`，绑定用户 + 权限 + 预算；
  可随时吊销、可轮换；程序化客户端 / IDE 用它接入。
- **订阅制授权**：用户可在自己账号下绑定订阅登录（Claude / Codex / OpenCode Go 等），
  凭据加密归个人，组织不共享订阅（§11 #19 的边界）。
- 两条腿都要：浏览器 OIDC + API key。

### 10.3 隔离
| 方案 | 说明 | 代价 |
|---|---|---|
| 逻辑隔离 | 共享一个 harness，Janus 按 user 打标 | 便宜，隔离弱 |
| **每用户 / 每租户一个 harness 实例** | Janus 管生命周期，按 DB 生成 config/auth | 重，但一步解决 provider 配置 + 鉴权 + 隔离 |

**推荐后者**（扩展现有 autostart 能力）。

### 10.4 provider 登录
- **不要 Janus 自己实现各厂商 OAuth**。
- 走 harness 自身登录（`opencode auth login`）或 **ACP `authenticate`**；Janus 只透出 URL / device code。
- API：`POST /v1/providers/{id}/login` → `{url, user_code}`；`GET .../login/status`。

---

## 11. 可行性与硬约束

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
| 23 | Agent Registry + 能力自描述匹配 | 🟡 中 | `agents` 表 + skills 契约 + 管理 API |
| 24 | Intent Router（Router Agent） | 🟡 中 | 复用现有 Agent 通道；多一跳 |
| 25 | DAG Scheduler（agents 列表 → DAG） | 🔴 难 | 依赖异步 Job + 派单（#15/#16） |
| 26 | 模型/供应商自由创建（DB + API/UI） | 🟡 中 | providers/models 表 + 管理 API（§4） |
| 27 | 订阅制授权（用户维度 OAuth/登录态） | 🟡→🔴 中高 | 每用户自登自用；不透传/不共享（§4） |
| 28 | 配置即数据（sqlite 全量，config-as-data） | 🟡 中 | 已有 `BRIDGE_DB` 基础，补管理 API/UI |

**三条硬约束别硬碰**：会话不能跨 harness（#18）、网络/命令隔离靠 OS（#20）、订阅 OAuth 别做主干（#19）。

---

## 12. 路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| **P0** | Role 注册表 + `janus/<role>` 虚拟模型 + API key；配置「文件 + DB 覆盖」 | 让客户端只选 role；模型可集中配置 |
| **P1** | 自建模型网关 + 目录（摆脱 Console）+ **模型/供应商自由创建（DB + API/UI）**；权限策略（服务端裁决 + 审计） | 模型访问层独立；权限可管 |
| **P2** | **异步 Job**（durable、events / cancel / 续订） | 派单与 Multi-Agent 编排队列的地基 |
| **P3** | ACP 适配层 + 每用户 harness 实例；OIDC 登录 + **用户自建 API key**；**订阅制授权（用户维度）**；sqlite 配置中心（config-as-data） | harness 可插拔；多用户 |
| **P4** | 派单（先串行 + 人工门禁）+ **Agent Registry / Intent Router**（§6.3 方案 1/2/4），再逐步加 artifact 依赖图 | 产品研发流水线 / Multi-Agent |

每阶段独立可交付，且可回退。

---

## 13. 现状对照

**已实现（见 DESIGN.md）**：OpenAI/Anthropic/Responses 三套协议、会话分桶与历史重放、**无会话 id 的 scope（IDE+项目）共享会话**（独立 TTL）、工具桥（MCP，等待分两档）、权限自动应答、上游自动发现与托管、`/v1/usage`、`/v1/requests` + `/ui`、持久化、工具结果注释、终止后重开会话、**跨平台（linux/darwin/windows）**、统一出站 `User-Agent`。

**已实现（本路线的早期落点）**：
- **harness 配置自动注入**：janus 通过 `OPENCODE_CONFIG_CONTENT` 注入自动生成的 `orchestrator` 白名单（§3.3），无需手写 `~/.config/opencode/opencode.jsonc`；
- **janus 自管上游**：`OPENCODE_REUSE_EXTERNAL=false` 时 janus 总是自己拉起 OpenCode（注入的前提）。

**本路线新增**：模型访问层、Role/虚拟模型、权限策略、异步 Job、ACP、平台层（DB/OIDC/多用户）、派单、**多 Agent 编排（Registry / Router / DAG，§6）**。

---

## 14. 非目标（明确排除）

- 不重写 agent / 工具 / MCP / 会话 runtime。
- 不实现模型推理。
- 不在 Janus 内造沙箱（下沉 OS）。
- 不为客户端做权限审批（无标准通道）。
- 不以订阅式 OAuth 作为多用户主干。
- 不做硬编码意图匹配（`if contains(prompt, "设计")` 型路由）——角色路由走 §6.3 的 Registry / Router。
