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
  failed?: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  duration_ms?: number;
};

export type UsageGroup = {
  name: string;
  entries: number;
  failed: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  duration_ms?: number;
};

export type UsageBucket = {
  start: number;
  entries: number;
  failed: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
};

export type UsageEntry = {
  id: string;
  key_id?: string;
  provider_id: string;
  credential_id?: string;
  model: string;
  upstream_model?: string;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens?: number;
  cost_usd?: number;
  status?: number;
  error_code?: string;
  stream?: boolean;
  duration_ms?: number;
  created_at: number;
};

export type UsageStats = {
  totals: UsageTotals;
  by_model: UsageGroup[];
  by_provider: UsageGroup[];
  by_key: UsageGroup[];
  trend: UsageBucket[];
  bucket: "hour" | "day" | string;
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

export type Me = {
  agent_id: string;
  scopes: string[];
};

export type ProviderKind = "openai" | "anthropic" | "custom";
export type CredentialKind = "api_key" | "oauth";
export type CredentialStatus = "unknown" | "ok" | "exhausted" | "rate_limited" | "invalid";

export type OAuthConfig = {
  client_id: string;
  authorize_url: string;
  token_url: string;
  redirect_uri: string;
  scopes?: string[];
  extra_auth_params?: Record<string, string>;
  token_style?: "form" | "json";
  verifier_encoding?: "hex" | "base64url";
  verifier_bytes?: number;
};

export type Provider = {
  id: string;
  name: string;
  kind: ProviderKind;
  base_url: string;
  channel?: string;
  protocol?: "chat" | "responses" | "anthropic";
  oauth?: OAuthConfig;
  enabled: boolean;
  created_at: number;
  updated_at: number;
};

export type Channel = {
  id: string;
  name: string;
  category: string;
  kind: ProviderKind;
  protocols: string[];
  auth: string[];
  base_url: string;
  alt_base_urls?: string[];
  note?: string;
};

export type SyncResult = {
  provider_id: string;
  credential_id?: string;
  masked?: string;
  total: number;
  created: string[];
  existing: string[];
  conflicts: string[];
  skipped: string[];
  stale: string[];
  pruned: string[];
  error?: string;
};

export type QuotaWindow = {
  name: string;
  used_percent?: number;
  used?: number;
  limit?: number;
  unit?: string;
  resets_at?: number;
  text?: string;
};

export type Quota = {
  balance?: string;
  currency?: string;
  plan?: string;
  windows?: QuotaWindow[];
  source?: string;
  checked_at: number;
  error?: string;
};

export type QuotaResult = {
  provider_id: string;
  credential_id: string;
  masked?: string;
  quota: Quota;
};

export type Credential = {
  id: string;
  provider_id: string;
  label?: string;
  account?: string;
  plan?: string;
  kind: CredentialKind;
  status: CredentialStatus;
  masked?: string;
  has_secret: boolean;
  priority: number;
  disabled?: boolean;
  remaining?: number;
  total?: number;
  expires_at?: number;
  last_used_at?: number;
  cooldown_until?: number;
  resets_at?: number;
  last_error?: string;
  quota?: Quota;
  created_at: number;
  updated_at: number;
};

export type Route = {
  id: string;
  exposed_model: string;
  provider_id: string;
  upstream_model: string;
  aliases?: string[];
  priority: number;
  enabled: boolean;
  created_at: number;
  updated_at: number;
};

export type ProbeResult = {
  ok: boolean;
  provider_id: string;
  base_url: string;
  credential_id?: string;
  masked?: string;
  latency_ms: number;
  model_count: number;
  models?: string[];
  status: CredentialStatus | "";
  error?: string;
};

export type OAuthStart = {
  session_id: string;
  auth_url: string;
  redirect_uri: string;
  expires_in: number;
};
