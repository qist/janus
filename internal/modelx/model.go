// Package modelx 负责「客户端 model 字符串 ↔ 上游模型引用」的解析，以及
// 把上游目录投影成 OpenAI 线格式（列表条目 / 详情）。
//
// 这里不缓存目录、不发请求：目录缓存归各渠道（internal/channel/*），
// 本包只做纯函数式的命名解析与结构投影，便于单独测试。
package modelx

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"janus/internal/channel/workbuddy"
	"janus/internal/protocol/openai"
	"janus/internal/upstream"
)

// Resolve 解析客户端传来的 model 字符串。
//
//	opencode/fledge-alpha-free      → providerID=opencode id=fledge-alpha-free
//	opencode/claude-sonnet-5-5:high → 加 variant=high
//	fledge-alpha-free               → 在列表里唯一匹配
func Resolve(raw string, list []upstream.OCModel) (upstream.OCModelRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return upstream.OCModelRef{}, fmt.Errorf("model is required")
	}

	provider, id, variant := "", raw, ""

	// 拆 variant（最后一个 ':'，且不能在开头）
	if i := strings.LastIndex(id, ":"); i > 0 && !strings.Contains(id[i:], "/") {
		variant = id[i+1:]
		id = id[:i]
	}
	// 拆 provider（第一个 '/'）
	if i := strings.Index(id, "/"); i > 0 {
		provider, id = id[:i], id[i+1:]
	}

	if provider != "" {
		for _, m := range list {
			if m.ProviderID == provider && m.ID == id {
				return upstream.OCModelRef{ProviderID: provider, ID: id, Variant: normalizeVariant(variant)}, nil
			}
		}
		// provider/id 精确匹配失败，但 provider 已知 → 仍然回传，让上游自己报错
		// （比返回 404 更利于客户端显示真实原因）。仅当 provider 完全未知时才 404。
		knownProvider := false
		for _, m := range list {
			if m.ProviderID == provider {
				knownProvider = true
				break
			}
		}
		if knownProvider {
			return upstream.OCModelRef{ProviderID: provider, ID: id, Variant: normalizeVariant(variant)}, nil
		}
		return upstream.OCModelRef{}, fmt.Errorf("model %q not found (unknown provider %q)", raw, provider)
	}

	// 裸 id：唯一匹配
	var hits []upstream.OCModel
	lower := strings.ToLower(id)
	for _, m := range list {
		if strings.ToLower(m.ID) == lower {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return upstream.OCModelRef{ProviderID: hits[0].ProviderID, ID: hits[0].ID, Variant: normalizeVariant(variant)}, nil
	case 0:
		return upstream.OCModelRef{}, fmt.Errorf("model %q not found", raw)
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.ProviderID+"/"+h.ID)
		}
		return upstream.OCModelRef{}, fmt.Errorf("model %q is ambiguous, use one of: %s", raw, strings.Join(names, ", "))
	}
}

func normalizeVariant(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "default":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

// ---------- 思考强度（reasoning_effort）↔ 上游 variant ----------

// 上游 variant 就是思考强度档位：variant.settings.reasoningEffort = <档位>。
// 实测存在过的取值：none / minimal / low / medium / high / xhigh / max / thinking。
var effortRank = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// effortAliases 把客户端常见的写法归一化。
var effortAliases = map[string]string{
	"":           "",
	"default":    "",
	"auto":       "",
	"min":        "minimal",
	"minimum":    "minimal",
	"med":        "medium",
	"mid":        "medium",
	"normal":     "medium",
	"x-high":     "xhigh",
	"extra-high": "xhigh",
	"extra_high": "xhigh",
	"very-high":  "xhigh",
	"maximum":    "max",
	"highest":    "max",
	"off":        "none",
	"disabled":   "none",
}

// VariantIDs 返回模型可用的思考强度档位（按强度升序）。
func VariantIDs(m upstream.OCModel) []string {
	out := make([]string, 0, len(m.Variants))
	for _, v := range m.Variants {
		if v.ID != "" {
			out = append(out, v.ID)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := effortRank[out[i]]
		rj, jok := effortRank[out[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true // 已知档位排前面
		case jok:
			return false
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// PickVariant 把 OpenAI 的 reasoning_effort 映射成上游 variant。
//
// 规则（按优先级）：
//  1. 空 / default / auto → 不指定 variant（用模型默认）
//  2. 与某个 variant id 完全一致 → 直接用（支持 xhigh/max 这类扩展档位）
//  3. 归一化别名后命中 → 用
//  4. 模型只有 none/thinking 这种布尔档位 → 非 none 一律用 thinking
//  5. 否则按档位强弱取最接近的（并列取更弱的，更省资源）
//
// 返回 false 表示该模型没有可用的思考强度档位，调用方应忽略该参数。
func PickVariant(m upstream.OCModel, effort string) (string, bool) {
	ids := VariantIDs(m)
	if len(ids) == 0 {
		return "", false
	}

	raw := strings.ToLower(strings.TrimSpace(effort))
	if norm, ok := effortAliases[raw]; ok {
		raw = norm
	}
	if raw == "" || raw == "default" {
		return "", true // 明确要求默认档
	}

	has := func(id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}

	// 2/3：直接命中
	if has(raw) {
		return raw, true
	}

	// 4：布尔档位模型（如 none / thinking）
	for _, id := range ids {
		if id == "thinking" {
			if raw == "none" {
				if has("none") {
					return "none", true
				}
			}
			return "thinking", true
		}
	}

	// 5：按强度取最接近
	want, known := effortRank[raw]
	if !known {
		return "", false // 客户端给了个我们不认识的档位名
	}
	best, bestDist, bestRank := "", 1<<30, 1<<30
	for _, id := range ids {
		r, ok := effortRank[id]
		if !ok {
			continue // 跳过 thinking 等特殊档位
		}
		d := r - want
		if d < 0 {
			d = -d
		}
		if d < bestDist || (d == bestDist && r < bestRank) {
			best, bestDist, bestRank = id, d, r
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// Find 在列表里按 ref 找到完整模型信息（含 variants / limit）。
func Find(list []upstream.OCModel, ref upstream.OCModelRef) *upstream.OCModel {
	for i := range list {
		if list[i].ProviderID == ref.ProviderID && list[i].ID == ref.ID {
			return &list[i]
		}
	}
	return nil
}

// ToOpenAI 把上游模型转成 /v1/models 的条目。
//
// 除了 OpenAI 标准的四个字段，额外补了上下文/输出上限与可用思考强度档位。
// 这些是各家网关（OpenRouter/LiteLLM/vLLM）的通行扩展，标准客户端会忽略。
func ToOpenAI(m upstream.OCModel) openai.ModelObject {
	return openai.ModelObject{
		ID:                        m.ProviderID + "/" + m.ID,
		Object:                    "model",
		Created:                   m.TimeReleased() / 1000, // 上游毫秒 → OpenAI 秒
		OwnedBy:                   m.ProviderID,
		ContextLength:             m.Limit.Context,
		MaxOutputTokens:           m.Limit.Output,
		SupportedReasoningEfforts: VariantIDs(m),
		InputModalities:           m.Capabilities.Input,
		Free:                      IsFree(m),
	}
}

// IsFree 判断模型是否免费：所有价格档位的 input/output 都是 0。
// 无价格信息（Cost 为空）时保守判定为收费，避免误标。
func IsFree(m upstream.OCModel) bool {
	if len(m.Cost) == 0 {
		return false
	}
	for _, c := range m.Cost {
		if c.Input > 0 || c.Output > 0 {
			return false
		}
	}
	return true
}

// ToOpenAIWithCredits 把模型转成 OpenAI 条目；workbuddy 模型附带计费倍率，
// 且上下文长度用运行时档位覆盖（立即生效）。非 workbuddy 模型（opencode
// 渠道）可用 ocCtx 覆盖展示预算（0 = 跟随目录）。
// 零倍率同时打上免费标记（workbuddy 无 OpenCode cost 数据，IsFree 判不出）。
func ToOpenAIWithCredits(m upstream.OCModel, ocCtx int) openai.ModelObject {
	obj := ToOpenAI(m)
	if c := workbuddy.CreditsFor(m.ProviderID, m.ID); c != "" {
		obj.WorkBuddyCredits = c
		if workbuddy.CreditsIsFree(c) {
			obj.Free = true
		}
	}
	if ctx := workbuddy.ContextOverride(m.ProviderID, m.ID); ctx > 0 {
		obj.ContextLength = ctx
	} else if ocCtx > 0 && m.ProviderID != workbuddy.ProviderID {
		obj.ContextLength = ocCtx
	}
	return obj
}

// ---------- 模型详情 ----------

// Caps 模型能力。
type Caps struct {
	Tools  bool     `json:"tools"`
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// VariantDetail 一个思考强度档位及其底层 provider 设置。
type VariantDetail struct {
	ID       string          `json:"id"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Pricing 价格，单位统一为 **USD / 百万 token**（上游原始单位）。
type Pricing struct {
	Tier                 string  `json:"tier,omitempty"` // 空 = 基础档；否则如 ">200k"
	InputPerMillion      float64 `json:"input_per_million"`
	OutputPerMillion     float64 `json:"output_per_million"`
	CacheReadPerMillion  float64 `json:"cache_read_per_million"`
	CacheWritePerMillion float64 `json:"cache_write_per_million"`
}

// Detail 是 GET /v1/models/{id} 的返回。
//
// 在标准字段之外给出挑选模型需要的全部信息：上下文/输出上限、思考档位及其底层
// 设置、能力、价格。列表端点只给轻量字段，详情端点才给这些（避免 /v1/models
// 体积过大）。
type Detail struct {
	openai.ModelObject
	Name         string          `json:"name,omitempty"`
	Family       string          `json:"family,omitempty"`
	Status       string          `json:"status,omitempty"`
	Enabled      bool            `json:"enabled"`
	ToolCalling  bool            `json:"tool_calling"`
	Capabilities *Caps           `json:"capabilities,omitempty"`
	Variants     []VariantDetail `json:"variants,omitempty"`
	Pricing      []Pricing       `json:"pricing,omitempty"`
}

// ToDetail 组装模型详情。
func ToDetail(m upstream.OCModel) Detail {
	d := Detail{
		ModelObject: ToOpenAI(m),
		Name:        m.Name,
		Family:      m.Family,
		Status:      m.Status,
		Enabled:     m.Enabled,
		ToolCalling: m.Capabilities.Tools,
		Capabilities: &Caps{
			Tools:  m.Capabilities.Tools,
			Input:  m.Capabilities.Input,
			Output: m.Capabilities.Output,
		},
	}
	for _, v := range m.Variants {
		if v.ID == "" {
			continue
		}
		d.Variants = append(d.Variants, VariantDetail{ID: v.ID, Settings: v.Settings})
	}
	for _, c := range m.Cost {
		mp := Pricing{
			InputPerMillion:      c.Input,
			OutputPerMillion:     c.Output,
			CacheReadPerMillion:  c.Cache.Read,
			CacheWritePerMillion: c.Cache.Write,
		}
		if c.Tier != nil && c.Tier.Size > 0 {
			mp.Tier = fmt.Sprintf(">%d", c.Tier.Size)
		}
		d.Pricing = append(d.Pricing, mp)
	}
	return d
}

// ---------- Claude Code 档位映射 ----------

// TierAliases 是 Claude Code 常用的档位别名（供 /anthropic/v1/models 展示与默认映射）。
var TierAliases = []struct{ Alias, Tier string }{
	{"claude-opus-4-1", "opus"},
	{"claude-sonnet-4-5", "sonnet"},
	{"claude-3-5-haiku", "haiku"},
}

// TierOf 判断模型名属于哪个 Anthropic 档位（opus/sonnet/haiku），否则 ""。
func TierOf(raw string) string {
	l := strings.ToLower(raw)
	switch {
	case strings.Contains(l, "opus"):
		return "opus"
	case strings.Contains(l, "sonnet"):
		return "sonnet"
	case strings.Contains(l, "haiku"):
		return "haiku"
	}
	return ""
}

// PickTier 按档位自动挑一个可用模型：
//   - 排除经 API 会 403 的 opencode 免费额度
//   - 名字命中档位关键词（flash/mini、pro/max）的优先；
//     在此前提下 haiku 取最便宜、opus 取最贵、sonnet 取中间
func PickTier(list []upstream.OCModel, tier string) (upstream.OCModelRef, bool) {
	type cand struct {
		m    upstream.OCModel
		cost float64
		rank int
	}
	rankName := func(name string) int {
		l := strings.ToLower(name)
		switch tier {
		case "haiku":
			if strings.Contains(l, "flash") || strings.Contains(l, "mini") || strings.Contains(l, "lite") {
				return 0
			}
		case "opus":
			if strings.Contains(l, "pro") || strings.Contains(l, "luna") || strings.Contains(l, "max") || strings.Contains(l, "opus") {
				return 0
			}
		}
		return 1
	}
	var cs []cand
	for _, m := range list {
		if !m.Enabled || !m.Capabilities.Tools {
			continue
		}
		if m.ProviderID == "opencode" && IsFree(m) {
			continue // 免费额度经 API 会 403
		}
		c := 0.0
		if len(m.Cost) > 0 {
			c = m.Cost[0].Input + m.Cost[0].Output
		}
		cs = append(cs, cand{m: m, cost: c, rank: rankName(m.ID)})
	}
	if len(cs) == 0 {
		return upstream.OCModelRef{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		if cs[i].cost != cs[j].cost {
			return cs[i].cost < cs[j].cost
		}
		return cs[i].m.ID < cs[j].m.ID
	})
	// 只在最优 rank 的一组里，按档位挑（组内已按价格升序）
	bestRank := cs[0].rank
	group := cs[:0:0]
	for _, c := range cs {
		if c.rank == bestRank {
			group = append(group, c)
		}
	}
	var pick upstream.OCModel
	switch tier {
	case "haiku":
		pick = group[0].m
	case "opus":
		pick = group[len(group)-1].m
	default:
		pick = group[len(group)/2].m
	}
	return upstream.OCModelRef{ProviderID: pick.ProviderID, ID: pick.ID}, true
}

// UsableByAPI 粗略判断模型能否经 Janus 的 API 实际调用。
// 已知 opencode 提供商的免费额度经 API 调用会 403
// （free tier can only be used from within OpenCode），故排除。
func UsableByAPI(m upstream.OCModel) bool {
	if !m.Enabled || !m.Capabilities.Tools {
		return false
	}
	if m.ProviderID == "opencode" && IsFree(m) {
		return false
	}
	return true
}
