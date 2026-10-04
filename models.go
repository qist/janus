package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 模型列表缓存 + OpenAI model 字符串 ↔ OpenCode Model.Ref 解析。
//
// OpenAI 侧统一用 "providerID/modelID"（可选 ":variant"），
// 保证 /v1/models 与 /v1/chat/completions 的 model 字段无歧义互指。

type ModelCache struct {
	mu      sync.RWMutex
	list    []OCModel
	def     *OCModel
	fetched time.Time
	ttl     time.Duration
}

func NewModelCache(ttl time.Duration) *ModelCache {
	return &ModelCache{ttl: ttl}
}

func (c *ModelCache) Get(ctx context.Context, up *Upstream, directory string, force bool) ([]OCModel, error) {
	c.mu.RLock()
	ok := !force && len(c.list) > 0 && time.Since(c.fetched) < c.ttl
	list := c.list
	c.mu.RUnlock()
	if ok {
		return list, nil
	}

	fresh, err := up.ListModels(ctx, directory)

	c.mu.RLock()
	old := c.list
	c.mu.RUnlock()

	if err == nil && len(fresh) == 0 {
		// 上游偶尔会 200 + 空 body/空数组（实测会导致所有模型"消失"，
		// 客户端随即报 unknown provider）。空列表一律不采纳：
		// 有旧数据就继续用，没有就当失败，别把可用状态覆盖成不可用。
		if len(old) > 0 {
			return old, nil
		}
		err = fmt.Errorf("upstream returned an empty model list")
	}

	if err != nil {
		// 拿不到新数据时退回旧缓存，别让模型列表变成 5xx
		if len(old) > 0 {
			return old, nil
		}
		return nil, err
	}

	c.mu.Lock()
	c.list = fresh
	c.fetched = time.Now()
	c.mu.Unlock()

	// 顺带刷新默认模型；失败不影响列表本身
	if def, derr := up.DefaultModel(ctx, directory); derr == nil {
		c.mu.Lock()
		c.def = def
		c.mu.Unlock()
	}
	return fresh, nil
}

// Default 返回上游默认模型（带缓存）。
//
// 注意 Get() 在缓存新鲜时会直接返回、不再请求上游，所以若上次 def 抓取失败，
// 光靠 Get() 永远补不上；这里在缓存后仍为空时强制刷新一次。
func (c *ModelCache) Default(ctx context.Context, up *Upstream, directory string) (*OCModel, error) {
	if def := c.cachedDefault(); def != nil {
		return def, nil
	}
	if _, err := c.Get(ctx, up, directory, false); err != nil {
		return nil, err
	}
	if def := c.cachedDefault(); def != nil {
		return def, nil
	}
	if _, err := c.Get(ctx, up, directory, true); err != nil {
		return nil, err
	}
	if def := c.cachedDefault(); def != nil {
		return def, nil
	}
	return nil, fmt.Errorf("cannot determine upstream default model")
}

func (c *ModelCache) cachedDefault() *OCModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.def
}

// Resolve 解析客户端传来的 model 字符串。
//
//	opencode/fledge-alpha-free      → providerID=opencode id=fledge-alpha-free
//	opencode/claude-sonnet-5-5:high → 加 variant=high
//	fledge-alpha-free               → 在列表里唯一匹配
func ResolveModel(raw string, list []OCModel) (OCModelRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return OCModelRef{}, fmt.Errorf("model is required")
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
				return OCModelRef{ProviderID: provider, ID: id, Variant: normalizeVariant(variant)}, nil
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
			return OCModelRef{ProviderID: provider, ID: id, Variant: normalizeVariant(variant)}, nil
		}
		return OCModelRef{}, fmt.Errorf("model %q not found (unknown provider %q)", raw, provider)
	}

	// 裸 id：唯一匹配
	var hits []OCModel
	lower := strings.ToLower(id)
	for _, m := range list {
		if strings.ToLower(m.ID) == lower {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return OCModelRef{ProviderID: hits[0].ProviderID, ID: hits[0].ID, Variant: normalizeVariant(variant)}, nil
	case 0:
		return OCModelRef{}, fmt.Errorf("model %q not found", raw)
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.ProviderID+"/"+h.ID)
		}
		return OCModelRef{}, fmt.Errorf("model %q is ambiguous, use one of: %s", raw, strings.Join(names, ", "))
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
func VariantIDs(m OCModel) []string {
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
func PickVariant(m OCModel, effort string) (string, bool) {
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

// FindModel 在列表里按 ref 找到完整模型信息（含 variants / limit）。
func FindModel(list []OCModel, ref OCModelRef) *OCModel {
	for i := range list {
		if list[i].ProviderID == ref.ProviderID && list[i].ID == ref.ID {
			return &list[i]
		}
	}
	return nil
}

// ToOpenAI 把 OpenCode 模型转成 /v1/models 的条目。
//
// 除了 OpenAI 标准的四个字段，额外补了上下文/输出上限与可用思考强度档位。
// 这些是各家网关（OpenRouter/LiteLLM/vLLM）的通行扩展，标准客户端会忽略。
func ToOpenAI(m OCModel) ModelObject {
	return ModelObject{
		ID:                        m.ProviderID + "/" + m.ID,
		Object:                    "model",
		Created:                   m.TimeReleased() / 1000, // OpenCode 毫秒 → OpenAI 秒
		OwnedBy:                   m.ProviderID,
		ContextLength:             m.Limit.Context,
		MaxOutputTokens:           m.Limit.Output,
		SupportedReasoningEfforts: VariantIDs(m),
		InputModalities:           m.Capabilities.Input,
		Free:                      isFreeModel(m),
	}
}

// isFreeModel 判断模型是否免费：所有价格档位的 input/output 都是 0。
// 无价格信息（Cost 为空）时保守判定为收费，避免误标。
func isFreeModel(m OCModel) bool {
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

// ---------- 模型详情 ----------

// ModelCaps 模型能力。
type ModelCaps struct {
	Tools  bool     `json:"tools"`
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// VariantDetail 一个思考强度档位及其底层 provider 设置。
type VariantDetail struct {
	ID       string          `json:"id"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// ModelPricing 价格，单位统一为 **USD / 百万 token**（上游原始单位）。
type ModelPricing struct {
	Tier                 string  `json:"tier,omitempty"` // 空 = 基础档；否则如 ">200k"
	InputPerMillion      float64 `json:"input_per_million"`
	OutputPerMillion     float64 `json:"output_per_million"`
	CacheReadPerMillion  float64 `json:"cache_read_per_million"`
	CacheWritePerMillion float64 `json:"cache_write_per_million"`
}

// ModelDetail 是 GET /v1/models/{id} 的返回。
//
// 在标准字段之外给出挑选模型需要的全部信息：上下文/输出上限、思考档位及其底层
// 设置、能力、价格。列表端点只给轻量字段，详情端点才给这些（避免 /v1/models
// 体积过大）。
type ModelDetail struct {
	ModelObject
	Name         string          `json:"name,omitempty"`
	Family       string          `json:"family,omitempty"`
	Status       string          `json:"status,omitempty"`
	Enabled      bool            `json:"enabled"`
	ToolCalling  bool            `json:"tool_calling"`
	Capabilities *ModelCaps      `json:"capabilities,omitempty"`
	Variants     []VariantDetail `json:"variants,omitempty"`
	Pricing      []ModelPricing  `json:"pricing,omitempty"`
}

// ToDetail 组装模型详情。
func ToDetail(m OCModel) ModelDetail {
	d := ModelDetail{
		ModelObject: ToOpenAI(m),
		Name:        m.Name,
		Family:      m.Family,
		Status:      m.Status,
		Enabled:     m.Enabled,
		ToolCalling: m.Capabilities.Tools,
		Capabilities: &ModelCaps{
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
		mp := ModelPricing{
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
