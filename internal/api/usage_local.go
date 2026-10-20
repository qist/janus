package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"janus/internal/db"
	"janus/internal/httpx"
)

// usage_local.go —— 本地用量记账（docs/ARCHITECTURE.md §11 / P1.5）。
//
// 与 usage 包的「远端账单」互补：
//   - 远端（/api/v1/usage）：OpenCode console 的余额与官方统计，全局一份；
//   - 本地（/api/v1/usage/local）：janus 自己按 provider/credential/model 记账，
//     多账号多渠道各自独立，不依赖上游是否开放账单接口。
//
// 记账点：直连模型面（agent key / agent=direct）。会话桥的 token 由 OpenCode
// 侧统计，不在这里重复计算。

// usageCounts 是从一次响应里解析出的 token 分项。
type usageCounts struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

func (u usageCounts) empty() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.Reasoning == 0
}

// tailBuffer 只保留最后 max 字节：流式响应的 usage 总在末尾事件里，
// 没必要缓存整条长回答。
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	if t.max <= 0 {
		t.max = 1 << 20
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

// extractUsage 从非流式 JSON 或 SSE 流里提取 usage。
//
// 兼容三种信封：
//   - OpenAI chat：usage.prompt_tokens / completion_tokens / prompt_tokens_details.cached_tokens
//   - OpenAI responses：usage.input_tokens / output_tokens / input_tokens_details.cached_tokens
//   - Anthropic：usage.input_tokens / output_tokens / cache_read_input_tokens / cache_creation_input_tokens
//
// 流式事件的 usage 是累进/分段给出的，这里取各字段的最大值（message_delta 的
// output_tokens 是累计值；message_start 只给输入）。
func extractUsage(raw []byte) usageCounts {
	var acc usageCounts
	if len(raw) == 0 {
		return acc
	}
	scan := func(b []byte) {
		b = bytes.TrimSpace(b)
		if len(b) == 0 || !json.Valid(b) {
			return
		}
		var v any
		if json.Unmarshal(b, &v) == nil {
			walkUsage(v, &acc)
		}
	}
	// 非流式：整包就是一个 JSON 对象。
	scan(raw)
	if !acc.empty() {
		return acc
	}
	// 流式：逐行找 data: 事件（事件体本身是完整 JSON）。
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		scan(payload)
	}
	return acc
}

// walkUsage 递归找响应里的 usage 对象（流式事件可能把它包在 response/message 里）。
func walkUsage(v any, acc *usageCounts) {
	m, ok := v.(map[string]any)
	if !ok {
		if arr, ok := v.([]any); ok {
			for _, it := range arr {
				walkUsage(it, acc)
			}
		}
		return
	}
	if u, ok := m["usage"].(map[string]any); ok {
		mergeUsage(u, acc)
	}
	for key, child := range m {
		if key == "usage" {
			continue
		}
		switch child.(type) {
		case map[string]any, []any:
			walkUsage(child, acc)
		}
	}
}

// mergeUsage 把单个 usage 对象合并进累计值（取最大值，避免流式分段重复累加）。
func mergeUsage(u map[string]any, acc *usageCounts) {
	num := func(key string) int64 {
		switch v := u[key].(type) {
		case float64:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		}
		return 0
	}
	take := func(key string, dst *int64) {
		if n := num(key); n > *dst {
			*dst = n
		}
	}
	take("input_tokens", &acc.Input)
	take("prompt_tokens", &acc.Input)
	take("output_tokens", &acc.Output)
	take("completion_tokens", &acc.Output)
	take("cache_read_input_tokens", &acc.CacheRead)
	take("cache_creation_input_tokens", &acc.CacheWrite)
	take("reasoning_tokens", &acc.Reasoning)
	// 嵌套明细（OpenAI / Responses）。
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := d["cached_tokens"].(float64); ok && int64(n) > acc.CacheRead {
			acc.CacheRead = int64(n)
		}
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if n, ok := d["cached_tokens"].(float64); ok && int64(n) > acc.CacheRead {
			acc.CacheRead = int64(n)
		}
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		if n, ok := d["reasoning_tokens"].(float64); ok && int64(n) > acc.Reasoning {
			acc.Reasoning = int64(n)
		}
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		if n, ok := d["reasoning_tokens"].(float64); ok && int64(n) > acc.Reasoning {
			acc.Reasoning = int64(n)
		}
	}
}

// usageCost 按模型目录里的价格估算成本（USD）；没有价格时返回 0。
func (s *Server) usageCost(st *db.Store, providerID, upstreamID string, u usageCounts) float64 {
	if st == nil || providerID == "" || upstreamID == "" {
		return 0
	}
	m, ok := st.GetModel(providerID + "/" + upstreamID)
	if !ok || len(m.Pricing) == 0 {
		return 0
	}
	type price struct {
		Tier                 string  `json:"tier"`
		InputPerMillion      float64 `json:"input_per_million"`
		OutputPerMillion     float64 `json:"output_per_million"`
		CacheReadPerMillion  float64 `json:"cache_read_per_million"`
		CacheWritePerMillion float64 `json:"cache_write_per_million"`
	}
	var list []price
	if err := json.Unmarshal(m.Pricing, &list); err != nil {
		var one price
		if json.Unmarshal(m.Pricing, &one) != nil {
			return 0
		}
		list = []price{one}
	}
	if len(list) == 0 {
		return 0
	}
	p := list[0]
	for _, it := range list {
		if it.Tier == "" {
			p = it
			break
		}
	}
	perM := func(n int64) float64 { return float64(n) / 1e6 }
	return perM(u.Input)*p.InputPerMillion +
		perM(u.Output)*p.OutputPerMillion +
		perM(u.CacheRead)*p.CacheReadPerMillion +
		perM(u.CacheWrite)*p.CacheWritePerMillion
}

// commitUsage 把一次请求写入 usage_events。记录失败只记 debug 日志，不影响请求。
func (s *Server) commitUsage(st *db.Store, ev db.UsageEvent, raw []byte) {
	if st == nil {
		return
	}
	if !ev.Stream || len(raw) > 0 {
		u := extractUsage(raw)
		ev.InputTokens = u.Input
		ev.OutputTokens = u.Output
		ev.CacheReadTokens = u.CacheRead
		ev.CacheWriteTokens = u.CacheWrite
		ev.ReasoningTokens = u.Reasoning
		ev.CostUSD = s.usageCost(st, ev.ProviderID, ev.UpstreamModel, u)
	}
	if err := st.RecordUsage(ev); err != nil {
		s.log.Debugf("record usage failed: %v", err)
	}
}

// parseUsageFilter 解析时间窗与过滤参数（默认最近 7 天）。
func parseUsageFilter(r *http.Request) db.UsageFilter {
	q := r.URL.Query()
	f := db.UsageFilter{
		ProviderID:   strings.TrimSpace(q.Get("provider_id")),
		CredentialID: strings.TrimSpace(q.Get("credential_id")),
		Model:        strings.TrimSpace(q.Get("model")),
		KeyID:        strings.TrimSpace(q.Get("key_id")),
		Source:       strings.TrimSpace(q.Get("source")),
	}
	if v := q.Get("from"); v != "" {
		f.From, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("to"); v != "" {
		f.To, _ = strconv.ParseInt(v, 10, 64)
	}
	if f.From == 0 && f.To == 0 {
		days := 7
		if v := q.Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
				days = n
			}
		}
		f.From = time.Now().AddDate(0, 0, -days).Unix()
	}
	return f
}

// handleLocalUsage 返回本地记账聚合：GET /api/v1/usage/local
func (s *Server) handleLocalUsage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	f := parseUsageFilter(r)
	summary := st.SummarizeUsage(f)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"summary":    summary,
		"from":       f.From,
		"to":         f.To,
		"fetched_at": time.Now().UTC(),
		"source":     "local",
	})
}

// handleLocalUsageEvents 返回逐条本地记账：GET /api/v1/usage/events
func (s *Server) handleLocalUsageEvents(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	f := parseUsageFilter(r)
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	rows := st.ListUsage(f, limit)
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, map[string]any{
			"id": e.ID, "ts": e.Ts, "provider_id": e.ProviderID,
			"credential_id": e.CredentialID, "key_id": e.KeyID, "key_kind": e.KeyKind,
			"protocol": e.Protocol, "model": e.Model, "upstream_model": e.UpstreamModel,
			"stream": e.Stream, "status": e.Status, "error_code": e.ErrorCode,
			"input_tokens": e.InputTokens, "output_tokens": e.OutputTokens,
			"cache_read_tokens": e.CacheReadTokens, "cache_write_tokens": e.CacheWriteTokens,
			"reasoning_tokens": e.ReasoningTokens, "cost_usd": e.CostUSD,
			"duration_ms": e.DurationMs, "source": e.Source,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
}
