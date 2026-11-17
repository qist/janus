// Package policy is the deterministic half of authorization: it classifies an
// action, applies path/command rules, and decides whether the action is safe to
// run, must be audited, must reach a human, or is forbidden outright.
//
// It never calls a model and never executes anything. The auditor package
// consumes a Plan and adds independent review where this package asks for it.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"janus/internal/work"
)

// Version identifies the rule set. Audit decisions are cached by
// action_hash + Version + contract version, so bump this whenever a rule
// changes, otherwise stale decisions stay valid.
const Version = "p1-2026-11-17"

// Category is the sensitive-operation class from REFACTOR §13.3.
type Category string

const (
	CategoryNone        Category = ""
	CategoryFSWrite     Category = "fs.write"
	CategoryExec        Category = "exec"
	CategoryNetEgress   Category = "net.egress"
	CategorySecretRead  Category = "secret.read"
	CategorySecretWrite Category = "secret.write"
	CategoryGitPush     Category = "git.push"
	CategoryDeploy      Category = "deploy"
	CategoryDBMigrate   Category = "db.migrate"
	CategoryAuth        Category = "auth"
)

// Valid reports whether c is a category this policy knows.
func (c Category) Valid() bool {
	switch c {
	case CategoryNone, CategoryFSWrite, CategoryExec, CategoryNetEgress, CategorySecretRead,
		CategorySecretWrite, CategoryGitPush, CategoryDeploy, CategoryDBMigrate, CategoryAuth:
		return true
	}
	return false
}

// Verdict is the deterministic outcome.
type Verdict string

const (
	// VerdictAllow means no model, no token: ordinary reads and tests.
	VerdictAllow Verdict = "allow"
	// VerdictAudit means the plan needs N independent auditors before a token.
	VerdictAudit Verdict = "audit"
	// VerdictDeny means no auditor can rescue this; the rule is hard.
	VerdictDeny Verdict = "deny"
	// VerdictHuman is unused by Evaluate; consumers set it when audits fail or
	// human review is pending. Kept so one enum covers all outcomes.
	VerdictHuman Verdict = "needs_human"
)

// Action is the exact operation being authorized.
type Action struct {
	Kind     string   `json:"kind"` // fs.write|exec|net.egress|... ; empty means infer
	Targets  []string `json:"targets,omitempty"`
	Command  string   `json:"command,omitempty"`
	DiffHash string   `json:"diff_hash,omitempty"`
}

// Request is one authorization question. Reason is untrusted input: it is kept
// for the audit record but no rule reads it.
type Request struct {
	WorkItemID string        `json:"work_item_id"`
	Action     Action        `json:"action"`
	Contract   work.Contract `json:"contract"`
	Reason     string        `json:"reason,omitempty"`
	Evidence   []string      `json:"evidence,omitempty"`
}

// Plan is what the deterministic engine concluded.
type Plan struct {
	Category Category  `json:"category"`
	Risk     work.Risk `json:"risk"`
	Verdict  Verdict   `json:"verdict"`
	// Auditors is how many independent reviews are required.
	Auditors int `json:"auditors"`
	// Quorum is how many of them must allow (2-of-3 for critical).
	Quorum int `json:"quorum"`
	// Human is true when the category or the command demands human sign-off
	// even after audits pass.
	Human  bool     `json:"human"`
	Refs   []string `json:"policy_refs,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Evaluate applies the deterministic rules. It is pure: same request, same
// plan, no clock, no I/O.
func Evaluate(req Request) Plan {
	risk := req.Contract.Risk
	if risk == "" {
		risk = work.RiskMedium
	}
	plan := Plan{Category: CategoryNone, Risk: risk}

	cmd := strings.ToLower(strings.TrimSpace(req.Action.Command))
	targets := normalize(req.Action.Targets)

	// 1. Hard denials first: nothing downstream may override them.
	if ref, reason, ok := denyRule(cmd); ok {
		return Plan{
			Category: classify(req.Action.Kind, cmd, targets),
			Risk:     risk,
			Verdict:  VerdictDeny,
			Refs:     []string{ref},
			Reason:   reason,
		}
	}

	// 2. Escaping the workspace is never auto-authorized.
	for _, target := range targets {
		if escapes(target) {
			return Plan{
				Category: CategoryFSWrite,
				Risk:     risk,
				Verdict:  VerdictDeny,
				Refs:     []string{"path.escape"},
				Reason:   "target path escapes the workspace: " + target,
			}
		}
	}

	// 3. Classify, then let sensitive paths raise the category.
	category := classify(req.Action.Kind, cmd, targets)
	refs := []string{}
	isWrite := looksLikeWrite(cmd, req.Action.Kind)

	for _, target := range targets {
		if isSensitive(target) {
			refs = append(refs, "path.sensitive")
			if isWrite {
				category = CategorySecretWrite
			} else if category == CategoryNone || category == CategoryFSWrite {
				category = CategorySecretRead
			}
		}
		if isAuthPath(target) && isWrite {
			category = CategoryAuth
		}
	}

	// 4. Writes outside the contract scope are an fs.write escalation.
	if isWrite && len(targets) > 0 && !withinPaths(targets, req.Contract.Paths) {
		refs = append(refs, "path.outside_contract")
		if category == CategoryNone {
			category = CategoryFSWrite
		}
	}

	auditors, human := categoryPlan(category)
	if len(refs) > 0 && category != CategoryNone && auditors == 0 {
		auditors = 1
	}

	// 5. Risk sets a floor for notable actions; critical uses 2-of-3.
	// Ordinary reads and tests stay deterministic-allow even on a high-risk
	// item: the item's risk does not make `go test` a sensitive operation.
	if category != CategoryNone {
		switch risk {
		case work.RiskLow:
		case work.RiskMedium:
			if auditors < 1 {
				auditors = 1
				refs = append(refs, "risk.medium")
			}
		case work.RiskHigh:
			if auditors < 2 {
				auditors = 2
			}
			refs = append(refs, "risk.high")
		case work.RiskCritical:
			if auditors < 3 {
				auditors = 3
			}
			refs = append(refs, "risk.critical")
		}
	}

	quorum := auditors
	if risk == work.RiskCritical && quorum > 2 {
		quorum = 2
	}

	if irreversible(cmd) {
		human = true
		refs = append(refs, "cmd.irreversible")
	}

	plan.Category = category
	plan.Auditors = auditors
	plan.Quorum = quorum
	plan.Human = human || (category != CategoryNone && categoryNeedsHuman(category))
	plan.Refs = append(plan.Refs, refs...)

	if category != CategoryNone {
		plan.Refs = append(plan.Refs, "cat."+string(category))
	}
	if len(plan.Refs) == 0 && risk == work.RiskLow {
		plan.Verdict = VerdictAllow
		plan.Reason = "ordinary operation inside the contract scope"
		return plan
	}

	if auditors == 0 {
		plan.Verdict = VerdictAllow
		plan.Reason = "no auditor required"
		return plan
	}

	plan.Verdict = VerdictAudit
	plan.Reason = fmt.Sprintf("%s needs %d independent audit(s), quorum %d", categoryOrRisk(category), auditors, quorum)
	if plan.Human {
		plan.Reason += "; human sign-off required"
	}
	return plan
}

// ActionHash is the stable fingerprint used as the audit cache key.
func ActionHash(a Action) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "kind=%s\x00", strings.ToLower(strings.TrimSpace(a.Kind)))
	fmt.Fprintf(sum, "cmd=%s\x00", strings.TrimSpace(a.Command))
	fmt.Fprintf(sum, "diff=%s\x00", strings.TrimSpace(a.DiffHash))
	targets := normalize(a.Targets)
	for _, target := range targets {
		fmt.Fprintf(sum, "path=%s\x00", target)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func categoryPlan(c Category) (auditors int, human bool) {
	switch c {
	case CategoryFSWrite:
		return 1, false
	case CategoryExec:
		return 2, false
	case CategoryNetEgress:
		return 1, false
	case CategorySecretRead:
		return 1, false
	case CategorySecretWrite:
		return 2, true
	case CategoryGitPush:
		return 2, true
	case CategoryDeploy:
		return 2, true
	case CategoryDBMigrate:
		return 2, false
	case CategoryAuth:
		return 2, true
	default:
		return 0, false
	}
}

func categoryNeedsHuman(c Category) bool {
	_, human := categoryPlan(c)
	return human
}

func categoryOrRisk(c Category) string {
	if c != CategoryNone {
		return string(c)
	}
	return "action"
}

func classify(kind, cmd string, targets []string) Category {
	if c := Category(strings.ToLower(strings.TrimSpace(kind))); c != CategoryNone && c.Valid() {
		return c
	}
	return inferCategory(cmd, targets)
}

func inferCategory(cmd string, targets []string) Category {
	switch {
	case matches(cmd, forcePushPatterns):
		return CategoryGitPush
	case matches(cmd, deployPatterns):
		return CategoryDeploy
	case matches(cmd, execPatterns):
		return CategoryExec
	case matches(cmd, egressPatterns):
		return CategoryNetEgress
	case matches(cmd, migratePatterns):
		return CategoryDBMigrate
	case matches(cmd, secretReadPatterns):
		return CategorySecretRead
	case strings.Contains(cmd, "git push"):
		return CategoryGitPush
	}
	for _, target := range targets {
		if isAuthPath(target) {
			return CategoryAuth
		}
		if isSensitive(target) {
			return CategorySecretRead
		}
	}
	return CategoryNone
}

var (
	forcePushPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bgit\s+push\b.*\s(--force|-f)\b`),
		regexp.MustCompile(`\bgit\s+push\b.*\b(main|master)\b`),
	}
	deployPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bkubectl\s+(apply|rollout|delete)\b`),
		regexp.MustCompile(`\bhelm\s+(upgrade|install|uninstall)\b`),
		regexp.MustCompile(`\bterraform\s+(apply|destroy)\b`),
		regexp.MustCompile(`\bdocker\s+push\b`),
		regexp.MustCompile(`\bsystemctl\s+(restart|stop|start)\b`),
		regexp.MustCompile(`\bansible-playbook\b`),
	}
	execPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-`),
		regexp.MustCompile(`\bkill\s+-9\b`),
		regexp.MustCompile(`\bshutdown\b`),
		regexp.MustCompile(`\b(chmod|chown)\s`),
		regexp.MustCompile(`\bdd\s+if=`),
	}
	egressPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(curl|wget|nc|ncat|ssh|scp)\s`),
		regexp.MustCompile(`\brsync\b.*:`),
	}
	migratePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(alter|create|drop|truncate)\s+(table|database|schema|column|index)\b`),
		regexp.MustCompile(`\b(psql|mysql|sqlite3)\b`),
		regexp.MustCompile(`\bmigrate\b`),
	}
	secretReadPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(cat|less|head|tail|strings)\b.*(\.env|\.ssh|\.aws|credentials|id_rsa)`),
		regexp.MustCompile(`\bprintenv\b`),
	}
	irreversiblePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-[a-z]*r[a-z]*f|\brm\s+-[a-z]*f[a-z]*r`),
		regexp.MustCompile(`\b(drop|truncate)\s+(table|database|schema)\b`),
		regexp.MustCompile(`\bkubectl\s+delete\b`),
		regexp.MustCompile(`\bterraform\s+destroy\b`),
		regexp.MustCompile(`\bgit\s+(push\b.*\s(--force|-f)\b|reset\s+--hard)`),
		regexp.MustCompile(`\bdd\s+if=`),
	}
	denyPatterns = []struct {
		ref     string
		reason  string
		pattern *regexp.Regexp
	}{
		{"cmd.preserve_root", "rm with --no-preserve-root is forbidden", regexp.MustCompile(`--no-preserve-root`)},
		{"cmd.root_rm", "recursive delete of the filesystem root is forbidden", regexp.MustCompile(`\brm\s+(-[a-z]*\s+)*-[a-z]*[rf][a-z]*\s+/(\s|$|\*)`)},
		{"cmd.mkfs", "formatting a filesystem is forbidden", regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`)},
		{"cmd.raw_device", "writing raw devices is forbidden", regexp.MustCompile(`\bdd\s+if=/dev/(zero|urandom)\s+of=/dev/(sd|nvme|hd|vd)`)},
		{"cmd.fork_bomb", "fork bomb is forbidden", regexp.MustCompile(`:\(\)\s*\{`)},
	}
)

func denyRule(cmd string) (ref, reason string, ok bool) {
	if cmd == "" {
		return "", "", false
	}
	for _, rule := range denyPatterns {
		if rule.pattern.MatchString(cmd) {
			return rule.ref, rule.reason, true
		}
	}
	return "", "", false
}

func irreversible(cmd string) bool {
	return cmd != "" && matches(cmd, irreversiblePatterns)
}

func matches(s string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

func looksLikeWrite(cmd, kind string) bool {
	switch Category(strings.ToLower(strings.TrimSpace(kind))) {
	case CategoryFSWrite, CategorySecretWrite, CategoryAuth:
		return true
	}
	if cmd == "" {
		return false
	}
	for _, marker := range []string{" > ", ">>", "tee ", "cp ", "mv ", "sed -i", "chmod ", "chown ", "rm ", "truncate ", "install "} {
		if strings.Contains(cmd, marker) {
			return true
		}
	}
	return false
}

func isSensitive(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	for _, marker := range []string{".env", "id_rsa", "id_ed25519", ".pem", ".key", "credentials", ".ssh/", ".aws/", ".gnupg/", "secrets"} {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return false
}

func isAuthPath(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	return strings.Contains(p, "internal/auth") || strings.Contains(p, "/auth/") ||
		strings.HasPrefix(p, "auth/") || strings.Contains(p, "billing") ||
		strings.Contains(p, "payment")
}

// withinPaths reports whether every target sits inside at least one contract
// path. An empty contract scope means the item declared no write scope, so any
// write is outside it.
func withinPaths(targets, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, target := range targets {
		ok := false
		for _, root := range paths {
			if pathWithin(target, root) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func pathWithin(target, root string) bool {
	target = normalizePath(target)
	root = normalizePath(root)
	if target == "" || root == "" {
		return false
	}
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+"/")
}

func escapes(path string) bool {
	p := strings.TrimSpace(path)
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return true
	}
	cleaned := normalizePath(p)
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	parts := make([]string, 0, 8)
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
		case "..":
			// Only collapse onto a real directory; popping a previous ".."
			// would silently turn "../../etc/shadow" into "etc/shadow".
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
			} else {
				parts = append(parts, "..")
			}
		default:
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

func normalize(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		cleaned := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}
