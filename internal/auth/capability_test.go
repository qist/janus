package auth

import (
	"errors"
	"testing"
)

func testAuthority(t *testing.T) (*Authority, *int64) {
	t.Helper()
	now := int64(1_700_000_000)
	a, err := NewAuthority([]byte("0123456789abcdef0123456789abcdef"), func() int64 { return now })
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	return a, &now
}

func testAction() Action {
	return Action{
		Kind:     "git.push",
		Targets:  []string{"main"},
		Command:  "git push --force origin main",
		DiffHash: "abc123",
	}
}

func testClaims() Claims {
	return Claims{
		WorkItemID: "w1",
		LeaseEpoch: 1,
		ActionHash: HashAction(testAction()),
		Paths:      []string{"/repo"},
	}
}

func TestCapabilityRoundTrip(t *testing.T) {
	a, _ := testAuthority(t)
	claims := testClaims()

	token, err := a.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}

	got, err := a.Verify(token, testAction())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.WorkItemID != claims.WorkItemID || got.LeaseEpoch != claims.LeaseEpoch {
		t.Fatalf("claims = %+v", got)
	}
	if got.Nonce == "" || got.ExpiresAt <= got.IssuedAt {
		t.Fatalf("token metadata = %+v", got)
	}
}

func TestCapabilityExpires(t *testing.T) {
	a, now := testAuthority(t)
	token, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	*now += DefaultTTL + 1
	if _, err := a.Verify(token, testAction()); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify = %v, want ErrExpired", err)
	}
}

func TestCapabilityReplay(t *testing.T) {
	a, _ := testAuthority(t)
	token, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := a.Verify(token, testAction()); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if _, err := a.Verify(token, testAction()); !errors.Is(err, ErrReplay) {
		t.Fatalf("second Verify = %v, want ErrReplay", err)
	}
}

func TestCapabilityTampered(t *testing.T) {
	a, _ := testAuthority(t)
	token, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	tampered := token[:len(token)-1] + "A"
	if tampered == token {
		tampered = token[:len(token)-1] + "B"
	}
	if _, err := a.Verify(tampered, testAction()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestCapabilityActionMismatch(t *testing.T) {
	a, _ := testAuthority(t)
	token, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	changed := testAction()
	changed.Command = "git push origin main"
	if _, err := a.Verify(token, changed); !errors.Is(err, ErrActionMismatch) {
		t.Fatalf("Verify = %v, want ErrActionMismatch", err)
	}
}

func TestCapabilityTTLTooLong(t *testing.T) {
	a, now := testAuthority(t)
	claims := testClaims()
	claims.ExpiresAt = *now + MaxTTL + 1

	if _, err := a.Issue(claims); !errors.Is(err, ErrTTLTooLong) {
		t.Fatalf("Issue = %v, want ErrTTLTooLong", err)
	}
}

func TestCapabilityWeakKey(t *testing.T) {
	if _, err := NewAuthority([]byte("short"), nil); err == nil {
		t.Fatal("NewAuthority accepted a short key")
	}
}

func TestCapabilityNonceUnique(t *testing.T) {
	a, _ := testAuthority(t)
	t1, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue 1: %v", err)
	}
	t2, err := a.Issue(testClaims())
	if err != nil {
		t.Fatalf("Issue 2: %v", err)
	}
	if t1 == t2 {
		t.Fatal("two Issue calls returned the same token")
	}
	if _, err := a.Verify(t1, testAction()); err != nil {
		t.Fatalf("Verify 1: %v", err)
	}
	if _, err := a.Verify(t2, testAction()); err != nil {
		t.Fatalf("Verify 2: %v", err)
	}
}

func TestActionHashStable(t *testing.T) {
	a := testAction()
	b := testAction()
	b.Targets = []string{"main"}

	if HashAction(a) != HashAction(b) {
		t.Fatal("action hash is not stable")
	}
	c := testAction()
	c.Targets = []string{"main", "release"}
	if HashAction(a) == HashAction(c) {
		t.Fatal("action hash ignored targets")
	}
}

func TestPathAllowed(t *testing.T) {
	cases := []struct {
		name     string
		prefixes []string
		target   string
		want     bool
	}{
		{"inside", []string{"/repo"}, "/repo/a", true},
		{"equal", []string{"/repo"}, "/repo", true},
		{"sibling", []string{"/repo"}, "/repo-evil/a", false},
		{"dotdot", []string{"/repo"}, "/repo/../etc/passwd", false},
		{"second prefix", []string{"/repo", "/tmp"}, "/tmp/x", true},
		{"empty", nil, "/repo/a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathAllowed(tc.prefixes, tc.target); got != tc.want {
				t.Fatalf("PathAllowed(%v, %q) = %v, want %v", tc.prefixes, tc.target, got, tc.want)
			}
		})
	}
}
