package metricx

import (
	"strings"
	"testing"
	"time"
)

// ---------- 指标 ----------

func TestMetricsRender(t *testing.T) {
	m := New(func() int { return 7 })
	m.ObserveRequest("/v1/models", 200, 12*time.Millisecond)
	m.ObserveRequest("/v1/models", 500, 3*time.Second)
	m.ObserveRequest("/v1/chat/completions", 200, 100*time.Millisecond)
	m.IncRateLimited()
	m.IncToolReg()
	m.IncToolCalls(2)
	m.IncToolTimeouts()
	m.AddTokens(10, 20)
	m.StreamStart()

	out := m.Render()
	for _, want := range []string{
		"opencode_bridge_up 1",
		`opencode_bridge_requests_total{endpoint="/v1/models",status="200"} 1`,
		`opencode_bridge_requests_total{endpoint="/v1/models",status="500"} 1`,
		"opencode_bridge_rate_limited_total 1",
		"opencode_bridge_tool_registrations_total 1",
		"opencode_bridge_tool_calls_total 2",
		"opencode_bridge_tool_timeouts_total 1",
		"opencode_bridge_active_streams 1",
		"opencode_bridge_conversations 7",
		`opencode_bridge_tokens_total{direction="input"} 10`,
		`opencode_bridge_tokens_total{direction="output"} 20`,
		`opencode_bridge_request_duration_seconds_count{endpoint="/v1/models"} 2`,
		"opencode_bridge_request_duration_seconds_bucket",
		`le="+Inf"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("指标输出缺少 %q\n%s", want, out)
		}
	}
	// 直方图桶必须单调不减
	if !strings.Contains(out, `le="0.05"} 1`) {
		t.Errorf("12ms 应落在 0.05 桶:\n%s", out)
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveRequest("/x", 200, time.Millisecond)
	m.IncRateLimited()
	m.IncUpstreamErr()
	m.IncToolReg()
	m.IncToolCalls(1)
	m.IncToolTimeouts()
	m.AddTokens(1, 1)
	m.StreamStart()
	m.StreamEnd()
	if m.Render() != "" {
		t.Fatal("nil metrics 应渲染为空")
	}
}

func TestMetricEndpointNormalization(t *testing.T) {
	cases := map[string]string{
		"/healthz":               "/healthz",
		"/metrics":               "/metrics",
		"/ui":                    "/ui",
		"/":                      "/",
		"/v1/models":             "/v1/models",
		"/v1/models/opencode/x":  "/v1/models/{id}",
		"/v1/chat/completions":   "/v1/chat/completions",
		"/v1/responses":          "/v1/responses",
		"/v1/responses/resp_abc": "/v1/responses/{id}",
		"/mcp/deadbeef":          "/mcp/{token}",
		"/v1/unknown":            "other",
	}
	for in, want := range cases {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
