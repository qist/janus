package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ② 免费档 403：映射成可操作的提示，而不是笼统的 upstream_auth_error
func TestMapUpstreamFailureFreeTier(t *testing.T) {
	f := &upstreamFailure{
		Type:    "FreeTierError",
		Message: "OpenCode's free tier can only be used from within OpenCode",
		Status:  http.StatusForbidden,
	}
	status, typ, code, msg := mapUpstreamFailure(f)
	if status != http.StatusForbidden || typ != "api_error" || code != "free_tier_restricted" {
		t.Fatalf("status=%d typ=%q code=%q", status, typ, code)
	}
	if !strings.Contains(msg, "opencode-go") {
		t.Errorf("提示应引导改用 opencode-go/*，实际: %s", msg)
	}
}

// ① Responses 的 web_search 服务端工具必须被识别并暴露给 agent
func TestResponsesWebSearchTool(t *testing.T) {
	raw := json.RawMessage(`[{"type":"web_search"},{"type":"function","name":"get_weather","parameters":{"type":"object"}}]`)
	specs := responsesToolsToSpecs(raw)
	if len(specs) != 2 {
		t.Fatalf("specs=%+v", specs)
	}
	var ws *ToolSpec
	for i := range specs {
		if specs[i].Function.Name == webSearchToolName {
			ws = &specs[i]
		}
	}
	if ws == nil {
		t.Fatal("web_search 未暴露给 agent")
	}
	if !strings.Contains(string(ws.Function.Parameters), `"query"`) {
		t.Errorf("web_search schema 缺少 query: %s", ws.Function.Parameters)
	}
	if !responsesHasWebSearch(raw) {
		t.Error("responsesHasWebSearch 漏判")
	}
	if responsesHasWebSearch(json.RawMessage(`[{"type":"function","name":"x"}]`)) {
		t.Error("responsesHasWebSearch 误判")
	}
}

// ③ BRIDGE_MODEL_ECHO=request 时回显客户端请求的模型名
func TestModelEcho(t *testing.T) {
	ref := OCModelRef{ProviderID: "opencode-go", ID: "mimo-v2.5-pro"}

	sReal := &Server{cfg: Config{ModelEcho: "real"}}
	if got := sReal.echoModel(ref, "claude-sonnet-4-5"); got != "opencode-go/mimo-v2.5-pro" {
		t.Errorf("real 应回显真实模型，got %q", got)
	}

	sReq := &Server{cfg: Config{ModelEcho: "request"}}
	if got := sReq.echoModel(ref, "claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Errorf("request 应回显请求名，got %q", got)
	}
	// 请求名为空时回落到真实模型
	if got := sReq.echoModel(ref, ""); got != "opencode-go/mimo-v2.5-pro" {
		t.Errorf("空请求名应回落到真实模型，got %q", got)
	}

	if normalizeModelEcho("req") != "request" || normalizeModelEcho("") != "real" {
		t.Error("normalizeModelEcho 归一化错误")
	}
}
