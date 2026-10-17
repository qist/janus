// Package metricx 实现极简 Prometheus 指标（零依赖，标准文本格式）。
//
// 只暴露真正有用的一小组：请求量/延迟、限流、上游错误、工具调用、会话数、token。
// 刻意不做通用指标库 —— 这个桥的观测需求就这么点。
package metricx

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var histogramBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type histo struct {
	counts []int64
	sum    float64
	total  int64
}

func newHisto() *histo { return &histo{counts: make([]int64, len(histogramBuckets)+1)} }

func (h *histo) observe(v float64) {
	h.sum += v
	h.total++
	for i, b := range histogramBuckets {
		if v <= b {
			h.counts[i]++
			return
		}
	}
	h.counts[len(histogramBuckets)]++
}

// Metrics 收集一组零依赖的进程内指标。
type Metrics struct {
	mu sync.Mutex

	started time.Time

	reqTotal map[string]int64 // "endpoint|status" -> n
	reqDur   map[string]*histo

	rateLimited  int64
	upstreamErrs int64
	toolReg      int64
	toolCalls    int64
	toolTimeouts int64
	tokensIn     int64
	tokensOut    int64

	activeStreams int64
	conversations func() int
}

// New 构造指标收集器；conversations 用于在渲染时读取当前会话数，可为 nil。
func New(conversations func() int) *Metrics {
	return &Metrics{
		started:       time.Now(),
		reqTotal:      map[string]int64{},
		reqDur:        map[string]*histo{},
		conversations: conversations,
	}
}

// ObserveRequest 记录一次请求的端点、状态码与耗时。
func (m *Metrics) ObserveRequest(endpoint string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqTotal[endpoint+"|"+strconv.Itoa(status)]++
	h := m.reqDur[endpoint]
	if h == nil {
		h = newHisto()
		m.reqDur[endpoint] = h
	}
	h.observe(d.Seconds())
}

// IncRateLimited 累加被限流拒绝的请求数。
func (m *Metrics) IncRateLimited() {
	if m == nil {
		return
	}
	m.add(&m.rateLimited, 1)
}

// IncUpstreamErr 累加上游失败数。
func (m *Metrics) IncUpstreamErr() {
	if m == nil {
		return
	}
	m.add(&m.upstreamErrs, 1)
}

// IncToolReg 累加 MCP 工具桥注册次数。
func (m *Metrics) IncToolReg() {
	if m == nil {
		return
	}
	m.add(&m.toolReg, 1)
}

// IncToolCalls 累加交给客户端的工具调用数。
func (m *Metrics) IncToolCalls(n int) {
	if m == nil {
		return
	}
	m.add(&m.toolCalls, int64(n))
}

// IncToolTimeouts 累加因客户端未按时回结果而释放的工具调用数。
func (m *Metrics) IncToolTimeouts() {
	if m == nil {
		return
	}
	m.add(&m.toolTimeouts, 1)
}

// AddTokens 累加上游上报的 token 数。
func (m *Metrics) AddTokens(in, out int) {
	if in == 0 && out == 0 {
		return
	}
	if m == nil {
		return
	}
	m.mu.Lock()
	m.tokensIn += int64(in)
	m.tokensOut += int64(out)
	m.mu.Unlock()
}

// StreamStart 标记一个 SSE 流开始。
func (m *Metrics) StreamStart() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.activeStreams++
	m.mu.Unlock()
}

// StreamEnd 标记一个 SSE 流结束。
func (m *Metrics) StreamEnd() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.activeStreams--
	m.mu.Unlock()
}

func (m *Metrics) add(p *int64, n int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	*p += n
	m.mu.Unlock()
}

// Endpoint 把路径归一到低基数的标签，避免 /v1/responses/{id} 造成标签爆炸。
func Endpoint(path string) string {
	switch {
	case path == "/healthz", path == "/metrics", path == "/ui", path == "/":
		return path
	case path == "/v1/models":
		return path
	case strings.HasPrefix(path, "/v1/models/"):
		return "/v1/models/{id}"
	case path == "/v1/chat/completions":
		return path
	case path == "/v1/responses":
		return path
	case strings.HasPrefix(path, "/v1/responses/"):
		return "/v1/responses/{id}"
	case path == "/v1/completions":
		return path
	case path == "/v1/usage":
		return path
	case strings.HasPrefix(path, "/mcp/"):
		return "/mcp/{token}"
	default:
		return "other"
	}
}

// Render 输出 Prometheus 文本格式。
func (m *Metrics) Render() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	help := func(name, typ, desc string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, desc, name, typ)
	}

	help("opencode_bridge_up", "gauge", "1 if the bridge process is running")
	b.WriteString("opencode_bridge_up 1\n")

	help("opencode_bridge_uptime_seconds", "gauge", "Seconds since the bridge started")
	fmt.Fprintf(&b, "opencode_bridge_uptime_seconds %.0f\n", time.Since(m.started).Seconds())

	help("opencode_bridge_requests_total", "counter", "HTTP requests by endpoint and status")
	keys := make([]string, 0, len(m.reqTotal))
	for k := range m.reqTotal {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "opencode_bridge_requests_total{endpoint=%q,status=%q} %d\n",
			parts[0], parts[1], m.reqTotal[k])
	}

	help("opencode_bridge_request_duration_seconds", "histogram", "HTTP request duration by endpoint")
	eps := make([]string, 0, len(m.reqDur))
	for k := range m.reqDur {
		eps = append(eps, k)
	}
	sort.Strings(eps)
	for _, ep := range eps {
		h := m.reqDur[ep]
		for i, ub := range histogramBuckets {
			fmt.Fprintf(&b, "opencode_bridge_request_duration_seconds_bucket{endpoint=%q,le=%q} %d\n",
				ep, trimFloat(ub), cumOf(h, i))
		}
		fmt.Fprintf(&b, "opencode_bridge_request_duration_seconds_bucket{endpoint=%q,le=\"+Inf\"} %d\n",
			ep, h.total)
		fmt.Fprintf(&b, "opencode_bridge_request_duration_seconds_sum{endpoint=%q} %g\n", ep, h.sum)
		fmt.Fprintf(&b, "opencode_bridge_request_duration_seconds_count{endpoint=%q} %d\n", ep, h.total)
	}

	help("opencode_bridge_rate_limited_total", "counter", "Requests rejected by the rate limiter")
	fmt.Fprintf(&b, "opencode_bridge_rate_limited_total %d\n", m.rateLimited)

	help("opencode_bridge_upstream_errors_total", "counter", "Upstream failures surfaced to clients")
	fmt.Fprintf(&b, "opencode_bridge_upstream_errors_total %d\n", m.upstreamErrs)

	help("opencode_bridge_tool_registrations_total", "counter", "MCP tool bridge registrations")
	fmt.Fprintf(&b, "opencode_bridge_tool_registrations_total %d\n", m.toolReg)

	help("opencode_bridge_tool_calls_total", "counter", "Tool calls handed to clients")
	fmt.Fprintf(&b, "opencode_bridge_tool_calls_total %d\n", m.toolCalls)

	help("opencode_bridge_tool_timeouts_total", "counter", "Tool calls released because the client did not return a result in time")
	fmt.Fprintf(&b, "opencode_bridge_tool_timeouts_total %d\n", m.toolTimeouts)

	help("opencode_bridge_active_streams", "gauge", "In-flight SSE streams")
	fmt.Fprintf(&b, "opencode_bridge_active_streams %d\n", m.activeStreams)

	help("opencode_bridge_conversations", "gauge", "Conversations held in memory")
	n := 0
	if m.conversations != nil {
		n = m.conversations()
	}
	fmt.Fprintf(&b, "opencode_bridge_conversations %d\n", n)

	help("opencode_bridge_tokens_total", "counter", "Tokens reported by the upstream")
	fmt.Fprintf(&b, "opencode_bridge_tokens_total{direction=\"input\"} %d\n", m.tokensIn)
	fmt.Fprintf(&b, "opencode_bridge_tokens_total{direction=\"output\"} %d\n", m.tokensOut)

	return b.String()
}

// cumOf 把"只落在首个命中桶"的计数变成累计值。
func cumOf(h *histo, upTo int) int64 {
	var c int64
	for i := 0; i <= upTo; i++ {
		c += h.counts[i]
	}
	return c
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
