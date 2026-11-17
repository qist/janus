package policy

import (
	"testing"

	"janus/internal/work"
)

func contract(paths ...string) work.Contract {
	return work.Contract{Goal: "ship", Paths: paths, Risk: work.RiskMedium}
}

func hasRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func TestOrdinaryOperationsAreDeterministicAllow(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{"go test", Request{Action: Action{Command: "go test ./..."}, Contract: contract("internal/work")}},
		{"go build", Request{Action: Action{Command: "go build ./..."}, Contract: contract("internal/work")}},
		{"read source", Request{Action: Action{Kind: "fs.read", Targets: []string{"internal/work/board.go"}}, Contract: contract("internal/work")}},
		// A high-risk item still does not make running tests a sensitive act.
		{"test on high risk", Request{Action: Action{Command: "go test ./..."}, Contract: work.Contract{Risk: work.RiskHigh, Paths: []string{"internal/work"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Evaluate(tc.req)
			if plan.Verdict != VerdictAllow {
				t.Fatalf("verdict = %s, want allow (refs %v)", plan.Verdict, plan.Refs)
			}
			if plan.Auditors != 0 {
				t.Fatalf("auditors = %d, want 0", plan.Auditors)
			}
			if plan.Human {
				t.Fatal("human sign-off requested for an ordinary operation")
			}
		})
	}
}

func TestHardDenials(t *testing.T) {
	cases := []struct{ name, cmd, ref string }{
		{"root rm", "rm -rf /", "cmd.root_rm"},
		{"no preserve root", "rm -rf --no-preserve-root /", "cmd.preserve_root"},
		{"mkfs", "mkfs.ext4 /dev/sdb1", "cmd.mkfs"},
		{"raw device", "dd if=/dev/zero of=/dev/sda bs=1M", "cmd.raw_device"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Evaluate(Request{Action: Action{Command: tc.cmd}, Contract: contract()})
			if plan.Verdict != VerdictDeny {
				t.Fatalf("verdict = %s, want deny", plan.Verdict)
			}
			if !hasRef(plan.Refs, tc.ref) {
				t.Fatalf("refs = %v, want %s", plan.Refs, tc.ref)
			}
		})
	}
}

func TestEscapeIsDenied(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "../../etc/shadow", "~/secrets"} {
		plan := Evaluate(Request{
			Action:   Action{Kind: "fs.write", Targets: []string{target}},
			Contract: contract("internal/work"),
		})
		if plan.Verdict != VerdictDeny || !hasRef(plan.Refs, "path.escape") {
			t.Fatalf("target %q: verdict = %s refs = %v", target, plan.Verdict, plan.Refs)
		}
	}
}

func TestCategoryMatrix(t *testing.T) {
	cases := []struct {
		name     string
		req      Request
		auditors int
		quorum   int
		human    bool
		ref      string
	}{
		{
			name:     "exec",
			req:      Request{Action: Action{Command: "rm -rf internal/work/tmp"}, Contract: contract("internal/work")},
			auditors: 2, quorum: 2, human: true, ref: "cat.exec",
		},
		{
			name:     "network egress",
			req:      Request{Action: Action{Command: "curl https://example.com/hook"}, Contract: contract()},
			auditors: 1, quorum: 1, ref: "cat.net.egress",
		},
		{
			name:     "secret read",
			req:      Request{Action: Action{Command: "cat .env"}, Contract: contract()},
			auditors: 1, quorum: 1, ref: "cat.secret.read",
		},
		{
			name:     "secret write",
			req:      Request{Action: Action{Kind: "fs.write", Targets: []string{".env"}}, Contract: contract(".env")},
			auditors: 2, quorum: 2, human: true, ref: "cat.secret.write",
		},
		{
			name:     "force push",
			req:      Request{Action: Action{Command: "git push --force origin main"}, Contract: contract()},
			auditors: 2, quorum: 2, human: true, ref: "cat.git.push",
		},
		{
			name:     "deploy",
			req:      Request{Action: Action{Command: "helm upgrade janus ./deploy"}, Contract: contract()},
			auditors: 2, quorum: 2, human: true, ref: "cat.deploy",
		},
		{
			name:     "migration",
			req:      Request{Action: Action{Command: "psql -c 'alter table work_items add column x int'"}, Contract: contract()},
			auditors: 2, quorum: 2, human: false, ref: "cat.db.migrate",
		},
		{
			name:     "auth code",
			req:      Request{Action: Action{Kind: "fs.write", Targets: []string{"internal/auth/capability.go"}}, Contract: contract("internal/auth")},
			auditors: 2, quorum: 2, human: true, ref: "cat.auth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Evaluate(tc.req)
			if plan.Verdict != VerdictAudit {
				t.Fatalf("verdict = %s, want audit (refs %v)", plan.Verdict, plan.Refs)
			}
			if plan.Auditors != tc.auditors || plan.Quorum != tc.quorum {
				t.Fatalf("auditors/quorum = %d/%d, want %d/%d", plan.Auditors, plan.Quorum, tc.auditors, tc.quorum)
			}
			if plan.Human != tc.human {
				t.Fatalf("human = %v, want %v", plan.Human, tc.human)
			}
			if !hasRef(plan.Refs, tc.ref) {
				t.Fatalf("refs = %v, want %s", plan.Refs, tc.ref)
			}
		})
	}
}

func TestWriteOutsideContractEscalates(t *testing.T) {
	plan := Evaluate(Request{
		Action:   Action{Kind: "fs.write", Targets: []string{"internal/api/server.go"}},
		Contract: contract("internal/work"),
	})
	if plan.Verdict != VerdictAudit {
		t.Fatalf("verdict = %s, want audit", plan.Verdict)
	}
	if plan.Category != CategoryFSWrite || plan.Auditors != 1 {
		t.Fatalf("category/auditors = %s/%d", plan.Category, plan.Auditors)
	}
	if !hasRef(plan.Refs, "path.outside_contract") {
		t.Fatalf("refs = %v", plan.Refs)
	}
	if plan.Human {
		t.Fatal("outside-contract write should not force a human by itself")
	}
}

func TestWriteInsideContractIsStillAudited(t *testing.T) {
	plan := Evaluate(Request{
		Action:   Action{Kind: "fs.write", Targets: []string{"internal/work/board.go"}},
		Contract: contract("internal/work"),
	})
	if plan.Category != CategoryFSWrite || plan.Auditors != 1 {
		t.Fatalf("category/auditors = %s/%d", plan.Category, plan.Auditors)
	}
	if hasRef(plan.Refs, "path.outside_contract") {
		t.Fatalf("refs = %v, should not include outside_contract", plan.Refs)
	}
}

func TestCriticalUsesTwoOfThree(t *testing.T) {
	plan := Evaluate(Request{
		Action:   Action{Kind: "fs.write", Targets: []string{"internal/work/board.go"}},
		Contract: work.Contract{Risk: work.RiskCritical, Paths: []string{"internal/work"}},
	})
	if plan.Verdict != VerdictAudit {
		t.Fatalf("verdict = %s", plan.Verdict)
	}
	if plan.Auditors != 3 || plan.Quorum != 2 {
		t.Fatalf("auditors/quorum = %d/%d, want 3/2", plan.Auditors, plan.Quorum)
	}
	if !hasRef(plan.Refs, "risk.critical") {
		t.Fatalf("refs = %v", plan.Refs)
	}
}

func TestHighRiskRaisesAuditorCount(t *testing.T) {
	plan := Evaluate(Request{
		Action:   Action{Kind: "fs.write", Targets: []string{"internal/work/x.go"}},
		Contract: work.Contract{Risk: work.RiskHigh, Paths: []string{"internal/work"}},
	})
	if plan.Auditors != 2 || plan.Quorum != 2 {
		t.Fatalf("auditors/quorum = %d/%d, want 2/2", plan.Auditors, plan.Quorum)
	}
}

func TestActionHashStability(t *testing.T) {
	a := Action{Kind: "exec", Command: "go test ./...", Targets: []string{"internal/work", "internal/work"}}
	b := Action{Kind: "exec", Command: "go test ./...", Targets: []string{"internal/work"}}
	if ActionHash(a) != ActionHash(b) {
		t.Fatal("duplicate targets changed the hash")
	}
	c := Action{Kind: "exec", Command: "go test ./...", Targets: []string{"internal/api"}}
	if ActionHash(a) == ActionHash(c) {
		t.Fatal("different targets produced the same hash")
	}
	d := Action{Kind: "exec", Command: "go build ./...", Targets: []string{"internal/work"}}
	if ActionHash(a) == ActionHash(d) {
		t.Fatal("different commands produced the same hash")
	}
}

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("policy version must be non-empty")
	}
}

func TestCategoryValid(t *testing.T) {
	for _, c := range []Category{CategoryNone, CategoryFSWrite, CategoryExec, CategoryAuth} {
		if !c.Valid() {
			t.Fatalf("%q should be valid", c)
		}
	}
	if Category("nope").Valid() {
		t.Fatal("unknown category reported valid")
	}
}
