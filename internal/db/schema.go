package db

// schema.go —— 新数据模型（见 docs/REFACTOR.md §2）。
//
// 设计原则：providers / credentials / models / routes / keys / agents / settings
// 全部是数据，经 /api/v1 管理。旧 accounts / account_models 表已删除，不做兼容。

// Provider 是一个厂商端点。Type 决定协议翻译方向。
type Provider struct {
	ID        string `gorm:"primaryKey;size:64"` // anthropic / openai / opencode-go / workbuddy
	Name      string `gorm:"size:120"`
	Type      string `gorm:"size:32"`  // openai-chat | openai-responses | anthropic-messages
	BaseURL   string `gorm:"size:512"`
	AuthKind  string `gorm:"size:32"`  // api_key | oauth | none
	Headers   []byte `gorm:"type:blob"` // 附加静态头（JSON object）
	Enabled   bool   `gorm:"index"`
	CreatedAt int64
	UpdatedAt int64
}

// Credential 是一份厂商凭据。**没有 Active 字段**：每份凭据独立 client，
// 天然并发，不存在「同一时刻只有一份凭据生效」的全局状态。
type Credential struct {
	ID         string `gorm:"primaryKey;size:64"`
	ProviderID string `gorm:"index;size:64"`
	Label      string `gorm:"index;size:80"` // provider 内唯一，客户端可用 "<label>/<model>"
	Secret     []byte `gorm:"type:blob"`     // AES-GCM 密文
	Priority   int    `gorm:"index"`
	Enabled    bool   `gorm:"index"`
	LastUsedAt int64
	LastError  string `gorm:"size:512"`
	FailUntil  int64  `gorm:"index"` // 冷却截止时间（Unix 秒），0 = 不在冷却
	CreatedAt  int64
	UpdatedAt  int64
}

// Model 是 janus 侧的模型目录条目。
type Model struct {
	ID           string `gorm:"primaryKey;size:200"` // provider_id + "/" + upstream_id
	ProviderID   string `gorm:"index;size:64"`
	UpstreamID   string `gorm:"size:160"`
	DisplayName  string `gorm:"size:200"`
	Capabilities []byte `gorm:"type:blob"` // {tools,vision,reasoning,context,max_output}
	Pricing      []byte `gorm:"type:blob"`
	Source       string `gorm:"size:16"` // discovered | manual
	Enabled      bool   `gorm:"index"`
	CreatedAt    int64
	UpdatedAt    int64
}

// ModelRoute 是路由规则。Scope 决定作用域：
//
//	global     → 所有请求
//	provider   → 该 provider 下的请求
//	credential → 该凭据下的请求
//	role       → janus/<role> 虚拟模型
type ModelRoute struct {
	ID        string `gorm:"primaryKey;size:64"`
	Scope     string `gorm:"index;size:16"`
	ScopeID   string `gorm:"index;size:64"`
	Alias     string `gorm:"index;size:160"` // 小写，尾部 * 为前缀通配
	Target    string `gorm:"size:200"`       // provider/model[:variant]
	Priority  int    `gorm:"index"`
	Enabled   bool   `gorm:"index"`
	CreatedAt int64
	UpdatedAt int64
}

// APIKey 是访问凭据。明文只在创建/轮换时返回一次，库里只存 sha256。
type APIKey struct {
	ID           string `gorm:"primaryKey;size:64"`
	Kind         string `gorm:"index;size:16"` // admin | client | agent | tool
	Name         string `gorm:"size:120"`
	Prefix       string `gorm:"index;size:24"`
	Hash         string `gorm:"uniqueIndex;size:64"`
	Scopes       []byte `gorm:"type:blob"`
	ProviderIDs  []byte `gorm:"type:blob"` // 空 = 全部（agent key 默认全量可用）
	CredentialIDs []byte `gorm:"type:blob"`
	ExpiresAt    int64  `gorm:"index"`
	Revoked      bool   `gorm:"index"`
	LastUsedAt   int64
	CreatedAt    int64
}

// Agent 是 harness 注册表。OpenCode 只是其中一条记录。
type Agent struct {
	ID        string `gorm:"primaryKey;size:64"` // opencode / codex / claude-code / direct
	Name      string `gorm:"size:120"`
	Kind      string `gorm:"size:32"` // opencode | direct | cli
	BaseURL   string `gorm:"size:512"`
	AuthKind  string `gorm:"size:32"` // basic | bearer | none
	Secret    []byte `gorm:"type:blob"`
	APIKeyID  string `gorm:"size:64"` // 反向注入给它的 agent key
	Enabled   bool   `gorm:"index"`
	Config    []byte `gorm:"type:blob"`
	Status    string `gorm:"size:32"` // unknown | ready | down
	LastSeen  int64
	CreatedAt int64
	UpdatedAt int64
}

// Setting 是运行期设置。Value 为 JSON，保留类型。
type Setting struct {
	Key       string `gorm:"primaryKey;size:80"`
	Value     []byte `gorm:"type:blob"`
	UpdatedAt int64
}

// UsageEvent 是本地用量记账的一行：每次模型面请求落一条，
// 按 provider / credential / key / model 维度可聚合（见 docs/ARCHITECTURE.md §11）。
//
// Source 区分数据来源：local = janus 自己从响应里解析；upstream = 从上游账单接口导入。
type UsageEvent struct {
	ID              string  `gorm:"primaryKey;size:64"`
	Ts              int64   `gorm:"index"`
	ProviderID      string  `gorm:"index;size:64"`
	CredentialID    string  `gorm:"index;size:64"`
	KeyID           string  `gorm:"index;size:64"`
	KeyKind         string  `gorm:"size:16"`
	Protocol        string  `gorm:"size:32"`
	Model           string  `gorm:"index;size:200"` // 客户端请求的原始模型名
	UpstreamModel   string  `gorm:"size:200"`
	Stream          bool
	Status          int
	ErrorCode       string  `gorm:"size:80"`
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CacheWriteTokens int64
	ReasoningTokens int64
	CostUSD         float64
	DurationMs      int64
	Source          string `gorm:"size:16;index"` // local | upstream
}

// AdminUser 是首次启动自动生成的管理员账号（用户名 + 密码登录面板）。
type AdminUser struct {
	ID           string `gorm:"primaryKey;size:64"`
	Username     string `gorm:"uniqueIndex;size:80"`
	PasswordHash string `gorm:"size:200"`
	CreatedAt    int64
	UpdatedAt    int64
}
