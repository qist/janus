export type Verdict = "approve" | "reject" | "needs_human";
export type Role = "worker" | "verifier" | "integrator";

export type CheckResult = {
  name: string;
  cmd: string;
  exit_code: number;
  duration_ms?: number;
  output_sha256?: string;
  output_tail?: string;
};

export type Evidence = {
  artifacts?: string[];
  diff_hash?: string;
  checks?: CheckResult[];
  summary?: string;
};

export type Claim = {
  id: string;
  work_item_id: string;
  agent_id: string;
  role: Role;
  lease_epoch: number;
  lease_expires_at: number;
  heartbeat_at: number;
  forced?: boolean;
  plan?: string;
  note?: string;
  status: string;
  model_id?: string;
  model_version?: string;
  tool_mode?: string;
  task_class?: string;
  evidence?: Evidence;
  created_at: number;
};

export type Review = {
  id: string;
  work_item_id: string;
  reviewer_id: string;
  verdict: Verdict;
  reasons?: string;
  evidence?: string[];
  checks?: CheckResult[];
  created_at: number;
};

export type WorkItem = {
  id: string;
  workspace_id: string;
  title: string;
  brief?: string;
  contract: {
    goal?: string;
    acceptance?: string[];
    checks?: { name: string; cmd: string; timeout?: number; expect_exit?: number }[];
    risk?: string;
    paths?: string[];
    budget_tokens?: number;
    autonomy?: string;
  };
  depends_on?: string[];
  priority?: number;
  status: string;
  attempts?: number;
  lease_expiries?: number;
  created_by?: string;
  version?: number;
  created_at?: number;
  updated_at?: number;
};

export type ItemView = {
  item: WorkItem;
  claim?: Claim;
  capsule?: unknown;
  reviews?: Review[];
  warnings?: string[];
};

export type ClaimResult = {
  claim: Claim;
  pack?: { text: string; tokens: number; budget: number; omitted?: string[] };
  capsule?: unknown;
  warnings?: string[];
};

export type Goal = {
  id: string;
  title: string;
  brief?: string;
  workspace_id: string;
  group_id?: string;
  budget_tokens?: number;
  risk?: string;
  status: string;
  item_ids?: string[];
  created_by?: string;
  error?: string;
  created_at: number;
  updated_at: number;
};

export type GoalProgress = {
  goal_id: string;
  status: string;
  total: number;
  done: number;
  blocked: number;
  counts?: Record<string, number>;
  percent: number;
  error?: string;
  updated_at: number;
};

export type UsageTotals = {
  entries: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
};

export type Score = {
  key: {
    agent_id: string;
    model_id: string;
    task_class: string;
    tool_mode: string;
    model_version: string;
  };
  tier: string;
  score: number;
  samples: number;
  approvals: number;
  first_pass: number;
  reject_streak: number;
  claims: number;
  lease_expiries: number;
  cost_median?: number;
  cycle_median?: number;
  pinned?: boolean;
  suspended?: boolean;
};

export type ModelInfo = {
  id: string;
  object: string;
  owned_by?: string;
};
