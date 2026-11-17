// Package work implements the self-claim work board: contracts, leases,
// fencing tokens, review transitions, and the state machine that keeps
// agent collaboration bounded and auditable.
package work

// Status is the lifecycle state of a work item.
type Status string

const (
	StatusOpen        Status = "open"
	StatusRunning     Status = "running"
	StatusReview      Status = "review"
	StatusApproved    Status = "approved"
	StatusIntegrating Status = "integrating"
	StatusDone        Status = "done"
	StatusBlocked     Status = "blocked"
	StatusCancelled   Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusRunning, StatusReview, StatusApproved, StatusIntegrating, StatusDone, StatusBlocked, StatusCancelled:
		return true
	default:
		return false
	}
}

// Role is who holds the lease for a work item.
type Role string

const (
	RoleWorker     Role = "worker"
	RoleVerifier   Role = "verifier"
	RoleIntegrator Role = "integrator"
)

func (r Role) Valid() bool {
	switch r {
	case RoleWorker, RoleVerifier, RoleIntegrator:
		return true
	default:
		return false
	}
}

// Risk drives verification depth and autonomous merge eligibility.
type Risk string

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

func (r Risk) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	default:
		return false
	}
}

// Autonomy is the earned review level for a model/task pair.
type Autonomy string

const (
	AutonomyL0 Autonomy = "L0"
	AutonomyL1 Autonomy = "L1"
	AutonomyL2 Autonomy = "L2"
	AutonomyL3 Autonomy = "L3"
)

func (a Autonomy) Valid() bool {
	switch a {
	case AutonomyL0, AutonomyL1, AutonomyL2, AutonomyL3:
		return true
	default:
		return false
	}
}

// Check is one machine-verifiable acceptance command.
type Check struct {
	Name           string `json:"name"`
	Command        string `json:"cmd"`
	TimeoutSeconds int    `json:"timeout"`
	ExpectExit     int    `json:"expect_exit"`
}

// Contract is the negotiable part of a work item. It is the source of truth
// for scope, acceptance, budget, risk, and review autonomy.
type Contract struct {
	Goal          string   `json:"goal"`
	Acceptance    []string `json:"acceptance"`
	Checks        []Check  `json:"checks"`
	Risk          Risk     `json:"risk"`
	Paths         []string `json:"paths"`
	BudgetTokens  int64    `json:"budget_tokens"`
	Autonomy      Autonomy `json:"autonomy"`
	CapsuleID     string   `json:"capsule_id"`
	PrefixVersion string   `json:"prefix_version"`
	Version       int      `json:"version"`
}

// CheckResult is one executed acceptance command. Output is truncated and
// stored by reference; the hash lets a verifier prove the output was not
// edited after the fact.
type CheckResult struct {
	Name       string `json:"name"`
	Command    string `json:"cmd"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	OutputSHA  string `json:"output_sha256,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
}

// Passed reports whether the check met the contract's expected exit code.
func (r CheckResult) Passed(expectExit int) bool { return r.ExitCode == expectExit }

// Evidence is what a worker must supply on submit. Without it the item cannot
// enter review: a diff reference plus the real output of every contract check.
type Evidence struct {
	Artifacts []string      `json:"artifacts,omitempty"` // artifact ids / URIs
	DiffHash  string        `json:"diff_hash,omitempty"`
	Checks    []CheckResult `json:"checks,omitempty"`
	Summary   string        `json:"summary,omitempty"`
}

// WorkItem is one claimable unit of delivery.
type WorkItem struct {
	ID                 string   `json:"id"`
	WorkspaceID        string   `json:"workspace_id"`
	ParentID           string   `json:"parent_id,omitempty"`
	Title              string   `json:"title"`
	Brief              string   `json:"brief"`
	Contract           Contract `json:"contract"`
	Capabilities       []string `json:"capabilities,omitempty"`
	ExecutionSite      string   `json:"execution_site,omitempty"` // server|client
	RequiredModes      []string `json:"required_modes,omitempty"` // native|bridge|none
	DependsOn          []string `json:"depends_on,omitempty"`
	Priority           int      `json:"priority"`
	Status             Status   `json:"status"`
	Attempts           int      `json:"attempts"`
	LeaseExpiries      int      `json:"lease_expiries"`
	Replicas           int      `json:"replicas"`
	CreatedBy          string   `json:"created_by"`
	SubmittedBy        string   `json:"submitted_by,omitempty"` // last worker that submitted
	LastIdempotencyKey string   `json:"last_idempotency_key,omitempty"`
	Version            int64    `json:"version"`
	CreatedAt          int64    `json:"created_at"`
	UpdatedAt          int64    `json:"updated_at"`
}

// ClaimStatus is the lifecycle of one lease.
type ClaimStatus string

const (
	ClaimActive     ClaimStatus = "active"
	ClaimSubmitted  ClaimStatus = "submitted"
	ClaimApproved   ClaimStatus = "approved"
	ClaimRejected   ClaimStatus = "rejected"
	ClaimReleased   ClaimStatus = "released"
	ClaimExpired    ClaimStatus = "expired"
	ClaimNeedsHuman ClaimStatus = "needs_human"
	ClaimDone       ClaimStatus = "done"
)

// Claim is a lease held by an agent for one role on one work item.
type Claim struct {
	ID             string      `json:"id"`
	WorkItemID     string      `json:"work_item_id"`
	AgentID        string      `json:"agent_id"`
	Role           Role        `json:"role"`
	LeaseEpoch     int64       `json:"lease_epoch"`
	LeaseExpiresAt int64       `json:"lease_expires_at"`
	HeartbeatAt    int64       `json:"heartbeat_at"`
	Forced         bool        `json:"forced"`
	Plan           string      `json:"plan"`
	Note           string      `json:"note,omitempty"`
	Status         ClaimStatus `json:"status"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	Evidence       *Evidence   `json:"evidence,omitempty"`
	// Model identity is recorded with the lease so scoring can attribute the
	// outcome to the model that actually did the work.
	ModelID      string `json:"model_id,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
	ToolMode     string `json:"tool_mode,omitempty"`
	TaskClass    string `json:"task_class,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// Claimant returns the identity recorded with this lease.
func (c Claim) Claimant() Claimant {
	return Claimant{
		AgentID:      c.AgentID,
		ModelID:      c.ModelID,
		ModelVersion: c.ModelVersion,
		ToolMode:     c.ToolMode,
		TaskClass:    c.TaskClass,
	}
}

// Claimant is who asks for a lease. The board stores the identity but never
// scores it; scoring happens in the service layer.
type Claimant struct {
	AgentID      string `json:"agent_id"`
	ModelID      string `json:"model_id,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
	ToolMode     string `json:"tool_mode,omitempty"`
	TaskClass    string `json:"task_class,omitempty"`
}

// Artifact is an evidence or output file produced while working an item.
type Artifact struct {
	ID         string `json:"id"`
	WorkItemID string `json:"work_item_id"`
	Kind       string `json:"kind"` // diff|patch|log|test|report|note
	URI        string `json:"uri"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Summary    string `json:"summary"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
}

// Review is one verifier decision with its evidence references and the checks
// the verifier actually re-ran in a clean workspace.
type Review struct {
	ID         string        `json:"id"`
	WorkItemID string        `json:"work_item_id"`
	ReviewerID string        `json:"reviewer_id"`
	Verdict    Verdict       `json:"verdict"`
	Reasons    string        `json:"reasons"`
	Evidence   []string      `json:"evidence,omitempty"`
	Checks     []CheckResult `json:"checks,omitempty"`
	CreatedAt  int64         `json:"created_at"`
}

// Event is one append-only work-board event.
type Event struct {
	ID         uint64 `json:"id"`
	WorkItemID string `json:"work_item_id"`
	Type       string `json:"type"`
	Actor      string `json:"actor"`
	Payload    []byte `json:"payload,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// Verdict is a verifier decision.
type Verdict string

const (
	VerdictApprove    Verdict = "approve"
	VerdictReject     Verdict = "reject"
	VerdictNeedsHuman Verdict = "needs_human"
)
