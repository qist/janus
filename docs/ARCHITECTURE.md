# Janus 架构演进：控制平面 + Agent 生态

> 状态：讨论稿（未实现部分均为规划）
> 日期：2026-10-08
> 关系：本文描述 janus 的**目标架构与路线**；当前已实现细节见 [`DESIGN.md`](./DESIGN.md)。
> 2026-10-08 更新：新增**多 Agent 编排**章节（§6，Agent Registry / Router / DAG）与定位升级。
> 2026-10-08 更新：补充 **§6.8 协商式路由（Self-Organizing Team）**——Coordinator + 协商协议 + max_round + Task Lock。
> 2026-10-08 更新：补充 **§6.9 Skill Registry（技能中心）**——技能独立于 Agent、动态挂载、版本化共享；同步 §6.2 内嵌 skills 改为挂载引用。
> 2026-10-08 更新：§6 一致性修订——AgentMessage 扩展协商消息、Task 对象携带技能、API 前缀约定（/v1/* vs janus/*）、越权防护覆盖技能挂载、角色表补技能行。
> 2026-10-08 更新：补充 **§6.10 Agent 执行预算（Tool Call Rounds 与止损）**——分层轮数 + 预算/阶段/异常三重止损 + 死循环检测；Task.budget 具体化。
> 2026-10-08 更新：补充 **§6.11 无 IDE 执行拓扑（Local Tool Executor）**——tool_target=client|local，闲聊/CI/Async Job 不依赖 IDE 也能跑完整多 Agent 编排。
> 2026-10-08 更新：§6.11 修正——**tool_target 不用全局 env**，改为**任务 / 角色属性**（任务来源决定：Web/Job→local、IDE→client，同进程共存）。

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
| 技能中心（Skill Registry / 动态挂载，§6.9） | 不实现模型推理 |
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
| `remote-tools` | 只用声明的工具，走 MCP 工具桥；**工具执行位置按 Task/role 的 `tool_target`（client→客户端 / local→Janus 主机，§6.11）** | 客户端 或 Janus 主机 | B：`orchestrator` + `TOOL_CALLING=true` |
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
| 3（无 IDE：自用 / CI / Async Job） | Janus | **Janus（Local Tool Executor，§6.11）** | **tool_call**（同一协议，本地执行） |

拓扑 1 与拓扑 3 可在**同一 Janus 进程内并存**：IDE 会话走 `client`（远程桥），janus Web / Async Job
走 `local`（§6.11）；`tool_target` 是任务 / 角色属性，不是全局开关。

拓扑 2 需要客户端跑一个 **Janus worker**（连 Janus → 领 id → 收任务 → 跑本地 harness → 回流事件）。任务体 `{task_id, harness, prompt, workspace, mode, permissions, budget}`；target = `{client_id, harness}`。

---

## 4. 模型访问层

**目标：不依赖 OpenCode Console；provider 与模型是「数据」，不是「文档/配置文件」。**

- **provider 抽象**：`{type: openrouter|openai|anthropic|litellm|opencode|local, base_url, auth}`。
  **auth 不止 api-key**：
  - `api-key`：BYOK（openrouter / openai / anthropic / litellm / 各网关 / 本地）；
  - `oauth-subscription`：订阅制授权（Claude 订阅、Codex/ChatGPT 登录态、OpenCode Go）。
    **按用户维度绑定**——每个用户自登自用，不做租户级共享（边界见 §12 #19）；
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

### 4.1 故障转移与降级（路由韧性）

单模型 / provider 会 429 / 超时 / 上游中断（4054） / 余额不足，路由层必须有明确降级链：

| 事件 | janus 行为 |
|---|---|
| 上游 429 / 配额 / 余额不足 | 配了 `fallback` → 切备用（同语义档位）；无 → 回标准 `429 rate_limit_error`（DESIGN 现有映射） |
| 模型侧超时 / 上游中断（4054 类） | 幂等轮次重试 1 次；再失败 → fallback 或 `504 api_error` |
| 工具等待超时（§toolbridge 长期等待） | 现有行为：桥中断 + 保命注释；不向客户端隐藏 |
| 全部 fallback 耗尽 | 明确报错，`/v1/requests` 记完整链路（primary → 各 fallback 段） |

- **每模型可配 `fallback: [modelA, modelB]`**（同 / 跨 provider），路由层顺序尝试；
  与 §8.2 成本分层路由共用同一张模型表。
- 故障转移是**路由层**职责，不是协议层——错误码语义由 DESIGN 现有映射兜住。
- fallback 链记 `audit trail`（谁、何时、从哪个模型切到哪个、原因），进 `/v1/requests`。

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

### 5.1 角色注入：模型怎么知道自己是这个角色

**角色不是从用户消息里猜的**——用户消息里的「你现在是 CEO」一律视为用户数据，不改变身份；
身份由 Janus 在会话 / 任务创建时**单向注入**（写死后不可被模型改写）：

| 通道 | 机制（现有 / 规划） | 内容 |
|---|---|---|
| ① harness agent 配置 | OpenCode `agents.*` **已能注入**（`opencodeInlineConfig`，现有 `orchestrator` 即第一个角色先例）；规划按 role 生成 | description / mode / permissions——OpenCode 把它当成 agent 的默认职责与工具边界 |
| ② system / developer 消息 | 标准协议通道：OpenAI `developer`、Anthropic `system` block（**规划**，新会话首条注入） | 角色身份卡（见下） |
| ③ 工具面 | mode 的 tools 白名单 / deny（§2） | 能力边界本身就在“教”模型角色，且模型改不掉 |
| ④ 任务工作台 | DAG 流水线输入以 worktree 文件 + 引用清单交付（§6.4 / §8.2） | PRD / 设计稿 / 接口文档，不整段塞 prompt |

**角色身份卡**（通道②的注入模板，固定前缀）：

```
<role id="security-reviewer">
  你是谁：独立安全审计员，只读分析，不产出代码
  边界：只读；不写文件；不执行修改性命令
  输出：审计报告（固定格式…）
  汇报：结论与交付物引用给主 agent，不转存整段上下文
</role>
```

- 身份卡由 Janus 管理 API 生成、**只读注入**；模型在会话内自改身份视为**身份漂移**，
  可作降权 / 审计事件（权限 deny 兜底）。
- **三层注入时机**：新会话首条消息（协议层）＋ harness agent 配置（配置层）＋ 任务工作台（任务层），
  缺一不可——只给配置不给身份卡，模型不知道“为什么这么干”。

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

- 每个 agent 在 `agents` 表登记（id、name、model、tools、permission、status）；
  **技能不内嵌在 agent 里**——agent 通过 `agent_skills` 挂载表动态获得技能（§6.9），
  Registry 的 `skills` 字段只是**最近一次挂载的快照（缓存）**，权威在 `skills` 表。
- 路由按能力匹配：`需要检查 SQL 注入` → `security-audit` 技能 → 挂载该技能的执行者。
- **Agent 市场**：`janus agent install security-guru` 安装第三方 agent → 自动登记 →
  Router 自动可见（类似 IDE 插件）。比「把角色写死在代码里」可演化得多。

### 6.3 Agent Router：怎么知道调谁（四条路线，推荐混用）

| 方案 | 机制 | 优点 | 缺点 / 适用 |
|---|---|---|---|
| 1 显式指定 | 客户端/用户 `@architect`，或请求带 `agent:"architect"` | 准确、零成本 | 用户要懂角色名；适合工单 / IDE 高级模式 |
| 2 **意图 Router Agent（推荐主干）** | 专用 router 不干活，只输出 `{agents:[{name, reason}]}` | 通用、可扩展 | 多一跳、一点延迟/token |
| 3 阶段状态机 | 复杂任务按 plan→architect→code→test→review 阶段推进 | 稳定、可预期 | 不适用于一次性的小请求 |
| 4 能力自描述匹配 | Router 先查 Registry，按 skills 命中即路由 | 精确、可审计 | 依赖注册质量 |
| 5 协商式组队（规划，§6.8） | Coordinator 主持，候选 PROPOSE → DISCUSS → ALLOCATE | 团队自组织、分工合理、可解释 | 多跳；依赖 Registry + Router + DAG + 异步 Job |

**明确不做**：`strings.Contains(prompt, "设计")` 这类硬编码意图匹配——新角色一多就会废。

### 6.4 Agent 通信（内部协议）

```
AgentMessage { from, to, task_id, type: plan|code|test|report|review|propose|discuss|vote|allocate, payload }
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

### 6.7 交付顺序（与 §13 路线图对齐）

1. **Agent Registry**：`agents` 表 + `janus/agent/*` 管理 API + 市场安装；
2. **Intent Router**：默认走方案 2 意图路由，方案 1/4 作旁路；
3. **DAG Scheduler**：扩展现有派单，agents 列表 → DAG 节点 → 门禁推进；
4. **协商式路由（§6.8）**：Coordinator + 协商协议 + Task Lock，默认关闭按任务开启。
5. **Skill Registry（§6.9）**：`skills` / `agent_skills` 表 + 挂载 API + `match`，是 2/4 路的匹配底座。

这三个补齐，Janus 就从「接口转换器」升级为真正的 **Agent Runtime**。

### 6.8 协商式路由（Self-Organizing Team）

> 定位：Registry / Router / DAG 解决"派人干活"；协商式路由解决**派活之前先组队**——
> 多个 Agent 像临时项目组一样先讨论、再分工，Router 退化为 **Coordinator（主持）**。
> 不固定角色（没有 architect-agent / coder-agent 这种写死分工），是 §6.3 的**方案 5**，
> 默认关闭、按任务开启，失败可逐级降级。

**为什么不是完全自治**：自由讨论会内耗——两个 agent 抢同一块活、一个 agent 反复提重构、
无限争论。工程口径是**半自治**：

- Agent 自由提出分工 ✅
- Agent 自己认领任务 ✅
- Coordinator 最终确认 ✅
- 权限与资源由 Janus 控制 ✅（§7）

#### 6.8.1 协商协议（Agent Negotiation Protocol）

```
DISCOVER   按 skill 匹配粗筛候选池（§6.9 skills.match：capability → 候选 agent）
PROPOSE    每个候选提交 {skills, proposal}（各一句建议，不展开）
DISCUSS    候选互相看到建议，最多 max_round 轮；只允许补充 / 反驳 / 让渡
VOTE/ALLOCATE  Coordinator 收束 → plan: [{task, owner, depends_on}]
EXECUTE     plan → Task Queue → DAG（§9 派单；资源按 §7 权限放行）
REVIEW      门禁（§9.4 Gate）——人工检查点是硬要求（§12 #17）
```

- 每轮讨论**只传结论与短引用**（复用 §6.4 AgentMessage 摘要原则，不传整段上下文）；
- 讨论预算挂在 per-job 上（§8.2 / §11.5），超预算直接进 ALLOCATE。

#### 6.8.2 Coordinator（主持，不干活）

- 可以是"最省模型 + 严格只读工具"的专用 agent；职责只有三个：
  **收集意见 → 结束讨论 → 生成任务图**，不执行任何业务代码。
- 输出物 = `plan[]`（与意图路由 §6.3 方案 2 同形），下游 DAG Scheduler 无感知——
  协商只是"路由的另一种输入"，**不新增执行层**。
- **降级链**：讨论超轮数 / 超时 / 预算 → Coordinator 直接按收到的 PROPOSE 拍板；
  连 PROPOSE 都拿不到 → 回退方案 2 意图路由 / 方案 3 阶段状态机。

#### 6.8.3 防内耗规则

| 规则 | 说明 |
|---|---|
| `max_round = 3` | DISCUSS 轮数硬上限（默认 1～2，可配）；超限即进 ALLOCATE |
| 单轮 token 预算 | 每轮讨论挂 per-job budget，防止"讨论到没钱执行"（§8.2） |
| 一人一票 | VOTE 只对"该技能域内"的候选有效，避免外行投票 |
| Coordinator 一票否决 | 分配冲突时 Coordinator 定夺，不做无限投票 |
| 参与白名单 | Registry 的 agent 可标 `conference: true/false`，决定是否进候选池 |

#### 6.8.4 Task Lock（认领与冲突）

- 协商产物落到 Task Queue 后，Agent 通过 **claim** 领取：`POST /janus/task/claim {agent, task_id}`；
- **文件级锁**：写任务认领后即锁定其输出路径（`file: internal/user.go → owner: agent-A`）；
- 冲突规则 = §9.3"同一 artifact 只有一个 writer"的具体化：
  - 锁冲突 → 拒绝认领，提示已认领者与交接路径（`handoff`）；
  - 只读任务（audit / review）不抢写锁，可并行（沿用 §9.3）；
  - 锁随 job 生命周期释放（cancel / 超时 / 门禁通过）。

#### 6.8.5 与现有设计的关系

- **不新增执行层**：`plan[]` → Task Queue → DAG（§9）完全复用；
- **不新增权限层**：认领即申请权限，放行与否仍由 §7 服务端裁决；
- **只新增路由层一跳**：DISCOVER / PROPOSE / DISCUSS ≈ 意图路由的"多智能体版本"，
  失败可逐级降级（6.8.2）；落地顺序依赖 §6.7 的前三步（Registry → Router → DAG）。

### 6.9 Skill Registry（技能中心，动态技能挂载）

> 定位：**技能不写死在 Agent 里**。Agent 是通用执行体，根据任务**临时挂载**技能成为对应
> 专家（`一个 Coder Agent 永远写代码` 的固定角色模式被 `通用 Agent + 动态技能` 取代）。
> 技能是独立、版本化、可共享的资源，分工匹配 = **技能匹配**（§6.8 协商的 DISCOVER 也用它），
> 而不是预设角色。类比：Kubernetes 的镜像仓库之于 Container，Skill Registry 之于 Agent。

#### 6.9.1 Skill ≠ Prompt（四件套）

很多系统把 Skill 等同于提示词，不够。一个 Skill 是：

```
skill:
  id: golang.backend.v1        # id@version 引用，不直接覆盖
  capability: [go, gin, grpc, mysql]   # 匹配用（Router / DISCOVER）
  prompt:                       # ① Instructions：注入系统，指导如何完成任务
    system: |
      你是一名资深 Go 后端工程师…
  tools: [filesystem, shell, git, trivy…]   # ② Tools：进权限白名单（§7）
  knowledge: [go-style-guide.md, api-rule.md, database-rule.md]  # ③ Knowledge：可检索文档
  workflow:                     # ④ Workflow：可执行的步骤（编译进 plan/DAG，不新造执行层）
    steps: [{run: go test ./...}, {run: golangci-lint}, {report: result}]
  version: 1.0.0
```

- prompt / knowledge 注入会话，tools 进白名单（§7 服务端裁决），workflow 复用 §9 派单；
- 挂载后生效范围 = **会话 / 任务级**；任务结束按 TTL / 门禁**卸载**，防止技能串台
  （与 §6.5 记忆隔离同一精神）。

#### 6.9.2 三层作用域（共享管理）

| 作用域 | 路径语义 | 示例 |
|---|---|---|
| 全局 | `/skills/global` | git、linux、security-basic |
| 团队 / 项目 | `/projects/{project}/skills` | api-design、go-style、deployment |
| 私有 | `/agents/{id}/skills` | experimental-code-review |

- 作用域决定可见性与**可挂载授权**；权限裁决仍走 §7（allowed_paths / tools / 服务端裁决）。

#### 6.9.3 动态挂载（Agent 不预装技能）

```
agent 初始: {agent_id: a001, model: gpt-6, skills: []}
任务: 开发 CoreFusion API
Router 匹配（§6.3 方案 4 / §6.8 DISCOVER）⟶ load_skills: [golang.backend.v1, grpc.v1]
挂载后: prompt 注入 / tools 进白名单 / knowledge 可检索 / workflow 可执行
```

- 匹配 = `POST /v1/skills/match {task}` → `{skills: [security.audit, jwt.review]}`；
- 同一 capability 多版本命中 → 按匹配分 / 策略选版（`@latest` 解析到 ENABLE 的版本）；
- **技能组合冲突检测**：同任务挂载多个技能时校验 tools / prompt / knowledge 不冲突
  （如两个技能对同一 tool 声明不同权限 → 拒绝共装，按 §7 最小权限合并）。

#### 6.9.4 生命周期（类软件包）

```
CREATE → REGISTER → PUBLISH → ENABLE → UPDATE → DEPRECATE
```

- **版本化**：`security-audit@1.2.0`，不直接覆盖；引用 `id@version` 或 `id@latest`；
- DEPRECATE 后新任务不挂载，存量任务可继续到门禁；
- 审核门：PUBLISH 需通过 Review（人工 / 自动，呼应 §12 #17「必须留人工检查点」）。

#### 6.9.5 Agent 生成技能（学习闭环，高级）

- Agent 完成批量同类任务后总结规律（如 80% Go 项目用 repository pattern + service layer）→
  生成技能草案（`golang-enterprise-pattern.skill`）→ **Review** → PUBLISH；
- 生成物必须过审才能进共享池，防污染；默认生成到私有域，人工提升到团队 / 全局。

#### 6.9.6 API 与管理

```
GET  /v1/skills                    # 查询技能（按作用域 / capability，含 version / score）
POST /v1/skills/match              # 任务 → 技能匹配（Router / DISCOVER 内部用）
POST /janus/agents/{id}/skills     # 给 agent 挂载技能（管理面，与 janus/agent/* 同族）
janus skill install|publish|deprecate  # CLI，与 §6.2 Agent 市场同款语义
```

> **API 前缀约定**：协议兼容端点走 `/v1/*`（OpenAI / Anthropic 同形）；Janus 管理 / 控制面
> 走 `janus/*`（`janus/agent/*`、`janus/skill/*`、`janus/task/*`），与虚拟模型 `janus/<role>` 同族。

#### 6.9.7 落库与一致性

- §10.4：新增 `skills`（id、capability、version、scope、prompt/tools/knowledge/workflow 引用）
  与 `agent_skills`（agent_id、skill_id@version、enable、mounted_at）两表；
  `agents.skills` 字段降级为挂载快照缓存，不再是权威；
- Router 方案 4 与 5 的能力匹配统一走 `skills.match`，不是读死静态字段。

### 6.10 Agent 执行预算（Tool Call Rounds 与止损）

> 定位：多 Agent 不能无限跑。**预算（Budget）+ 阶段限制 + 异常终止** 三重止损；
> 大任务不靠放大轮数，而是拆 **Job → Task**（§9 / §13 P2），真正的大任务走 **Async Agent Job**，
> 不一直挂 Chat Stream。**轮数不是唯一指标**——token / 时长 / 成本往往更关键。

#### 6.10.1 两个概念：工具轮 vs 生命周期

- **工具轮**：`LLM → Tool → 结果` 一个回合；一次 Bug 修复（读→改→编译→查日志→修复→测试）
  很容易 10+ 轮。
- **Agent 生命周期**：Planning → Coding → Testing → Review，累计几十轮工具调用。
- 因此限制是**分层**的（`planning_rounds / execution_rounds / review_rounds / total_rounds`），
  不是单一 `MAX_TOOL_ROUNDS`。

#### 6.10.2 默认预算（全局 + 每角色覆盖）

```
agent_execution:            # 全局默认（高级用户 / 平台可调大）
  max_rounds: 50
  max_tool_calls: 100
  max_duration: 30m
  max_tokens: 200_000
  same_tool_limit: 3        # 连续相同 tool+args 上限
  no_progress_limit: 5      # 连续无进展（环境信号未变）上限
```

| 角色 | 建议轮数上限 | 说明 |
|---|---|---|
| 普通 Chat Agent | 10 | 查询文件 / 改配置 / 简单命令 |
| Coding Agent | 30 ~ 50 | 修 Bug 一次 10+ 轮；默认给 50 |
| Coordinator / Planner | 5 | 只主持 / 只出计划，不执行 |
| Architect | 10 | 出方案 / 接口设计 |
| Tester / Security | 20 | 跑测试 / 审计 |
| Reviewer | 10 | 只读 Review |

- role / agent 可覆盖全局默认（沿用 §5 role 属性）；
- 超出上限不是静默失败：`finish` 原因（`length` / `budget`）+ 审计落 `/v1/requests`。

#### 6.10.3 防死循环三检测

1. **相同工具调用检测**：连续相同 `{tool, args}` ≥ `same_tool_limit(3)` → 中断该轮，
   提示换策略（附已尝试清单，喂回 agent 而不是沉默）；
2. **无进展检测**：连续 `no_progress_limit(5)` 次环境信号未变（同一测试失败 / 同一 diff /
   同一报错）→ 中断 → **Need human review**（或交 Coordinator 重新分工）；
3. **资源上限**：`max_tokens` / `max_cost`（$）/ `max_duration`——比轮数更重要，命中即停。

#### 6.10.4 大任务拆 Job，不是放大轮数

- 不要 `max_rounds=500`；拆 `Job → Task1..N`，每个 Task 20~50 轮；
- Task = **durable Async Job**（§13 P2），预算挂在 Task 上（§9.4 `Task.budget`）；
- 单 Task 超预算 → 摘要当前成果 + 事件回流 → 平台 / 人工决定续跑或终止。

#### 6.10.5 与现有能力的关系

- executor 的 `tokenBudget`（max_tokens）已是雏形，扩展 rounds / duration / cost；
- 与 §11.5 配额同库：**配额是租户 / 用户级，执行预算是任务 / Agent 级**，两层都命中才放行；
- 与 §6.8 协商衔接：DISCUSS 的 `max_round` / 单轮 token 预算就是本节的协商实例。

### 6.11 无 IDE 执行拓扑（Local Tool Executor）

> 定位：IDE 不接入时（本机自用 / 命令行 / CI / 定时任务 / **Async Agent Job**），协商、技能挂载、
> 执行预算这些编排能力**照常工作**。工具执行者从"客户端"换成"Janus 本地执行器"，
> 其余链路（MCP 工具桥 / 挂起 / 回填 / 审计）完全复用。

现状模式 A（`build` + 无工具桥）只是"单 agent 用自带工具"，**没有统一的可控工具集**——
多 Agent 编排要的是：每个 agent 按 §6.9 挂载技能、按 §7 裁决权限、按 §6.10 止损预算。
这必须走工具桥，只是执行者不同。

#### 6.11.1 tool_target：同一工具桥，两个执行者

现有工具桥链路：agent → MCP `tools/call` → 桥**挂起** → 执行者执行 → 结果回填 → agent 继续。
`tool_target` 只决定"谁执行"：

| target | 执行者 | 场景 |
|---|---|---|
| `client` | 客户端 IDE | 远程 IDE 接入；工具在客户端机器上跑（远程文件） |
| `local` | **Local Tool Executor** | janus Web / API / Async Job 创建的任务；工具在 janus 主机、会话目录内跑 |

- **tool_target 不是全局环境变量，而是「任务 / 角色」属性**：janus 可多角色并存，
  本地（Web 建任务）与远程（IDE 接入）**同时接入同一进程**，各自按来源决定执行者；
- 挂起 / 回填 / 超时 / 孤儿判定全部复用 `toolSession`（§toolbridge），只换执行后端；
- `client` 的"等客户端回填"改成 `local` 的"立即本地执行"，不需要任何人机回路。

#### 6.11.2 Local Tool Executor 内置工具集

```
fs     read_file / write_file / patch / glob / search      （限会话目录，§7 裁决）
shell  run {cmd, cwd, timeout}                              （白名单 + 超时 + 上限）
git    status / diff / log / commit / branch                 （只读默认，写需授权）
test   run {cmd}  如 go test ./... / golangci-lint           （复用 §6.10 预算）
web_search（已有 serverToolFunc 先例：桥自己执行，不甩客户端）
```

- 全部在**会话目录内**执行；越界访问由 §7 服务端裁决（allowed_paths / deny_paths / tools）；
- 全部进 `/v1/requests` 审计；`same_tool_limit` / `no_progress_limit`（§6.10.3）在本地
  **真正可落地**（远程那侧对客户端执行是盲区）；
- 实现即现有 `serverToolFunc` 机制的扩展：web_search 已是"桥自己执行"的先例，
  这里把一组内置工具统一注册为会话默认 `serverTools`。

#### 6.11.3 无 IDE 时的完整链路

```
Job / REST / cron / CLI
      │ 派遣（§9，Async Job）
      ▼
Coordinator ↔ Agent1/2/3（§6.8 协商 · §6.9 技能挂载）
      │ MCP tools/call（同一工具桥协议）
      ▼
Local Tool Executor（fs/shell/git/test…，§7 裁决 + §6.10 预算 + 审计）
      ▼
janus 主机 · 会话目录
```

- 与 IDE 场景的唯一差异：没有客户端回填环节，其余（协商、分工、权限、预算、审计）一致；
- 模式 A 演进为 **A'**：`orchestrator` + `TOOL_CALLING=true`，Web / Async Job 任务
  默认 `tool_target=local`——多 Agent 编排 + 本地工具统一控制面，替代"build + 自带工具不可控"
  （IDE 会话仍走 `client`，两者同进程共存）。

#### 6.11.4 tool_target 的赋值（任务来源决定，role 可覆盖）

| 任务来源 | 默认 tool_target | 说明 |
|---|---|---|
| janus Web / API / Async Job 创建的任务 | `local` | 无 IDE；走 Local Tool Executor |
| IDE 接入（`/v1` 协议请求，客户端声明了 tools[]） | `client` | 远程桥，工具在 IDE 机器执行 |
| IDE 接入但未声明 tools | `local`（兜底） | 用本地默认工具集，避免无工具可用 |

- `tool_target` 是 **Task 属性**（§9.4），与 mode、skills、budget 并列；role / agent 可写死默认
  （沿用 §5 role 属性，如 `audit` 强制 `local` + 只读工具）；
- 同一 Janus 进程内不同任务可以落在不同 target：Web 任务走 `local`、IDE 会话走 `client`，互不干扰；
- 本地工具集按 role / 技能裁剪（§6.9 `skills.tools` + §5 `role.tools`），不需要全局开关；
- `hybrid`（单任务内部分工具本地 / 部分客户端）仍为后续可选，不进主线。

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
| **执行预算 / 止损（§6.10）** | per-task rounds / tool_calls / duration / tokens / cost：轮数不是唯一，token 与成本更重要 |
| **子代理只回摘要** | 探索在子代理上下文，主线拿结论 |
| **按模型窗口适配截断** | `models.context_window` 配在模型表（§10.4）；摘要/截断阈值按窗口比例（如 70% 触发摘要），避免小窗口模型被同一条策略卡死 |

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
Task   派单单元：{task_id, skills[]（挂载技能 id@version）, tool_target(client|local，§6.11), target{client_id,harness}, workspace, budget{rounds, tool_calls, duration, tokens, cost}(§6.10), status, events}
Gate   门禁：人工 / 自动（测试、审计）
```
- dispatcher = **artifact-gated DAG 状态机**，默认单条关键路径单飞。
- 每个阶段 = 一个 **durable job**；完成 + 门禁通过 → 推进下一个。

---

## 10. 平台层（多用户）

### 10.1 配置入库（SQLite，config-as-data）
- **数据库 = SQLite**（单文件嵌入式、WAL、零外部依赖）；不引 PostgreSQL 等外部服务。
  schema 以 §10.4 表清单为准，不做任何“沿用旧结构”的兼容设计。
- **配置都是数据**：providers、models（§4）、roles、agents（§6）、**skills / agent_skills（§6.9）**、
  users、api_keys、budgets、permissions 全部落 sqlite，经 API / UI 管理——**运行配置不写进
  文档 / 配置文件**。
- **文件 / env 只做 bootstrap**：监听地址、DB 路径、加密主密钥、初始 admin（启动前必需的最小集）。
  旧 env 映射（`BRIDGE_PROJECT_MAP` / `BRIDGE_MODEL_MAP` 等）**不做兼容层**：数据库为准，
  无回退分支、无双读逻辑；存量配置一次性导入，导入完旧 env 即废弃。
- **部署形态**：默认单进程单机（SQLite 文件 + WAL）。多副本高可用不引外部服务，**本路线不做**；
  备份 = 定期 `.backup` + WAL checkpoint。

### 10.2 身份与用户自建 key
- **OIDC**（Janus 当 Relying Party；IdP 用 Authentik / Keycloak / Zitadel / Google / GitHub）。
- **用户自助创建 API key**：Web / API 里生成 `sk-janus-…`，绑定用户 + 权限 + 预算；
  可随时吊销、可轮换；程序化客户端 / IDE 用它接入。
- **订阅制授权**：用户可在自己账号下绑定订阅登录（Claude / Codex / OpenCode Go 等），
  凭据加密归个人，组织不共享订阅（§12 #19 的边界）。
- 两条腿都要：浏览器 OIDC + API key。

### 10.3 隔离
| 方案 | 说明 | 代价 |
|---|---|---|
| 逻辑隔离 | 共享一个 harness，Janus 按 user 打标 | 便宜，隔离弱 |
| **每用户 / 每租户一个 harness 实例** | Janus 管生命周期，按 DB 生成 config/auth | 重，但一步解决 provider 配置 + 鉴权 + 隔离 |

**推荐后者**（扩展现有 autostart 能力）。

### 10.4 核心表清单（config-as-data 落库对齐）

| 表 | 关键字段 | 说明 |
|---|---|---|
| `users` | id, oidc_sub | 身份；`oidc_sub` 唯一 |
| `api_keys` | id, user_id, hash, scopes | `sk-janus-…`；**只存 hash**（§15） |
| `providers` | id, type, base_url, auth_ref | auth 指向加密凭据（§4） |
| `models` | id, provider_id, name, context_window, pricing, fallback[] | §4 / §4.1 共用 |
| `roles` | id, agent_ref, mode, permissions, budget | §5 客户端可选面 |
| `agents` | id, model, tools, permission, status, skills(快照) | §6 Registry；技能权威在 `skills` 表 |
| `skills` | id, capability[], version, scope(global/team/private), prompt, tools, knowledge, workflow | §6.9 Skill Registry；`id@version` 不覆盖 |
| `agent_skills` | agent_id, skill_id@version, enable, mounted_at | §6.9 动态挂载；任务级装载 / 卸载 |
| `conversations` | id, user_id, scope, harness, session_id, summary | canonical 映射（§8.1） |
| `budgets` | subject(user/org/project), limit, window | §11.5 |
| `usage` | request_id, user_id, model, tokens(含 cache 分项), cost | §11 逐条落库 |
| `requests` | id, user_id, org, model, status, cache_hit, fallback_chain | 审计（现有 `/v1/requests` 的库化） |

### 10.5 provider 登录
- **不要 Janus 自己实现各厂商 OAuth**。
- 走 harness 自身登录（`opencode auth login`）或 **ACP `authenticate`**；Janus 只透出 URL / device code。
- API：`POST /v1/providers/{id}/login` → `{url, user_code}`；`GET .../login/status`。

---

## 11. 观测与计量（用量 / 请求 / 缓存 / 多租户监控）

> 结论先行：**上游（OpenAI / Anthropic / OpenCode）不会通过普通聊天接口透传累计用量、成本、
> 缓存命中率**——每次响应只带**单请求**的 usage。累计与命中率必须 Janus 自己算：
> **带内聚合为主，带外专查为辅**。

### 11.1 数据来源三层

| 层 | 来源 | 提供什么 |
|---|---|---|
| 带内（in-band） | 每次请求响应的 `usage`（非流式；流式配 `stream_options.include_usage`） | prompt / completion tokens；Anthropic `cache_read_input_tokens` / `cache_creation_input_tokens`；OpenAI 自动提示缓存分项 |
| 带外（out-of-band） | 上游用量端点**专门查询**：OpenAI Usage、Anthropic Usage & Cost、**OpenCode Go `/usage`（代码已支持 `OPENCODE_GO_USAGE`）**、LiteLLM / new-api `/balance` `/usage` | 累计用量 / 成本 / 限额 / 余额（校正带内差额） |
| Janus 自有 | 请求日志（`/v1/requests`）、响应缓存命中（§8.1 DiffNone→回放）、网关指标（`/metrics`） | 请求量、时延、错误率、**自身缓存命中率**、工具调用/超时 |

### 11.2 聚合与口径

- **单请求 usage 必须逐条落库**（含 cache 分项），累计才有依据；无 usage 的请求按模型单价（§4
  模型表可配定价）估算或补查。
- **缓存命中率两个口径**：
  1. **上游 prompt 缓存命中**：`cache_read / (cache_read + cache_creation + no_cache)`——看供应商省钱效果；
  2. **Janus 自身响应缓存命中**：`replyCached` / DiffNone 回放次数 ÷ 总请求——看桥省了多少上游调用。
- **成本**：模型单价 × tokens（缓存价更低），按用户 / 租户 / 项目累计落库。

### 11.3 多租户监控 API（对外）

- **上游不按「IDE 组织」维度**：OpenAI / OpenCode 用量按账号算，没有多企业视角；
  多租户维度只有 Janus 自己有。**因此对外监控 API 是 Janus 自己的**：
  - `GET /v1/usage`（现有，扩展）：按 `user / org / project / date_range` 返回累计用量与成本；
  - `GET /metrics`（现有）：按 `user / org / project` 打标，权限内可见；
  - `GET /v1/requests`（现有，审计）：请求明细 + 缓存命中标记 + 用量，支持租户过滤；
  - 管理 API：`GET /v1/admin/orgs/{id}/usage` 等，仅管理员。
- **不透传上游单请求 usage 给普通客户端**：那是计费/审计数据，走管理员与统计维度，
  不进标准聊天响应（标准协议无此通道）。

### 11.4 落地顺序

1. 单请求 usage 全量落 sqlite（含 cache 分项）→ 累计口径成立；
2. `/v1/usage` 扩展租户维度 + `/metrics` 打标；
3. 带外查上游用量端点（OpenCode Go 已有）校正差额；
4. 缓存命中率两口径上线（上游 prompt 缓存 + 自身响应缓存）。

### 11.5 配额与限流（计量 → 配额 → 限流 闭环）

计量（§11.1-11.4）之后必须落到**执行**，否则只是报表：

| 层级 | 配额 | 超限行为 |
|---|---|---|
| 每用户 / 每租户 | 月度 token / 金额（`budgets` 表） | 拒绝新请求（`402`/`429`）或降级到备用模型 |
| 每 role / 每任务 | role 的 `budget`（§5）+ `Task.budget`（§6.10：rounds/tokens/时长/成本） | **先摘要在途轮次、再停**（统一 §8.2 口径） |
| 每模型 / provider | 上游 rate limit（分钟级） | 排队 / 退避 / 切 fallback（§4.1） |

- **决策点都在 Janus**（网关层统一裁决），harness / 客户端不感知；
- 配额与用量同表同库，`/v1/usage` 直接可查剩余额度；
- 预算剩余 = 已批准额度 − 用量聚合（带内落库值），管理 API 与 `/ui` 可查。

---

## 12. 可行性与硬约束

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
| 28 | 配置即数据（sqlite 全量，config-as-data） | 🟡 中 | §10.4 表清单 + 管理 API/UI；无 env 兼容层 |
| 29 | 用量/成本聚合（单请求 usage 全量落库，含 cache 分项） | 🟡 中 | 现有 `/v1/usage` + `/v1/requests` 基础 |
| 30 | 缓存命中率两口径（上游 prompt 缓存 + 自身响应缓存） | 🟡 中 | 上游 usage 字段 + DiffNone 回放计数 |
| 31 | 多租户监控 API（`/v1/usage` 扩展 + `/metrics` 打标） | 🟡 中 | 权限 + 租户标签维度 |
| 32 | 角色注入（agent 配置 + system 身份卡 + 任务工作台） | 🟡 中 | §5.1；`orchestrator` 注入已有先例 |
| 33 | 模型故障转移 / 降级（fallback 链 + audit trail） | 🟡 中 | 路由层职责（§4.1） |
| 34 | 配额与限流执行（budgets 表 + 网关裁决） | 🟡 中 | 用量同库（§11.5） |
| 35 | 核心表清单落库（users/api_keys/providers/models/roles/agents/usage…） | 🟡 中 | §10.4；不保留 env 兼容分支 |
| 36 | 安全与合规（密钥轮换 / 日志脱敏 / 审计保留期 / 越权过滤） | 🟡 中 | §15；AUDIT.md 已有明细 |
| 37 | 协商式路由（Coordinator + Negotiation Protocol + max_round + Task Lock） | 🔴 难 | 依赖 #23/#24/#25 + 异步 Job；先文档后落地（§6.8） |
| 38 | Skill Registry（skills/agent_skills 表 + 挂载 API + match + 生命周期） | 🟡→🔴 中大 | 依赖 #23；与方案 4/5 匹配共用（§6.9） |
| 39 | Agent 执行预算（rounds / tool_calls / duration / tokens / cost + 相同调用 / 无进展检测） | 🟡 中 | executor tokenBudget 已有雏形；与 Task.budget 对接（§6.10） |
| 40 | Local Tool Executor（tool_target 按任务来源 per-task：Web/Job→local、IDE→client） | 🟡 中 | serverToolFunc（web_search）已是先例；依赖 #4 权限裁决 + Task.tool_target（§6.11） |

**三条硬约束别硬碰**：会话不能跨 harness（#18）、网络/命令隔离靠 OS（#20）、订阅 OAuth 别做主干（#19）。

---

## 13. 路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| **P0** | Role 注册表 + `janus/<role>` 虚拟模型 + **角色注入（§5.1：agent 配置 + 身份卡 + 工作台）** + API key；配置「文件 + DB 覆盖」 | 让客户端只选 role；模型可集中配置 |
| **P1** | 自建模型网关 + 目录（摆脱 Console）+ **模型/供应商自由创建（DB + API/UI）**；权限策略（服务端裁决 + 审计）+ **Local Tool Executor（§6.11）** | 模型访问层独立；权限可管；无 IDE 也能跑编排 |
| **P1.5** | **观测与计量**：单请求 usage 全量落库（含 cache 分项）、成本聚合、缓存命中率两口径、多租户监控 API（§11） | 平台的计费 / 审计 / 监控卖点 |
| **P2** | **异步 Job**（durable、events / cancel / 续订；承载 task claim 与技能挂载 TTL） | 派单与 Multi-Agent 编排队列的地基 |
| **P3** | ACP 适配层 + 每用户 harness 实例；OIDC 登录 + **用户自建 API key**；**订阅制授权（用户维度）**；sqlite 配置中心（config-as-data） | harness 可插拔；多用户 |
| **P4** | 派单（先串行 + 人工门禁）+ **Agent Registry / Intent Router**（§6.3 方案 1/2/4）+ **Skill Registry（§6.9，skills 表 + 挂载 + match）**，再逐步加 artifact 依赖图；最后叠加**协商式组队（§6.8，方案 5）** | 产品研发流水线 / Multi-Agent 自组织 |

每阶段独立可交付，且可回退。

---

## 14. 现状对照

**已实现（见 DESIGN.md）**：OpenAI/Anthropic/Responses 三套协议、会话分桶与历史重放、**无会话 id 的 scope（IDE+项目）共享会话**（独立 TTL）、工具桥（MCP，等待分两档）、权限自动应答、上游自动发现与托管、`/v1/usage`、`/v1/requests` + `/ui`、`/metrics`（opencode_bridge_* 指标）、持久化、工具结果注释、终止后重开会话、**跨平台（linux/darwin/windows）**、统一出站 `User-Agent`。

**已实现（本路线的早期落点）**：
- **harness 配置自动注入**：janus 通过 `OPENCODE_CONFIG_CONTENT` 注入自动生成的 `orchestrator` 白名单（§3.3），无需手写 `~/.config/opencode/opencode.jsonc`；
- **janus 自管上游**：`OPENCODE_REUSE_EXTERNAL=false` 时 janus 总是自己拉起 OpenCode（注入的前提）。

**本路线新增**：模型访问层、Role/虚拟模型、**角色注入（§5.1）**、**故障转移（§4.1）**、权限策略、异步 Job、ACP、平台层（DB/OIDC/多用户，**§10.4 表清单**）、派单、**多 Agent 编排（Registry / Router / DAG / 协商式路由 / Skill Registry，§6）**、**观测计量与配额（§11）**、**安全合规（§15）**。

---

## 15. 安全与合规（密钥 / 审计 / 隐私）

> 详细审计设计见 [`AUDIT.md`](./AUDIT.md)；本章只定架构级原则。

### 15.1 密钥与凭据
- **三层密钥**：主密钥（文件/env bootstrap，AES-GCM）→ 用户凭据（api-key / 订阅登录态，加密落库 §4）
  → 访问密钥（`sk-janus-…`，**只存 hash**）。
- **轮换**：主密钥轮换 = `rekey` 管理操作（重加密全部凭据）；`sk-janus-*` 随时吊销 / 再生（§10.2）。
- **脱敏**：请求 / 审计日志对 api-key、provider auth、订阅 token 脱敏（只留尾 4 位或 hash）；
  prompt / 回复正文默认不落库（除显式审计开关）。

### 15.2 审计与合规
- `/v1/requests`（§11.3）是审计主通道：谁、何时、哪个用户/租户、模型、用量、fallback 链、权限裁决事件。
- **保留期**：请求明细与 usage 设保留窗口（如 90 天），到期归档 / 清理；用量聚合单独长存（§11）。
- `/ui` 与日志只进运维侧，**不进客户端**（§7 观测原则）。

### 15.3 越权防护（多租户）
- 所有查询按 `user_id / org` 过滤（服务端强制，不是前端过滤）；管理 API 仅 admin 角色。
- `agents` / `roles` / `skills`（含 `agent_skills` 挂载）的资源引用在会话创建时**校验归属与作用域**，
  防跨租户引用 / 跨作用域挂载。
- 会话 scope（IDE+项目，§8.1）归属到 user，跨用户不可见。

---

## 16. 非目标（明确排除）

- 不重写 agent / 工具 / MCP / 会话 runtime。
- 不实现模型推理。
- 不在 Janus 内造沙箱（下沉 OS）。
- 不为客户端做权限审批（无标准通道）。
- 不以订阅式 OAuth 作为多用户主干。
- 不做硬编码意图匹配（`if contains(prompt, "设计")` 型路由）——角色路由走 §6.3 的 Registry / Router。
