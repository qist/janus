package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"janus/internal/auditor"
	"janus/internal/auth"
	"janus/internal/authz"
	"janus/internal/policy"
	"janus/internal/scoring"
	"janus/internal/usage"
	"janus/internal/work"
	"janus/internal/worksvc"
)

const (
	adminKey   = "admin-key-for-tests"
	workerKey  = "worker-key-for-tests"
	selfKey    = "self-review-key-for-tests"
	otherKey   = "other-agent-key-for-tests"
	readerKey  = "reader-key-for-tests"
	adminAgent = "agent-admin"
)

type testEnv struct {
	srv    *httptest.Server
	board  *work.Board
	server *Server
	svc    *worksvc.Service
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	now := int64(1_700_000_000)
	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	svc := &worksvc.Service{Board: board, Now: func() int64 { return now }}

	keys := NewStaticKeys()
	keys.Add(adminKey, Identity{AgentID: adminAgent, Scopes: []Scope{ScopeAdmin}})
	keys.Add(workerKey, Identity{AgentID: "agent-worker", Scopes: []Scope{ScopeRead, ScopeClaim, ScopeSubmit}})
	// Same agent as the worker, but this key may verify: it proves the board,
	// not the scope check, is what blocks self-review.
	keys.Add(selfKey, Identity{AgentID: "agent-worker", Scopes: []Scope{ScopeRead, ScopeClaim, ScopeVerify}})
	keys.Add(otherKey, Identity{AgentID: "agent-other", Scopes: []Scope{ScopeRead, ScopeClaim, ScopeSubmit, ScopeVerify}})
	keys.Add(readerKey, Identity{AgentID: "agent-reader", Scopes: []Scope{ScopeRead}})

	server := New(svc, keys)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: ts, board: board, server: server, svc: svc}
}

func (e *testEnv) do(t *testing.T, method, path, key string, body any) *http.Response {
	t.Helper()

	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set(headerKey, key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (e *testEnv) expect(t *testing.T, method, path, key string, body any, want int) *http.Response {
	t.Helper()
	resp := e.do(t, method, path, key, body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d (%s)", method, path, resp.StatusCode, want, readBody(t, resp))
	}
	return resp
}

// doRaw sends a body verbatim so malformed JSON can be tested.
func (e *testEnv) doRaw(t *testing.T, method, path, key, raw string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader([]byte(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set(headerKey, key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf.String()
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body errorResponse
	decodeBody(t, resp, &body)
	return body.Error.Code
}

func createItem(t *testing.T, e *testEnv, item work.WorkItem) {
	t.Helper()
	e.expect(t, http.MethodPost, "/api/v1/work-items", adminKey, createRequest{WorkItem: item}, http.StatusCreated)
}

func plainItem(id string) work.WorkItem {
	return work.WorkItem{ID: id, WorkspaceID: "ws", Title: id}
}

func TestHealthzNeedsNoKey(t *testing.T) {
	e := newTestEnv(t)
	resp := e.expect(t, http.MethodGet, "/api/v1/healthz", "", nil, http.StatusOK)
	var body map[string]string
	decodeBody(t, resp, &body)
	if body["status"] != "ok" {
		t.Fatalf("body = %+v", body)
	}
}

func TestMissingOrUnknownKeyIsUnauthorized(t *testing.T) {
	e := newTestEnv(t)

	for _, key := range []string{"", "nope"} {
		resp := e.expect(t, http.MethodGet, "/api/v1/work-items", key, nil, http.StatusUnauthorized)
		if code := errCode(t, resp); code != "unauthenticated" {
			t.Fatalf("key %q code = %q", key, code)
		}
	}
}

func TestScopeIsEnforced(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", readerKey,
		claimRequest{Role: work.RoleWorker, Plan: "x"}, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", code)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items", readerKey,
		createRequest{WorkItem: plainItem("w2")}, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", code)
	}
}

func TestFullLifecycle(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "fix claim race",
		Brief:       "two agents claim at once",
		Contract:    work.Contract{Goal: "one claim wins", Paths: []string{"internal/work"}},
	})

	// Worker claims: the agent id comes from the key, not the body.
	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement", LeaseSeconds: 60}, http.StatusOK)
	var claimed worksvc.ClaimResult
	decodeBody(t, resp, &claimed)
	if claimed.Claim.AgentID != "agent-worker" || claimed.Claim.LeaseEpoch != 1 {
		t.Fatalf("claim = %+v", claimed.Claim)
	}
	if claimed.Pack.Text == "" {
		t.Fatal("claim did not return a handoff pack")
	}

	// A second claim on the same item conflicts.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", otherKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusConflict)
	if code := errCode(t, resp); code != "already_claimed" {
		t.Fatalf("code = %q, want already_claimed", code)
	}

	// Submit without evidence is rejected.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/submit", workerKey,
		submitRequest{Epoch: 1, IdempotencyKey: "k1"}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "evidence_missing" {
		t.Fatalf("code = %q, want evidence_missing", code)
	}

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/submit", workerKey, submitRequest{
		Epoch:          1,
		IdempotencyKey: "k1",
		Evidence:       work.Evidence{Artifacts: []string{"artifact:diff"}, DiffHash: "sha-diff"},
	}, http.StatusNoContent)

	// Verifier must be a different agent; agent-other does that.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", otherKey,
		claimRequest{Role: work.RoleVerifier, Plan: "review", LeaseSeconds: 60}, http.StatusOK)
	var verified worksvc.ClaimResult
	decodeBody(t, resp, &verified)
	if verified.Claim.Role != work.RoleVerifier {
		t.Fatalf("role = %q", verified.Claim.Role)
	}

	// A verify-scoped key that does not hold the lease cannot post a verdict.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", selfKey,
		verdictRequest{Epoch: verified.Claim.LeaseEpoch, Verdict: work.VerdictApprove}, http.StatusForbidden)
	if code := errCode(t, resp); code != "not_lease_holder" {
		t.Fatalf("code = %q, want not_lease_holder", code)
	}

	// A key without the verify scope cannot post a verdict at all.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", workerKey,
		verdictRequest{Epoch: verified.Claim.LeaseEpoch, Verdict: work.VerdictApprove}, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", code)
	}

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", otherKey,
		verdictRequest{Epoch: verified.Claim.LeaseEpoch, Verdict: work.VerdictApprove, Reasons: "looks fine"},
		http.StatusNoContent)

	// Integrator scope is admin-only in this test setup; use admin key.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleIntegrator, Plan: "merge", LeaseSeconds: 60}, http.StatusOK)
	var integrating worksvc.ClaimResult
	decodeBody(t, resp, &integrating)

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/complete", adminKey,
		completeRequest{Epoch: integrating.Claim.LeaseEpoch, IdempotencyKey: "done-1"}, http.StatusNoContent)

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items/w1", readerKey, nil, http.StatusOK)
	var view worksvc.ItemView
	decodeBody(t, resp, &view)
	if view.Item.Status != work.StatusDone {
		t.Fatalf("status = %s, want done", view.Item.Status)
	}
	if len(view.Reviews) != 1 || view.Reviews[0].Verdict != work.VerdictApprove {
		t.Fatalf("reviews = %+v", view.Reviews)
	}
	if view.Claim != nil {
		t.Fatalf("claim = %+v, want none after complete", view.Claim)
	}
}

func TestListFiltersAndShape(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))
	createItem(t, e, work.WorkItem{ID: "w2", WorkspaceID: "other", Title: "w2"})

	resp := e.expect(t, http.MethodGet, "/api/v1/work-items", readerKey, nil, http.StatusOK)
	var all struct {
		Items []work.WorkItem `json:"items"`
	}
	decodeBody(t, resp, &all)
	if len(all.Items) != 2 {
		t.Fatalf("items = %+v, want 2", all.Items)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items?status=open", readerKey, nil, http.StatusOK)
	decodeBody(t, resp, &all)
	if len(all.Items) != 2 {
		t.Fatalf("open items = %d, want 2", len(all.Items))
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items?workspace=other", readerKey, nil, http.StatusOK)
	decodeBody(t, resp, &all)
	if len(all.Items) != 1 || all.Items[0].ID != "w2" {
		t.Fatalf("workspace filter = %+v", all.Items)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items?status=bogus", readerKey, nil, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_status" {
		t.Fatalf("code = %q", code)
	}

	// Empty result must be [] not null.
	resp = e.expect(t, http.MethodGet, "/api/v1/work-items?workspace=none", readerKey, nil, http.StatusOK)
	raw := readBody(t, resp)
	if !bytes.Contains([]byte(raw), []byte(`"items":[]`)) {
		t.Fatalf("empty list body = %s", raw)
	}
}

func TestNotFoundAndUnknownEndpoint(t *testing.T) {
	e := newTestEnv(t)

	resp := e.expect(t, http.MethodGet, "/api/v1/work-items/missing", readerKey, nil, http.StatusNotFound)
	if code := errCode(t, resp); code != "not_found" {
		t.Fatalf("code = %q", code)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/nope", readerKey, nil, http.StatusNotFound)
	if code := errCode(t, resp); code != "not_found" {
		t.Fatalf("code = %q", code)
	}
}

func TestInvalidJSONAndUnknownFields(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.doRaw(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey, `{"role":"worker","agent_id":"hacker"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "invalid_json" {
		t.Fatalf("code = %q", code)
	}

	resp = e.doRaw(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey, `{"role":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

func TestInvalidRoleAndVerdict(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: "wizard", Plan: "x"}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_role" {
		t.Fatalf("code = %q", code)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", selfKey,
		verdictRequest{Epoch: 1, Verdict: "maybe"}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_verdict" {
		t.Fatalf("code = %q", code)
	}
}

func TestHeartbeatReleaseAndHandoff(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement", LeaseSeconds: 30}, http.StatusOK)
	var claimed worksvc.ClaimResult
	decodeBody(t, resp, &claimed)

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/heartbeat", workerKey,
		heartbeatRequest{Epoch: claimed.Claim.LeaseEpoch, ExtendSeconds: 30}, http.StatusNoContent)

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items/w1/handoff", readerKey, nil, http.StatusOK)
	var handoff struct {
		Pack     struct{ Text string } `json:"pack"`
		Warnings []string              `json:"warnings"`
	}
	decodeBody(t, resp, &handoff)
	if handoff.Pack.Text == "" {
		t.Fatal("handoff pack is empty")
	}

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/release", workerKey,
		epochRequest{Epoch: claimed.Claim.LeaseEpoch}, http.StatusNoContent)

	resp = e.expect(t, http.MethodGet, "/api/v1/work-items/w1", readerKey, nil, http.StatusOK)
	var view worksvc.ItemView
	decodeBody(t, resp, &view)
	if view.Item.Status != work.StatusOpen {
		t.Fatalf("status = %s, want open", view.Item.Status)
	}
}

func TestVerifyWithoutRunnerIsNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verify", selfKey,
		epochRequest{Epoch: 1}, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "runner_unavailable" {
		t.Fatalf("code = %q", code)
	}
}

type stubRunner struct {
	results []work.CheckResult
}

func (s *stubRunner) Run(context.Context, []work.Check) ([]work.CheckResult, error) {
	return s.results, nil
}

func TestVerifyRunsConfiguredRunner(t *testing.T) {
	now := int64(1_700_000_000)
	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	svc := &worksvc.Service{Board: board, Now: func() int64 { return now }}

	keys := NewStaticKeys()
	keys.Add(adminKey, Identity{AgentID: adminAgent, Scopes: []Scope{ScopeAdmin}})
	keys.Add(otherKey, Identity{AgentID: "agent-other", Scopes: []Scope{ScopeAdmin}})

	server := New(svc, keys)
	server.RunnerFor = func(context.Context, work.WorkItem) (work.CheckRunner, error) {
		return &stubRunner{results: []work.CheckResult{{Name: "unit", ExitCode: 0}}}, nil
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	e := &testEnv{srv: ts, board: board}

	createItem(t, e, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		Contract: work.Contract{
			Goal:   "ship",
			Checks: []work.Check{{Name: "unit", Command: "go test ./...", TimeoutSeconds: 60}},
		},
	})

	// Worker scope is admin here; claim as worker, submit, then verify as verifier.
	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)
	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/submit", adminKey, submitRequest{
		Epoch:          1,
		IdempotencyKey: "k1",
		Evidence: work.Evidence{
			Artifacts: []string{"a1"},
			Checks:    []work.CheckResult{{Name: "unit", ExitCode: 0}},
		},
	}, http.StatusNoContent)

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", otherKey,
		claimRequest{Role: work.RoleVerifier, Plan: "review"}, http.StatusOK)
	var verified worksvc.ClaimResult
	decodeBody(t, resp, &verified)

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verify", otherKey,
		epochRequest{Epoch: verified.Claim.LeaseEpoch}, http.StatusOK)
	var body struct {
		Checks []work.CheckResult `json:"checks"`
	}
	decodeBody(t, resp, &body)
	if len(body.Checks) != 1 || body.Checks[0].Name != "unit" {
		t.Fatalf("checks = %+v", body.Checks)
	}
}

func TestCreateRequiresID(t *testing.T) {
	e := newTestEnv(t)
	resp := e.expect(t, http.MethodPost, "/api/v1/work-items", adminKey,
		createRequest{WorkItem: work.WorkItem{Title: "no id"}}, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_request" {
		t.Fatalf("code = %q", code)
	}
}

func TestDuplicateCreateConflicts(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items", adminKey,
		createRequest{WorkItem: plainItem("w1")}, http.StatusConflict)
	if code := errCode(t, resp); code != "already_exists" {
		t.Fatalf("code = %q", code)
	}
}

func TestSelfReviewIsRefused(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	// agent-worker submits, then claims the verifier lease with the same
	// identity through a verify-scoped key.
	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)
	var claimed worksvc.ClaimResult
	decodeBody(t, resp, &claimed)
	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/submit", workerKey, submitRequest{
		Epoch:          claimed.Claim.LeaseEpoch,
		IdempotencyKey: "k1",
		Evidence:       work.Evidence{Artifacts: []string{"a1"}},
	}, http.StatusNoContent)

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", selfKey,
		claimRequest{Role: work.RoleVerifier, Plan: "review"}, http.StatusOK)
	var verified worksvc.ClaimResult
	decodeBody(t, resp, &verified)

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", selfKey,
		verdictRequest{Epoch: verified.Claim.LeaseEpoch, Verdict: work.VerdictApprove}, http.StatusForbidden)
	if code := errCode(t, resp); code != "self_review" {
		t.Fatalf("code = %q, want self_review", code)
	}
}

func TestResponsesUseSnakeCaseKeys(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"claim", "pack"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing key %q in %v", key, keysOf(raw))
		}
	}
	var pack map[string]json.RawMessage
	if err := json.Unmarshal(raw["pack"], &pack); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	for _, key := range []string{"text", "tokens", "budget"} {
		if _, ok := pack[key]; !ok {
			t.Fatalf("missing pack key %q in %v", key, keysOf(pack))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStaticKeysRejectEmptyAndCount(t *testing.T) {
	keys := NewStaticKeys()
	keys.Add("", Identity{AgentID: "ignored"})
	if keys.Len() != 0 {
		t.Fatalf("len = %d, want 0", keys.Len())
	}
	keys.Add("k1", Identity{AgentID: "a"})
	if keys.Len() != 1 {
		t.Fatalf("len = %d, want 1", keys.Len())
	}
	if _, err := keys.Authenticate(context.Background(), "k1"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if _, err := keys.Authenticate(context.Background(), "k2"); err == nil {
		t.Fatal("unknown key accepted")
	}
}

type allowAllAuditor struct{}

func (allowAllAuditor) Name() string { return "test/allow" }

func (allowAllAuditor) Review(context.Context, auditor.Input) (auditor.Decision, error) {
	return auditor.Decision{Verdict: auditor.VerdictAllow, Confidence: 0.95}, nil
}

func newAuthzEnv(t *testing.T) (*testEnv, *auth.Authority) {
	t.Helper()

	now := int64(1_700_000_000)
	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	svc := &worksvc.Service{Board: board, Now: func() int64 { return now }}

	authority, err := auth.NewAuthority([]byte("0123456789abcdef0123456789abcdef"), func() int64 { return now })
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	svc.Authz = authz.New(auditor.NewGate(allowAllAuditor{}), authority)

	keys := NewStaticKeys()
	keys.Add(adminKey, Identity{AgentID: adminAgent, Scopes: []Scope{ScopeAdmin}})
	keys.Add("authz-other-key", Identity{AgentID: "agent-other", Scopes: []Scope{ScopeAuthorize, ScopeRead}})

	server := New(svc, keys)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: ts, board: board, server: server, svc: svc}, authority
}

func TestAuthorizeIssuesToken(t *testing.T) {
	e, authority := newAuthzEnv(t)
	createItem(t, e, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		Contract:    work.Contract{Goal: "ship", Paths: []string{"internal/work"}},
	})

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)
	var claimed worksvc.ClaimResult
	decodeBody(t, resp, &claimed)

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/authorize", adminKey, authorizeRequest{
		Epoch:  claimed.Claim.LeaseEpoch,
		Action: policy.Action{Kind: "fs.write", Targets: []string{"internal/work/x.go"}},
		Reason: "add a helper",
	}, http.StatusOK)

	var out authz.Outcome
	decodeBody(t, resp, &out)
	if out.Verdict != auditor.VerdictAllow || out.Token == "" {
		t.Fatalf("outcome = %+v", out)
	}
	if out.ExpiresAt == 0 {
		t.Fatal("expires_at not set")
	}

	claims, err := authority.Verify(out.Token, authz.CapabilityAction(policy.Action{
		Kind: "fs.write", Targets: []string{"internal/work/x.go"},
	}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.WorkItemID != "w1" || claims.LeaseEpoch != claimed.Claim.LeaseEpoch {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestAuthorizeDeniesDangerousCommand(t *testing.T) {
	e, _ := newAuthzEnv(t)
	createItem(t, e, plainItem("w1"))

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/authorize", adminKey, authorizeRequest{
		Epoch:  1,
		Action: policy.Action{Command: "rm -rf /"},
	}, http.StatusOK)

	var out authz.Outcome
	decodeBody(t, resp, &out)
	if out.Verdict != auditor.VerdictDeny || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestAuthorizeRequiresScopeAndLease(t *testing.T) {
	e, _ := newAuthzEnv(t)
	createItem(t, e, plainItem("w1"))
	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)

	// Right scope, wrong holder.
	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/authorize", "authz-other-key", authorizeRequest{
		Epoch:  1,
		Action: policy.Action{Kind: "fs.write", Targets: []string{"x.go"}},
	}, http.StatusForbidden)
	if code := errCode(t, resp); code != "not_lease_holder" {
		t.Fatalf("code = %q", code)
	}

	// Empty action is rejected before policy runs.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/authorize", adminKey, authorizeRequest{
		Epoch: 1,
	}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_action" {
		t.Fatalf("code = %q", code)
	}
}

func TestAuthorizeWithoutServiceIsNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	createItem(t, e, plainItem("w1"))
	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", adminKey,
		claimRequest{Role: work.RoleWorker, Plan: "implement"}, http.StatusOK)

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/authorize", adminKey, authorizeRequest{
		Epoch:  1,
		Action: policy.Action{Kind: "fs.write", Targets: []string{"x.go"}},
	}, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "authz_unavailable" {
		t.Fatalf("code = %q", code)
	}
}

func TestUsageEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ledger := usage.NewMemoryLedger(func() int64 { return 1000 })
	for _, entry := range []usage.Entry{
		{Model: "m1", ProviderID: "p1", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.01, CreatedAt: 100},
		{Model: "m2", ProviderID: "p2", PromptTokens: 20, CompletionTokens: 10, CostUSD: 0.02, CreatedAt: 200},
	} {
		if err := ledger.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	e.server.Usage = ledger

	resp := e.expect(t, http.MethodGet, "/api/v1/usage", readerKey, nil, http.StatusOK)
	var totals usage.Totals
	decodeBody(t, resp, &totals)
	if totals.Entries != 2 || totals.Tokens() != 45 || totals.CostUSD != 0.03 {
		t.Fatalf("totals = %+v", totals)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/usage?provider=p2", readerKey, nil, http.StatusOK)
	decodeBody(t, resp, &totals)
	if totals.Entries != 1 || totals.PromptTokens != 20 {
		t.Fatalf("filtered totals = %+v", totals)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/usage?since=nope", readerKey, nil, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_time" {
		t.Fatalf("code = %q", code)
	}
}

func TestUsageWithoutLedgerIsNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	resp := e.expect(t, http.MethodGet, "/api/v1/usage", readerKey, nil, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "usage_unavailable" {
		t.Fatalf("code = %q", code)
	}
}

func TestClaimTierGate(t *testing.T) {
	e := newTestEnv(t)
	engine := scoring.New(scoring.Config{Now: func() int64 { return 1_700_000_000 }})
	e.svc.Scores = engine
	e.server.Scores = engine

	createItem(t, e, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "medium",
		Contract:    work.Contract{Goal: "ship", Risk: work.RiskMedium, Paths: []string{"internal/work"}},
	})

	// Scoring on: a worker claim must name its model.
	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "p"}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "model_required" {
		t.Fatalf("code = %q", code)
	}

	// A new key is probation, which cannot take medium risk.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "p", ModelID: "m1", ModelVersion: "v1"}, http.StatusForbidden)
	if code := errCode(t, resp); code != "tier_denied" {
		t.Fatalf("code = %q", code)
	}

	// Low risk is allowed for probation.
	createItem(t, e, work.WorkItem{
		ID:          "w2",
		WorkspaceID: "ws",
		Title:       "low",
		Contract:    work.Contract{Goal: "ship", Risk: work.RiskLow, Paths: []string{"internal/work"}},
	})
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w2/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "p", ModelID: "m1", ModelVersion: "v1", ToolMode: "native", TaskClass: "default"},
		http.StatusOK)
	var claimed worksvc.ClaimResult
	decodeBody(t, resp, &claimed)
	if claimed.Claim.ModelID != "m1" || claimed.Claim.TaskClass != "default" {
		t.Fatalf("claim = %+v", claimed.Claim)
	}
}

func TestVerdictRequiresFailureOwner(t *testing.T) {
	e := newTestEnv(t)
	engine := scoring.New(scoring.Config{Now: func() int64 { return 1_700_000_000 }})
	e.svc.Scores = engine
	e.server.Scores = engine

	createItem(t, e, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "low",
		Contract:    work.Contract{Goal: "ship", Risk: work.RiskLow, Paths: []string{"internal/work"}},
	})

	resp := e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", workerKey,
		claimRequest{Role: work.RoleWorker, Plan: "p", ModelID: "m1"}, http.StatusOK)
	var worker worksvc.ClaimResult
	decodeBody(t, resp, &worker)

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/submit", workerKey, submitRequest{
		Epoch:          worker.Claim.LeaseEpoch,
		IdempotencyKey: "k1",
		Evidence:       work.Evidence{Artifacts: []string{"a1"}},
	}, http.StatusNoContent)

	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/claim", otherKey,
		claimRequest{Role: work.RoleVerifier, Plan: "review"}, http.StatusOK)
	var verifier worksvc.ClaimResult
	decodeBody(t, resp, &verifier)

	// A reject without attribution is refused before the board is touched.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", otherKey,
		verdictRequest{Epoch: verifier.Claim.LeaseEpoch, Verdict: work.VerdictReject, Reasons: "bad"},
		http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "failure_owner_required" {
		t.Fatalf("code = %q", code)
	}

	// An unknown owner is rejected by the transport.
	resp = e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", otherKey,
		verdictRequest{Epoch: verifier.Claim.LeaseEpoch, Verdict: work.VerdictReject, FailureOwner: "someone"},
		http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_owner" {
		t.Fatalf("code = %q", code)
	}

	e.expect(t, http.MethodPost, "/api/v1/work-items/w1/verdict", otherKey,
		verdictRequest{Epoch: verifier.Claim.LeaseEpoch, Verdict: work.VerdictReject, Reasons: "harness timeout", FailureOwner: "harness"},
		http.StatusNoContent)

	// Harness failure must not change the model score.
	score, ok := engine.Get(worksvc.ScoreKey(work.Claimant{AgentID: "agent-worker", ModelID: "m1"}, work.WorkItem{}))
	if !ok {
		t.Fatal("no score recorded")
	}
	if score.Samples != 0 {
		t.Fatalf("score = %+v", score)
	}
}

func TestScoresEndpoint(t *testing.T) {
	e := newTestEnv(t)
	engine := scoring.New(scoring.Config{Now: func() int64 { return 1000 }})
	key := scoring.Key{AgentID: "agent-1", ModelID: "m1", TaskClass: "default", ToolMode: "native", ModelVersion: "v1"}
	if _, err := engine.Record(scoring.Event{Key: key, Type: scoring.EventVerdict, Verdict: work.VerdictApprove, Attempt: 1}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	e.server.Scores = engine

	resp := e.expect(t, http.MethodGet, "/api/v1/scores", readerKey, nil, http.StatusOK)
	var body struct {
		Scores []scoring.Score `json:"scores"`
	}
	decodeBody(t, resp, &body)
	if len(body.Scores) != 1 || body.Scores[0].Key.ModelID != "m1" {
		t.Fatalf("scores = %+v", body.Scores)
	}
	if body.Scores[0].Samples != 1 || body.Scores[0].Tier != scoring.TierProbation {
		t.Fatalf("score = %+v", body.Scores[0])
	}
}

func TestScoresWithoutEngineIsNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	resp := e.expect(t, http.MethodGet, "/api/v1/scores", readerKey, nil, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "scores_unavailable" {
		t.Fatalf("code = %q", code)
	}
}
