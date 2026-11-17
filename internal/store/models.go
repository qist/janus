package store

// WorkItemModel is the persisted form of work.WorkItem. Contract and slice
// fields are JSON blobs so the contract can evolve without a migration for
// every new acceptance field.
type WorkItemModel struct {
	ID                 string `gorm:"primaryKey;size:64"`
	WorkspaceID        string `gorm:"index;size:64"`
	ParentID           string `gorm:"index;size:64"`
	Title              string `gorm:"size:200"`
	Brief              string `gorm:"type:text"`
	ContractJSON       []byte `gorm:"type:blob"`
	CapabilitiesJSON   []byte `gorm:"type:blob"`
	ExecutionSite      string `gorm:"size:16"`
	RequiredModesJSON  []byte `gorm:"type:blob"`
	DependsOnJSON      []byte `gorm:"type:blob"`
	Priority           int    `gorm:"index"`
	Status             string `gorm:"index;size:16"`
	Attempts           int
	LeaseExpiries      int
	Replicas           int
	CreatedBy          string `gorm:"size:64"`
	SubmittedBy        string `gorm:"index;size:64"`
	LastIdempotencyKey string `gorm:"index;size:64"`
	Version            int64  `gorm:"index"`
	CreatedAt          int64
	UpdatedAt          int64
}

func (WorkItemModel) TableName() string { return "work_items" }

// WorkClaimModel is the persisted form of work.Claim.
type WorkClaimModel struct {
	ID             string `gorm:"primaryKey;size:64"`
	WorkItemID     string `gorm:"index;size:64"`
	AgentID        string `gorm:"index;size:64"`
	Role           string `gorm:"size:16"`
	LeaseEpoch     int64  `gorm:"index"`
	LeaseExpiresAt int64  `gorm:"index"`
	HeartbeatAt    int64
	Forced         bool
	Plan           string `gorm:"size:512"`
	Note           string `gorm:"size:2048"`
	Status         string `gorm:"index;size:16"`
	IdempotencyKey string `gorm:"index;size:64"`
	EvidenceJSON   []byte `gorm:"type:blob"`
	CreatedAt      int64
}

func (WorkClaimModel) TableName() string { return "work_claims" }

// WorkArtifactModel is the persisted form of work.Artifact.
type WorkArtifactModel struct {
	ID         string `gorm:"primaryKey;size:64"`
	WorkItemID string `gorm:"index;size:64"`
	Kind       string `gorm:"size:16"`
	URI        string `gorm:"size:512"`
	SHA256     string `gorm:"index;size:64"`
	Bytes      int64
	Summary    string `gorm:"size:1024"`
	CreatedBy  string `gorm:"size:64"`
	CreatedAt  int64
}

func (WorkArtifactModel) TableName() string { return "work_artifacts" }

// WorkReviewModel is the persisted form of work.Review.
type WorkReviewModel struct {
	ID           string `gorm:"primaryKey;size:64"`
	WorkItemID   string `gorm:"index;size:64"`
	ReviewerID   string `gorm:"index;size:64"`
	Verdict      string `gorm:"size:16"`
	Reasons      string `gorm:"size:2048"`
	EvidenceJSON []byte `gorm:"type:blob"`
	CreatedAt    int64
}

func (WorkReviewModel) TableName() string { return "work_reviews" }

// WorkEventModel is an append-only audit/SSE record.
type WorkEventModel struct {
	ID         uint   `gorm:"primaryKey;autoIncrement"`
	WorkItemID string `gorm:"index;size:64"`
	Type       string `gorm:"index;size:32"`
	Actor      string `gorm:"size:64"`
	Payload    []byte `gorm:"type:blob"`
	CreatedAt  int64  `gorm:"index"`
}

func (WorkEventModel) TableName() string { return "work_events" }
