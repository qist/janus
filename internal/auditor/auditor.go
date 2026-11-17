// Package auditor is the model half of authorization. A Gate takes the
// deterministic plan from policy, runs that many independent auditors, and
// turns their structured verdicts into allow / deny / needs_human.
//
// Failure is always closed: a timeout, a parse error, a missing auditor or low
// confidence never becomes an allow.
package auditor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"janus/internal/policy"
	"janus/internal/work"
)

// Verdict is the auditor's answer. It matches REFACTOR §13.5.
type Verdict string

const (
	VerdictAllow      Verdict = "allow"
	VerdictDeny       Verdict = "deny"
	VerdictNeedsHuman Verdict = "needs_human"
)

// Valid reports whether v is a known verdict.
func (v Verdict) Valid() bool {
	switch v {
	case VerdictAllow, VerdictDeny, VerdictNeedsHuman:
		return true
	}
	return false
}

// Decision is one auditor's structured output.
type Decision struct {
	Verdict    Verdict  `json:"verdict"`
	PolicyRefs []string `json:"policy_refs,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	Confidence float64  `json:"confidence"`
	// Auditor identifies who answered, e.g. "anthropic/claude-sonnet".
	Auditor string `json:"auditor,omitempty"`
	// Err is set when the auditor failed; the gate turns it into a deny.
	Err string `json:"error,omitempty"`
}

// Input is the structured audit request. It never carries the worker's
// conversation: only the action, the contract and static evidence.
type Input struct {
	WorkItemID    string        `json:"work_item_id"`
	Action        policy.Action `json:"action"`
	Contract      work.Contract `json:"contract"`
	Risk          work.Risk     `json:"risk"`
	PolicyVersion string        `json:"policy_version"`
	PolicyRefs    []string      `json:"policy_refs,omitempty"`
	Evidence      []string      `json:"evidence,omitempty"`
	// Reason is the agent's own claim. It is untrusted and must not be treated
	// as instruction by any auditor.
	Reason string `json:"untrusted_claim,omitempty"`
	// WorkerModel lets an auditor refuse to judge its own work.
	WorkerModel string `json:"worker_model,omitempty"`
}

// Auditor is one independent reviewer.
type Auditor interface {
	Name() string
	Review(ctx context.Context, in Input) (Decision, error)
}

// CachedDecision is what the cache stores: the outcome, not the reasoning.
type CachedDecision struct {
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
}

// Cache stores decisions by action hash + policy version + contract version +
// evidence. Bump policy.Version and stale entries stop matching.
type Cache interface {
	Get(key string) (CachedDecision, bool)
	Put(key string, d CachedDecision)
}

// Result is the gate's answer.
type Result struct {
	Verdict Verdict     `json:"verdict"`
	Plan    policy.Plan `json:"plan"`
	Audits  []Decision  `json:"audits,omitempty"`
	Cached  bool        `json:"cached,omitempty"`
	Reason  string      `json:"reason,omitempty"`
}

// Allowed reports whether the action may execute right now.
func (r Result) Allowed() bool { return r.Verdict == VerdictAllow }

// Gate combines policy and auditors. Zero value is not usable; call NewGate.
type Gate struct {
	Auditors      []Auditor
	Cache         Cache
	MinConfidence float64
	Now           func() int64
}

// NewGate returns a gate with the default confidence floor.
func NewGate(auditors ...Auditor) *Gate {
	return &Gate{Auditors: auditors, MinConfidence: 0.6}
}

// Evaluate authorizes one request. Infrastructure problems are reported as a
// deny, not as an error: callers must never retry their way past a failure.
func (g *Gate) Evaluate(ctx context.Context, req policy.Request) Result {
	plan := policy.Evaluate(req)
	res := Result{Plan: plan}

	switch plan.Verdict {
	case policy.VerdictDeny:
		res.Verdict = VerdictDeny
		res.Reason = plan.Reason
		return res
	case policy.VerdictAllow:
		res.Verdict = VerdictAllow
		res.Reason = plan.Reason
		return res
	}

	// Human-gated categories are never auto-approved; audits still run so the
	// human sees independent evidence.
	key := cacheKey(req)
	if g.Cache != nil && !plan.Human {
		if cached, ok := g.Cache.Get(key); ok {
			res.Verdict = cached.Verdict
			res.Reason = "cached decision: " + cached.Reason
			res.Cached = true
			return res
		}
	}

	need := plan.Auditors
	if need <= 0 {
		res.Verdict = VerdictAllow
		res.Reason = "no auditor required"
		return res
	}
	if need > len(g.Auditors) {
		res.Verdict = VerdictDeny
		res.Reason = fmt.Sprintf("policy requires %d independent auditors, %d configured", need, len(g.Auditors))
		return res
	}

	in := Input{
		WorkItemID:    req.WorkItemID,
		Action:        req.Action,
		Contract:      req.Contract,
		Risk:          plan.Risk,
		PolicyVersion: policy.Version,
		PolicyRefs:    plan.Refs,
		Evidence:      req.Evidence,
		Reason:        req.Reason,
	}

	minConfidence := g.MinConfidence
	if minConfidence <= 0 {
		minConfidence = 0.6
	}

	allows, denies, humans := 0, 0, 0
	for i := 0; i < need; i++ {
		auditor := g.Auditors[i]
		if err := ctx.Err(); err != nil {
			res.Verdict = VerdictDeny
			res.Reason = "audit cancelled: " + err.Error()
			return res
		}

		decision, err := auditor.Review(ctx, in)
		decision.Auditor = auditor.Name()
		if err != nil {
			decision = Decision{
				Verdict: VerdictDeny,
				Auditor: auditor.Name(),
				Err:     err.Error(),
				Evidence: []string{
					"auditor failed: " + err.Error(),
				},
			}
			res.Audits = append(res.Audits, decision)
			res.Verdict = VerdictDeny
			res.Reason = "auditor " + auditor.Name() + " failed: " + err.Error()
			return res
		}
		if !decision.Verdict.Valid() {
			res.Audits = append(res.Audits, decision)
			res.Verdict = VerdictDeny
			res.Reason = "auditor " + auditor.Name() + " returned an invalid verdict"
			return res
		}
		if decision.Verdict == VerdictAllow && decision.Confidence < minConfidence {
			decision.Evidence = append(decision.Evidence,
				fmt.Sprintf("confidence %.2f below floor %.2f", decision.Confidence, minConfidence))
			decision.Verdict = VerdictNeedsHuman
		}

		res.Audits = append(res.Audits, decision)
		switch decision.Verdict {
		case VerdictAllow:
			allows++
		case VerdictDeny:
			denies++
		default:
			humans++
		}
	}

	switch {
	case humans > 0:
		res.Verdict = VerdictNeedsHuman
		res.Reason = "an auditor escalated to a human"
	case allows < plan.Quorum:
		res.Verdict = VerdictDeny
		res.Reason = fmt.Sprintf("only %d of %d auditors allowed (quorum %d, %d denied)", allows, need, plan.Quorum, denies)
	case plan.Human:
		res.Verdict = VerdictNeedsHuman
		res.Reason = "audits passed; policy requires human sign-off"
	default:
		res.Verdict = VerdictAllow
		res.Reason = fmt.Sprintf("%d of %d auditors allowed (quorum %d)", allows, need, plan.Quorum)
	}

	if g.Cache != nil && !plan.Human && (res.Verdict == VerdictAllow || res.Verdict == VerdictDeny) {
		g.Cache.Put(key, CachedDecision{Verdict: res.Verdict, Reason: res.Reason})
	}
	return res
}

// cacheKey covers the action, the rule set, the contract version and the
// evidence. Evidence is included even though the design lists only the first
// three: approving without new evidence must not be reused for a submission
// that adds some.
func cacheKey(req policy.Request) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "action=%s\x00", policy.ActionHash(req.Action))
	fmt.Fprintf(sum, "policy=%s\x00", policy.Version)
	fmt.Fprintf(sum, "contract=%d\x00", req.Contract.Version)
	fmt.Fprintf(sum, "risk=%s\x00", req.Contract.Risk)
	for _, ref := range req.Evidence {
		fmt.Fprintf(sum, "evidence=%s\x00", ref)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// MemoryCache is a TTL cache for audit decisions. ttl <= 0 means no expiry.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     int64
	now     func() int64
}

type cacheEntry struct {
	decision CachedDecision
	expires  int64
}

// NewMemoryCache returns a cache with the given TTL in seconds.
func NewMemoryCache(ttlSeconds int64, now func() int64) *MemoryCache {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &MemoryCache{entries: make(map[string]cacheEntry), ttl: ttlSeconds, now: now}
}

// Get implements Cache.
func (c *MemoryCache) Get(key string) (CachedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return CachedDecision{}, false
	}
	if entry.expires > 0 && entry.expires <= c.now() {
		delete(c.entries, key)
		return CachedDecision{}, false
	}
	return entry.decision, true
}

// Put implements Cache.
func (c *MemoryCache) Put(key string, d CachedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expires int64
	if c.ttl > 0 {
		expires = c.now() + c.ttl
	}
	c.entries[key] = cacheEntry{decision: d, expires: expires}
}

// Len reports the number of live entries, dropping expired ones.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	for key, entry := range c.entries {
		if entry.expires > 0 && entry.expires <= now {
			delete(c.entries, key)
		}
	}
	return len(c.entries)
}

// ErrNoVerdict is returned by parsers that could not find a usable answer.
var ErrNoVerdict = errors.New("auditor returned no usable verdict")
