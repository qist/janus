package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	srv   *httptest.Server
	board *work.Board
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
	return &testEnv{srv: ts, board: board}
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
