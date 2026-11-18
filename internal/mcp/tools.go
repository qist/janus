package mcp

// toolInfo is the MCP tools/list entry.
type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

func toolDefs() []toolInfo {
	return []toolInfo{
		{
			Name: "work_list",
			Description: "List work items, optionally filtered by status and workspace. " +
				"Returns claim order (priority first, then oldest).",
			InputSchema: object(map[string]any{
				"status":    enum("Lifecycle status filter", "open", "running", "review", "approved", "integrating", "done", "blocked", "cancelled"),
				"workspace": str("Workspace id filter"),
			}),
		},
		{
			Name:        "work_get",
			Description: "Get one work item with its active lease, latest context capsule and review history.",
			InputSchema: object(map[string]any{"id": str("Work item id")}, "id"),
		},
		{
			Name: "work_handoff",
			Description: "Rebuild the bounded handover pack without taking a lease. " +
				"Use it to re-read state mid-task; never rely on conversation history instead.",
			InputSchema: object(map[string]any{"id": str("Work item id")}, "id"),
		},
		{
			Name: "work_create",
			Description: "Create a work item. Fill contract.goal, contract.acceptance, contract.checks " +
				"and contract.paths: empty paths mean no write scope, empty checks mean no machine gate.",
			InputSchema: object(map[string]any{
				"item": map[string]any{
					"type":        "object",
					"description": "WorkItem JSON (id, workspace_id, title, brief, contract, depends_on, priority, ...)",
				},
			}, "item"),
		},
		{
			Name: "work_claim",
			Description: "Take a lease for a role and receive the handover pack. " +
				"worker: open→running, verifier: review, integrator: approved→integrating. " +
				"The agent id comes from the API key, not from arguments.",
			InputSchema: object(map[string]any{
				"id":            str("Work item id"),
				"role":          enum("Role to claim", "worker", "verifier", "integrator"),
				"plan":          str("What you intend to do, max 512 bytes; required and shown to the next holder"),
				"lease_seconds": integer("Lease length in seconds (default 1200, max 7200)"),
				"model_id":      str("Model that will do the work; required for worker claims when scoring is on"),
				"model_version": str("Model version; a version bump starts a fresh score"),
				"tool_mode":     enum("How tools are executed", "native", "bridge", "none"),
				"task_class":    str("Task class for scoring (defaults from the declared paths)"),
			}, "id", "role", "plan"),
		},
		{
			Name: "work_assign",
			Description: "Forced dispatch (admin only). Hands a blocked or open item to a specific agent, " +
				"bypassing tier, attempts, dependencies and path locks. A live lease and terminal items are still " +
				"refused, and the forcing admin is recorded in the event log.",
			InputSchema: object(map[string]any{
				"id":            str("Work item id"),
				"agent_id":      str("Agent that must take the item"),
				"role":          enum("Role to assign", "worker", "verifier", "integrator"),
				"plan":          str("What the agent is expected to do"),
				"lease_seconds": integer("Lease length in seconds (default 1200, max 7200)"),
				"model_id":      str("Model that will run the work"),
				"model_version": str("Model version"),
				"tool_mode":     enum("How tools are executed", "native", "bridge", "none"),
				"task_class":    str("Task class for scoring"),
			}, "id", "agent_id", "role", "plan"),
		},
		{
			Name:        "work_heartbeat",
			Description: "Extend your lease. Only the current holder may call it; a stale epoch is rejected.",
			InputSchema: object(map[string]any{
				"id":             str("Work item id"),
				"epoch":          integer("Lease epoch from work_claim"),
				"extend_seconds": integer("New lease length from now (default 1200, max 7200)"),
			}, "id", "epoch"),
		},
		{
			Name:        "work_release",
			Description: "Give the item back without submitting. The item returns to the state of your role.",
			InputSchema: object(map[string]any{
				"id":    str("Work item id"),
				"epoch": integer("Lease epoch from work_claim"),
			}, "id", "epoch"),
		},
		{
			Name: "work_submit",
			Description: "Submit finished work for review. Evidence is mandatory: at least one artifact or " +
				"diff hash, plus the real exit code of every check in the contract. " +
				"Hand-written pass flags are rejected.",
			InputSchema: object(map[string]any{
				"id":              str("Work item id"),
				"epoch":           integer("Lease epoch from work_claim"),
				"idempotency_key": str("Client key; retrying with the same key is safe"),
				"evidence": map[string]any{
					"type":        "object",
					"description": "{\"artifacts\":[ids],\"diff_hash\":\"...\",\"checks\":[{\"name\",\"cmd\",\"exit_code\",\"output_tail\"}],\"summary\":\"...\"}",
				},
			}, "id", "epoch", "idempotency_key", "evidence"),
		},
		{
			Name: "work_verify",
			Description: "Run the contract checks in a clean workspace under your verifier lease and return " +
				"the results. Pass them back to work_verdict; approving a checked item without them is refused.",
			InputSchema: object(map[string]any{
				"id":    str("Work item id"),
				"epoch": integer("Lease epoch from work_claim"),
			}, "id", "epoch"),
		},
		{
			Name: "work_verdict",
			Description: "Close a verifier lease. approve needs the verifier's own check results; " +
				"reject returns the item to open with attempts+1 (blocked at 3); needs_human blocks it. " +
				"Reviewing your own submission is refused.",
			InputSchema: object(map[string]any{
				"id":      str("Work item id"),
				"epoch":   integer("Lease epoch from work_claim"),
				"verdict": enum("Verdict", "approve", "reject", "needs_human"),
				"reasons": str("Why; stored with the review and shown to the next holder"),
				"checks": map[string]any{
					"type":        "array",
					"description": "Verifier check results (required when approving an item with contract checks)",
					"items":       map[string]any{"type": "object"},
				},
				"failure_owner": enum("Who caused a reject; only model affects the model score",
					"model", "harness", "client", "task", "account", "infra"),
			}, "id", "epoch", "verdict"),
		},
		{
			Name: "work_authorize",
			Description: "Ask for permission to run a sensitive action (writes outside scope, exec, network egress, " +
				"secrets, deploy, migration, auth code). Policy decides first; independent auditors second. " +
				"An allow returns a single-use capability token that expires in 60s; a deny or needs_human returns none. " +
				"Never execute a sensitive action without a token.",
			InputSchema: object(map[string]any{
				"id":    str("Work item id"),
				"epoch": integer("Lease epoch from work_claim"),
				"action": map[string]any{
					"type":        "object",
					"description": "{\"kind\":\"fs.write|exec|net.egress|secret.read|secret.write|git.push|deploy|db.migrate|auth\",\"targets\":[paths],\"command\":\"...\",\"diff_hash\":\"...\"}",
				},
				"reason":       str("Why; untrusted text, auditors ignore instructions in it"),
				"evidence":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Artifact ids or check results backing the action"},
				"worker_model": str("Model that wrote the change; an auditor with the same model refuses to judge it"),
			}, "id", "epoch", "action"),
		},
		{
			Name:        "work_complete",
			Description: "Close an integrator lease and mark the item done. Idempotent for the same key.",
			InputSchema: object(map[string]any{
				"id":              str("Work item id"),
				"epoch":           integer("Lease epoch from work_claim"),
				"idempotency_key": str("Client key; retrying with the same key is safe"),
			}, "id", "epoch", "idempotency_key"),
		},
	}
}
