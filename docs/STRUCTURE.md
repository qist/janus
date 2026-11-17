# Janus 目录规范

> 状态：设计定稿配套规范
> 目标：从 0 重写后，目录结构能直接映射 `docs/REFACTOR.md` 的模型面、工作板、审计与账号池；
> 旧代码不进入新结构，避免历史包袱污染。

---

## 0. 三条原则

1. **目录按职责分，不按技术名词分**：没有 `utils/`、`common/`、`helpers/`、`models/` 这种垃圾桶。
2. **依赖单向**：`cmd → api → domain → store/platform`，不允许反向或环。
3. **一个目录一个包**：包名小写、单数；接口定义在消费方，不搞全局 `types`。

---

## 1. 顶层结构

```
janus/
├── cmd/
│   ├── janus/                 # 服务进程入口（唯一 main）
│   └── janusctl/              # 运维 / 调试 CLI（可选）
├── api/
│   └── openapi.yaml           # /v1 与 /api/v1 的接口契约
├── internal/
│   ├── bootstrap/             # 6 个 bootstrap env、master key、首启引导
│   ├── config/                # settings schema、默认值、校验
│   ├── store/                 # GORM 模型、事务、迁移
│   │   └── migrations/        # 生产用版本化 SQL
│   ├── gateway/               # 模型面：路由、provider client、协议翻译、透传
│   ├── work/                  # 工作板：contract、claim、capsule、状态机、验证、合入
│   ├── scoring/               # 打分、tier、自治等级、归因
│   ├── policy/                # 敏感操作分类、风险分、策略引擎
│   ├── auditor/               # 审计模型客户端、rubric、决策缓存
│   ├── auth/                  # API key、scope、capability token
│   ├── agent/                 # AgentRuntime 接口 + opencode/direct/cli 适配
│   ├── channel/               # harness provision：opencode/codex/claudecode/workbuddy
│   ├── mcp/                   # MCP server、tool registry、tool bridge
│   ├── protocol/
│   │   ├── openai/            # OpenAI Chat / Responses 线格式
│   │   ├── anthropic/         # Anthropic Messages 线格式
│   │   └── shared/            # 错误信封、消息、工具、ID
│   ├── api/                   # HTTP 传输层：/v1、/api/v1、/mcp、/ui
│   ├── ui/                    # go:embed dist；只放生成产物与 embed.go
│   ├── usage/                 # 用量、成本、额度、账号池
│   ├── observe/               # 日志、指标、trace
│   ├── platform/              # paths、fs、proc、net、clock、ids
│   └── testkit/               # fakes、fixtures、golden（只允许测试引用）
├── web/                       # 前端源码（Preact + htm 或 Svelte）
├── docs/
│   ├── REFACTOR.md            # 设计基线
│   ├── STRUCTURE.md           # 本文
│   ├── OPERATIONS.md          # 部署、升级、排障
│   ├── SECURITY.md            # 威胁模型、密钥、审计
│   └── ADR/                   # 架构决策记录
├── deploy/
│   ├── Dockerfile
│   ├── janus.service
│   └── nginx.conf.example
├── scripts/
│   ├── dev.sh
│   ├── build.sh
│   └── release.sh
├── tests/
│   ├── e2e/
│   └── compat/
├── go.mod / go.sum
├── Makefile
├── README.md
├── LICENSE
├── VERSION
└── .github/workflows/
```

根目录**不再放 `main.go`**；入口统一在 `cmd/janus/`。

---

## 2. `internal` 分层

| 层 | 目录 | 只做什么 | 不做什么 |
|---|---|---|---|
| 传输 | `api` | HTTP 路由、鉴权、请求/响应转换 | 不写业务规则、不直接读 DB |
| 领域 | `work` / `scoring` / `policy` / `auditor` / `gateway` / `agent` | 工作板、打分、授权、路由、运行时 | 不拼 SQL、不关心 HTTP 细节 |
| 持久化 | `store` | 表结构、事务、迁移、查询 | 不写 HTTP、不写业务决策 |
| 协议 | `protocol/*` | 纯线格式类型与转换 | 不依赖 DB、不依赖 HTTP server |
| 平台 | `platform` / `observe` | 路径、进程、网络、时钟、日志、指标 | 不依赖领域包 |
| 入口 | `cmd/*` | 装配依赖、启动/关闭 | 不写业务逻辑 |

### 当前已落地的包（v2 骨架）

上面的目录是目标形态；截至 v2 起步，实际存在且被测试覆盖的只有这些：

| 包 | 职责 |
|---|---|
| `internal/work` | 工作板状态机：契约、租约、fencing、路径锁、证据与评审；只通过 `Repository` 读写 |
| `internal/store` | GORM 模型与实现：`Repository`、事务、胶囊表、SQLite 指标（`DBStatus`） |
| `internal/auth` | API key scope、capability token、路径作用域 |
| `internal/verify` | 执行契约 checks（超时、输出哈希、尾部截断） |
| `internal/capsule` | 有界上下文胶囊：token 预算、版本、stale、裁剪 |
| `internal/contextpack` | 认领交接包：brief + 胶囊 + 最近拒绝 + workspace 事实 |
| `internal/worksvc` | 组合层：租约 + 交接包、证据/失败回写胶囊、租约持有者校验 |
| `internal/api` | HTTP 传输层：`/api/v1/work-items/*`、鉴权与错误映射 |
| `internal/mcp` | MCP 传输层：`/mcp` JSON-RPC、`work_*` 工具，复用同一批 scope 与错误码 |
| `internal/policy` | 确定性授权：敏感操作分类、路径/命令规则、审计/人审需求 |
| `internal/auditor` | 独立审计：结构化输入输出、quorum、置信度下限、失败即拒、决策缓存 |
| `internal/gateway` | 模型面最小实现：OpenAI 兼容 `chat/completions` 客户端（多 provider、多 key 失败切换） |
| `internal/pool` | 账号池：额度状态、冷却、候选排序、失败分类、append-only 账号事件 |
| `internal/usage` | 用量账本：append-only 记账、按 key/group/工作项/账号/时间聚合 |
| `internal/authz` | 授权编排：policy → auditor → 单次能力令牌，绑定租约与动作哈希 |

依赖方向同样成立：`api → worksvc → work/contextpack/capsule`，`store` 实现接口但不被领域包 import。

### 依赖方向

```
cmd
 └── api
      ├── work / scoring / policy / auditor
      ├── gateway / agent / channel / mcp
      ├── auth / usage
      └── store / platform / observe
```

- `store` 不得 import `api`、`gateway`。
- `protocol` 不得 import `store`。
- `platform` 不得 import 领域包。
- `testkit` 只能被 `_test.go` 引用。

---

## 3. 命名与文件规范

- 包名：小写、单数，如 `work`、`policy`、`auditor`；禁止 `utils`、`common`、`helpers`。
- 文件名：小写 `snake_case.go`，如 `work_item.go`、`capability_token.go`；测试 `_test.go`。
- 导出类型/函数：`CamelCase`；错误：`ErrXxx`；接口：按角色命名（`Runtime`、`Store`、`Auditor`），不叫 `IWorkService`。
- JSON / DB 字段：`snake_case`；Go 字段：`CamelCase`。
- 时间：DB 统一 Unix 秒（`int64`），与 `REFACTOR.md` 一致。
- ID：`crypto/rand` 生成，前缀区分类型（`wk_`、`goal_`、`cap_`）。
- 错误：领域包定义错误类型，`api` 统一映射为 `{"error":{"code","message","details"}}`。

---

## 4. 测试、生成物与配置

### 测试

- 单元测试与被测包同目录：`work_item_test.go`。
- 端到端：`tests/e2e/`，只通过 HTTP / MCP 入口。
- 兼容矩阵：`tests/compat/`，覆盖 OpenAI / Anthropic / Responses / 工具桥。
- 测试数据：`internal/testkit/fixtures/`；黄金文件：`testdata/`。

### 生成物

- `internal/ui/dist/` 由 `web/` 构建生成，**不手改**。
- `api/openapi.yaml` 是接口契约源文件；改接口先改它。
- `go.sum` 提交；`go.mod` 只保留实际依赖。
- 不提交二进制、`dist/`、workspace、日志、`.venv/`。

### 配置

只有 6 个 bootstrap env 留在环境变量：

```
JANUS_ADDR
JANUS_DB
JANUS_DB_KEY
JANUS_DATA_DIR
JANUS_LOG_LEVEL
JANUS_BOOTSTRAP_TOKEN
```

其余全部进 `settings` 表，由 `config.SettingSpec` 声明，前端按 schema 自动生成表单。

---

## 5. 旧代码清理评估

### 当前状态

| 项 | 数量 |
|---|---|
| main 跟踪文件 | 203 |
| `internal/` | 175（`api` 67 / `channel` 23 / `ui` 22） |
| 旧文档 | 6（`ARCHITECTURE` / `AUDIT` / `COMPAT` / `DESIGN` / `REFACTOR` / `workbuddy-decrypt`） |
| 旧测试 | 4 个 Python 脚本 |
| 分支 | `main`、`fix-issue-229`、`origin/main`、`origin/fix-issue-229` |
| 标签 | `v0.3.x` 系列 |

结论：**旧代码是 v0.3.x 的 OpenCode 兼容桥，与新设计（模型面 + 工作板 + 审计 + 账号池）不是同一套骨架。保留会带来三类污染：**

1. 旧 `internal/api` 67 个文件会把新传输层拖回旧路由；
2. 旧 `channel` / `workbuddy` / `ui` 的配置与注入逻辑会和新 `agent` / `gateway` 冲突；
3. 旧测试与旧文档会把“已废弃行为”当成契约。

### 建议

**要清理，但不要直接 `rm`。先存档，再一次性替换。**

| 方案 | 做法 | 评价 |
|---|---|---|
| **A. 普通清理提交（推荐）** | 先打 tag `legacy-v0.3.63`；在 main 上一个提交删除旧代码，换新骨架 | 历史可追溯、无需 force push、评审简单 |
| B. 全新 orphan 分支 | 新建无历史 main，旧 main 改名为 `legacy` | 历史最干净，但必须 force push main，风险高 |
| C. 新仓库 | 归档旧仓库，新建 janus-v2 | 最彻底，但丢 issue / star / 链接 |

默认按 **A** 执行。只有你明确要“零历史”，才做 B。

---

## 6. 清理清单

### 保留 / 重写

| 路径 | 处理 |
|---|---|
| `LICENSE` | 保留 |
| `.gitignore` | 保留，补 `build/`、workspace、`docs/huashu.php` |
| `go.mod` / `go.sum` | 保留 module `janus`，裁掉旧依赖 |
| `Makefile` | 重写：`build` / `test` / `lint` / `ui` / `dist` |
| `README.md` | 重写为新定位，不保留旧桥说明 |
| `VERSION` / `CHANGELOG.md` | 保留，CHANGELOG 重新起一节 |
| `docs/REFACTOR.md` | **保留**，设计基线 |
| `docs/STRUCTURE.md` | 新增（本文） |
| `.github/workflows/` | 重写 CI / release |
| `deploy/` | 重写 Dockerfile / systemd / nginx |
| `janus.env.example` | 替换为 `janus.bootstrap.example`（只含 6 项） |

### 删除（旧 tag 里仍可找回）

| 路径 | 原因 |
|---|---|
| `internal/` 全部 175 文件 | 旧桥业务，不进入新骨架 |
| 根 `main.go` | 入口改到 `cmd/janus/` |
| `tests/*.py` | 绑定旧接口 |
| `scripts/run.sh` | 旧启动方式 |
| `Dockerfile`（根） | 由 `deploy/Dockerfile` 取代 |
| `docs/ARCHITECTURE.md` / `AUDIT.md` / `COMPAT.md` / `DESIGN.md` / `workbuddy-decrypt.md` | 旧设计/旧兼容结论，已被 REFACTOR 取代 |
| `docs/huashu.php` | 与 Janus 无关的 IPTV 跳转脚本，移出仓库或单独存放 |

### 不碰

- `d8d043541aca226c/`、`default/`：可能是运行期 workspace，先确认再移走，不直接删。
- `dist/`、根 `janus` 二进制：已在 `.gitignore`，清理时删除即可。

---

## 7. 执行步骤（待确认后执行）

1. `git tag legacy-v0.3.63 && git push origin legacy-v0.3.63`；
2. 新建 `chore/reset-v2` 分支；
3. 删除第 6 节“删除”清单；
4. 建立第 1 节新目录骨架（只放空包 + `doc.go`，不写业务）；
5. 重写 `go.mod` / `Makefile` / `README.md` / `.github` / `deploy`；
6. `go build ./...` + `go test ./...` 通过；
7. 合并回 `main` 并推送。

> 第 1 步的 tag 是回滚保险；第 3 步之后，旧代码仍可从 tag 找回。

---

## 8. 验收

- 根目录没有 Go 文件，入口只在 `cmd/janus`；
- `internal/` 只有新骨架包，`git grep` 不到旧包名（`upstream`、`modelx`、`workbuddy` 旧路径）；
- `go build ./...`、`go test ./...`、`go vet ./...` 全绿；
- `git ls-files | wc -l` 从 203 降到预期骨架规模（约 30–50）；
- 旧代码可通过 `git show legacy-v0.3.63:<path>` 找回。
