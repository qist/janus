// Package authz turns an authorization question into an executable decision:
// deterministic policy first, independent audits second, and a single-use
// capability token only when the action may run.
//
// It is the seam between "the model says it is fine" and "an executor may do
// it": the model never approves anything, and the executor never trusts words.
package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"janus/internal/auditor"
	"janus/internal/auth"
	"janus/internal/policy"
	"janus/internal/work"
)

// DefaultTTL is how long an issued capability token stays valid.
const DefaultTTL int64 = 60

// Request is one authorization question, bound to a lease.
type Request struct {
	WorkItemID  string        `json:"work_item_id"`
	LeaseEpoch  int64         `json:"lease_epoch"`
	AgentID     string        `json:"agent_id"`
	Action      policy.Action `json:"action"`
	Contract    work.Contract `json:"contract"`
	Reason      string        `json:"reason,omitempty"`
	Evidence    []string      `json:"evidence,omitempty"`
	WorkerModel string        `json:"worker_model,omitempty"`
}

// Outcome is the answer. Token is empty unless Verdict is allow.
type Outcome struct {
	Verdict   auditor.Verdict    `json:"verdict"`
	Plan      policy.Plan        `json:"plan"`
	Audits    []auditor.Decision `json:"audits,omitempty"`
	Token     string             `json:"token,omitempty"`
	ExpiresAt int64              `json:"expires_at,omitempty"`
	Cached    bool               `json:"cached,omitempty"`
	Reason    string             `json:"reason,omitempty"`
}

// Service wires policy, the auditor gate and the capability authority.
type Service struct {
	Gate      *auditor.Gate
	Authority *auth.Authority
	// TTL overrides DefaultTTL when positive; auth.MaxTTL still applies.
	TTL int64
	Now func() int64
}

// New returns a service. A nil gate means every sensitive action is denied.
func New(gate *auditor.Gate, authority *auth.Authority) *Service {
	return &Service{Gate: gate, Authority: authority, TTL: DefaultTTL}
}

// Authorize evaluates the request and, on allow for a sensitive action, issues
// a capability token. Allow for ordinary actions carries no token: nothing
// sensitive is being authorized.
func (s *Service) Authorize(ctx context.Context, req Request) (Outcome, error) {
	if strings.TrimSpace(req.WorkItemID) == "" {
		return Outcome{}, errors.New("work item id required")
	}

	gate := s.Gate
	if gate == nil {
		gate = &auditor.Gate{}
	}

	result := gate.Evaluate(ctx, policy.Request{
		WorkItemID: req.WorkItemID,
		Action:     req.Action,
		Contract:   req.Contract,
		Reason:     req.Reason,
		Evidence:   req.Evidence,
	})

	out := Outcome{
		Verdict: result.Verdict,
		Plan:    result.Plan,
		Audits:  result.Audits,
		Cached:  result.Cached,
		Reason:  result.Reason,
	}
	if result.Verdict != auditor.VerdictAllow {
		return out, nil
	}

	// Ordinary actions need no token; sensitive ones are bound to a lease.
	if result.Plan.Verdict == policy.VerdictAllow && result.Plan.Auditors == 0 {
		return out, nil
	}
	if req.LeaseEpoch <= 0 {
		out.Verdict = auditor.VerdictDeny
		out.Reason = "sensitive actions require a work item lease epoch"
		return out, nil
	}
	if s.Authority == nil {
		out.Verdict = auditor.VerdictDeny
		out.Reason = "capability authority is not configured"
		return out, nil
	}

	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > auth.MaxTTL {
		ttl = auth.MaxTTL
	}

	now := s.now()
	claims := auth.Claims{
		WorkItemID: req.WorkItemID,
		LeaseEpoch: req.LeaseEpoch,
		ActionHash: auth.HashAction(CapabilityAction(req.Action)),
		Paths:      append([]string(nil), req.Action.Targets...),
		ExpiresAt:  now + ttl,
	}

	token, err := s.Authority.Issue(claims)
	if err != nil {
		return Outcome{}, fmt.Errorf("issue capability token: %w", err)
	}

	out.Token = token
	out.ExpiresAt = now + ttl
	return out, nil
}

// CapabilityAction converts a policy action into the shape the executor hashes
// when it verifies the token.
func CapabilityAction(a policy.Action) auth.Action {
	return auth.Action{
		Kind:     a.Kind,
		Targets:  append([]string(nil), a.Targets...),
		Command:  a.Command,
		DiffHash: a.DiffHash,
	}
}

func (s *Service) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	// The authority stamps and validates tokens with its own clock; deriving
	// expiries from a different clock would make tokens born expired.
	if s.Authority != nil {
		return s.Authority.Now()
	}
	return time.Now().Unix()
}
