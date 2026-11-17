package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/auth"
	"janus/internal/work"
	"janus/internal/worksvc"
)

const (
	adminKey  = "mcp-admin-key"
	workerKey = "mcp-worker-key"
	otherKey  = "mcp-other-key"
	readerKey = "mcp-reader-key"
)

type env struct {
	srv   *httptest.Server
	board *work.Board
}

func newEnv(t *testing.T, withRunner bool) *env {
	t.Helper()

	now := int64(1_700_000_000)
	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	svc := &worksvc.Service{Board: board, Now: func() int64 { return now }}

	keys := auth.NewStaticKeys()
	keys.Add(adminKey, auth.Identity{AgentID: "agent-admin", Scopes: []auth.Scope{auth.ScopeAdmin}})
	keys.Add(workerKey, auth.Identity{AgentID: "agent-worker", Scopes: []auth.Scope{auth.ScopeRead, auth.ScopeClaim, auth.ScopeSubmit}})
	keys.Add(otherKey, auth.Identity{AgentID: "agent-other", Scopes: []auth.Scope{auth.ScopeRead, auth.ScopeClaim, auth.ScopeVerify}})
	keys.Add(readerKey, auth.Identity{AgentID: "agent-reader", Scopes: []auth.Scope{auth.ScopeRead}})

	server := New(svc, keys)
	if withRunner {
		server.RunnerFor = func(context.Context, work.WorkItem) (work.CheckRunner, error) {
			return stubRunner{}, nil
		}
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &env{srv: ts, board: board}
}

type stubRunner struct{}

func (stubRunner) Run(_ context.Context, checks []work.Check) ([]work.CheckResult, error) {
	out := make([]work.CheckResult, 0, len(checks))
	for _, check := range checks {
		out = append(out, work.CheckResult{Name: check.Name, Command: check.Command, ExitCode: check.ExpectExit})
	}
	return out, nil
}

type rpcResult struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) rpc(t *testing.T, token, method string, params any) rpcResult {
	t.Helper()
	return e.rpcAt(t, "/mcp/"+token, method, params)
}

func (e *env) rpcAt(t *testing.T, path, method string, params any) rpcResult {
	t.Helper()

	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		payload["params"] = params
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := e.srv.Client().Post(e.srv.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	var out rpcResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (e *env) call(t *testing.T, token, name string, args any) callResult {
	t.Helper()
	res := e.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if res.Error != nil {
		t.Fatalf("%s: protocol error %d %s", name, res.Error.Code, res.Error.Message)
	}
	var out callResult
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("%s: decode result: %v", name, err)
	}
	if len(out.Content) == 0 {
		t.Fatalf("%s: empty content", name)
	}
	return out
}

func (c callResult) text() string { return c.Content[0].Text }

func (c callResult) mustFail(t *testing.T, code string) {
	t.Helper()
	if !c.IsError {
		t.Fatalf("expected failure %q, got %s", code, c.text())
	}
	if !strings.Contains(c.text(), code) {
		t.Fatalf("error text = %s, want code %q", c.text(), code)
	}
}

func TestInitialize(t *testing.T) {
	e := newEnv(t, false)

	res := e.rpc(t, adminKey, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	if res.Error != nil {
		t.Fatalf("error: %+v", res.Error)
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res.Result, &init); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if init.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocolVersion = %q", init.ProtocolVersion)
	}
	if init.ServerInfo.Name != "janus" || init.ServerInfo.Version == "" {
		t.Fatalf("serverInfo = %+v", init.ServerInfo)
	}
}

func TestUnauthenticated(t *testing.T) {
	e := newEnv(t, false)

	res := e.rpc(t, "bogus", "tools/list", nil)
	if res.Error == nil || res.Error.Code != codeUnauthorized {
		t.Fatalf("error = %+v, want %d", res.Error, codeUnauthorized)
	}
}

func TestToolsList(t *testing.T) {
	e := newEnv(t, false)

	res := e.rpc(t, readerKey, "tools/list", nil)
	if res.Error != nil {
		t.Fatalf("error: %+v", res.Error)
	}
	var list struct {
		Tools []toolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res.Result, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := []string{
		"work_list", "work_get", "work_handoff", "work_create", "work_claim",
		"work_heartbeat", "work_release", "work_submit", "work_verify",
		"work_verdict", "work_complete",
	}
	got := make(map[string]toolInfo, len(list.Tools))
	for _, tool := range list.Tools {
		got[tool.Name] = tool
		if tool.Description == "" {
			t.Fatalf("%s has no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("%s inputSchema type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing tool %s (have %d)", name, len(got))
		}
	}
	if len(list.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(list.Tools), len(want))
	}

	claim := got["work_claim"]
	required, _ := claim.InputSchema["required"].([]any)
	if len(required) != 3 {
		t.Fatalf("work_claim required = %v", required)
	}
}

func TestFullLifecycleOverMCP(t *testing.T) {
	e := newEnv(t, true)

	if res := e.call(t, adminKey, "work_create", map[string]any{"item": map[string]any{
		"id":           "w1",
		"workspace_id": "ws",
		"title":        "fix claim race",
		"brief":        "two agents claim at once",
		"contract": map[string]any{
			"goal":  "one claim wins",
			"paths": []string{"internal/work"},
			"checks": []map[string]any{
				{"name": "unit", "cmd": "true", "timeout": 10},
			},
		},
	}}); res.IsError {
		t.Fatalf("work_create: %s", res.text())
	}

	var claim struct {
		Claim work.Claim `json:"claim"`
		Pack  struct {
			Text   string `json:"text"`
			Tokens int    `json:"tokens"`
			Budget int    `json:"budget"`
		} `json:"pack"`
	}
	res := e.call(t, workerKey, "work_claim", map[string]any{"id": "w1", "role": "worker", "plan": "implement"})
	if res.IsError {
		t.Fatalf("work_claim: %s", res.text())
	}
	if err := json.Unmarshal([]byte(res.text()), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claim.Claim.AgentID != "agent-worker" || claim.Claim.LeaseEpoch != 1 {
		t.Fatalf("claim = %+v", claim.Claim)
	}
	if claim.Pack.Text == "" || claim.Pack.Tokens == 0 {
		t.Fatalf("pack = %+v", claim.Pack)
	}

	// Missing evidence must fail without moving the item.
	e.call(t, workerKey, "work_submit", map[string]any{
		"id": "w1", "epoch": claim.Claim.LeaseEpoch, "idempotency_key": "k0",
	}).mustFail(t, "evidence_missing")

	if res := e.call(t, workerKey, "work_submit", map[string]any{
		"id":              "w1",
		"epoch":           claim.Claim.LeaseEpoch,
		"idempotency_key": "k1",
		"evidence": map[string]any{
			"artifacts": []string{"artifact:diff"},
			"diff_hash": "sha-diff",
			"checks": []map[string]any{
				{"name": "unit", "cmd": "true", "exit_code": 0},
			},
		},
	}); res.IsError {
		t.Fatalf("work_submit: %s", res.text())
	}

	var verified struct {
		Claim work.Claim `json:"claim"`
	}
	res = e.call(t, otherKey, "work_claim", map[string]any{"id": "w1", "role": "verifier", "plan": "review"})
	if res.IsError {
		t.Fatalf("verifier claim: %s", res.text())
	}
	if err := json.Unmarshal([]byte(res.text()), &verified); err != nil {
		t.Fatalf("decode verifier claim: %v", err)
	}

	var checkOut struct {
		Checks []work.CheckResult `json:"checks"`
	}
	res = e.call(t, otherKey, "work_verify", map[string]any{"id": "w1", "epoch": verified.Claim.LeaseEpoch})
	if res.IsError {
		t.Fatalf("work_verify: %s", res.text())
	}
	if err := json.Unmarshal([]byte(res.text()), &checkOut); err != nil {
		t.Fatalf("decode checks: %v", err)
	}
	if len(checkOut.Checks) != 1 || checkOut.Checks[0].Name != "unit" {
		t.Fatalf("checks = %+v", checkOut.Checks)
	}

	// Approving a checked item without the verifier's own results is refused.
	e.call(t, otherKey, "work_verdict", map[string]any{
		"id": "w1", "epoch": verified.Claim.LeaseEpoch, "verdict": "approve",
	}).mustFail(t, "verify_required")

	if res := e.call(t, otherKey, "work_verdict", map[string]any{
		"id":      "w1",
		"epoch":   verified.Claim.LeaseEpoch,
		"verdict": "approve",
		"reasons": "unit passed",
		"checks":  checkOut.Checks,
	}); res.IsError {
		t.Fatalf("work_verdict: %s", res.text())
	}

	var integrating struct {
		Claim work.Claim `json:"claim"`
	}
	res = e.call(t, adminKey, "work_claim", map[string]any{"id": "w1", "role": "integrator", "plan": "merge"})
	if res.IsError {
		t.Fatalf("integrator claim: %s", res.text())
	}
	if err := json.Unmarshal([]byte(res.text()), &integrating); err != nil {
		t.Fatalf("decode integrator claim: %v", err)
	}

	if res := e.call(t, adminKey, "work_complete", map[string]any{
		"id": "w1", "epoch": integrating.Claim.LeaseEpoch, "idempotency_key": "done-1",
	}); res.IsError {
		t.Fatalf("work_complete: %s", res.text())
	}

	res = e.call(t, readerKey, "work_get", map[string]any{"id": "w1"})
	if res.IsError {
		t.Fatalf("work_get: %s", res.text())
	}
	var view worksvc.ItemView
	if err := json.Unmarshal([]byte(res.text()), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	if view.Item.Status != work.StatusDone {
		t.Fatalf("status = %s, want done", view.Item.Status)
	}
	if len(view.Reviews) != 1 || view.Reviews[0].Verdict != work.VerdictApprove {
		t.Fatalf("reviews = %+v", view.Reviews)
	}
}

func TestScopeAndToolErrors(t *testing.T) {
	e := newEnv(t, false)

	e.call(t, readerKey, "work_create", map[string]any{
		"item": map[string]any{"id": "w1", "title": "x"},
	}).mustFail(t, "forbidden")

	e.call(t, readerKey, "work_claim", map[string]any{
		"id": "w1", "role": "worker", "plan": "x",
	}).mustFail(t, "forbidden")

	e.call(t, adminKey, "work_nope", map[string]any{}).mustFail(t, "unknown_tool")

	e.call(t, readerKey, "work_get", map[string]any{"id": "missing"}).mustFail(t, "not_found")

	e.call(t, adminKey, "work_list", map[string]any{"status": "bogus"}).mustFail(t, "invalid_status")

	e.call(t, adminKey, "work_verify", map[string]any{"id": "w1", "epoch": 1}).mustFail(t, "not_found")
}

func TestVerifyWithoutRunner(t *testing.T) {
	e := newEnv(t, false)
	e.call(t, adminKey, "work_create", map[string]any{"item": map[string]any{"id": "w1", "title": "x"}})

	e.call(t, adminKey, "work_verify", map[string]any{"id": "w1", "epoch": 1}).mustFail(t, "runner_unavailable")
}

func TestNotificationAndProtocolErrors(t *testing.T) {
	e := newEnv(t, false)

	// Notifications get 202 with no body.
	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	resp, err := e.srv.Client().Post(e.srv.URL+"/mcp/"+adminKey, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	var errRes rpcResult
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "no/such"})
	resp, err = e.srv.Client().Post(e.srv.URL+"/mcp/"+adminKey, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&errRes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errRes.Error == nil || errRes.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v", errRes.Error)
	}
}

func TestMalformedJSON(t *testing.T) {
	e := newEnv(t, false)

	resp, err := e.srv.Client().Post(e.srv.URL+"/mcp/"+adminKey, "application/json", strings.NewReader(`{"jsonrpc":`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	var out rpcResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error == nil || out.Error.Code != codeParseError {
		t.Fatalf("error = %+v", out.Error)
	}
}

func TestHeaderTokenWorks(t *testing.T) {
	e := newEnv(t, false)

	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-API-Key", readerKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	var out rpcResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("error = %+v", out.Error)
	}
}

func TestListReturnsEmptyArray(t *testing.T) {
	e := newEnv(t, false)

	res := e.call(t, readerKey, "work_list", map[string]any{})
	if res.IsError {
		t.Fatalf("work_list: %s", res.text())
	}
	if !strings.Contains(res.text(), `"items":[]`) {
		t.Fatalf("empty list = %s", res.text())
	}
}
