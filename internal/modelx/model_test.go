package modelx

import (
	"encoding/json"
	"strings"
	"testing"

	"janus/internal/upstream"
)

// ---------- ResolveModel ----------

func testModels() []upstream.OCModel {
	return []upstream.OCModel{
		{ID: "fledge-alpha-free", ProviderID: "opencode"},
		{ID: "gpt-6.1-sol", ProviderID: "opencode"},
		{ID: "deepseek-v4.1-flash", ProviderID: "opencode-go"},
		{ID: "dup", ProviderID: "prov-a"},
		{ID: "dup", ProviderID: "prov-b"},
	}
}

func TestResolveQualified(t *testing.T) {
	r, err := Resolve("opencode-go/deepseek-v4.1-flash", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderID != "opencode-go" || r.ID != "deepseek-v4.1-flash" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveVariant(t *testing.T) {
	r, err := Resolve("opencode/claude-sonnet-5-5:high", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.Variant != "high" || r.ID != "claude-sonnet-5-5" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveDefaultVariantEmptied(t *testing.T) {
	r, err := Resolve("opencode/gpt-6.1-sol:default", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.Variant != "" {
		t.Fatalf("variant should be normalized to empty, got %q", r.Variant)
	}
}

func TestResolveBareUnique(t *testing.T) {
	r, err := Resolve("fledge-alpha-free", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderID != "opencode" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveBareAmbiguous(t *testing.T) {
	_, err := Resolve("dup", testModels())
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	_, err := Resolve("gpt-4o", testModels())
	if err == nil {
		t.Fatal("expected error")
	}
}

// 未知 provider 必须报 404，不能悄悄转发给上游。
func TestResolveUnknownProvider(t *testing.T) {
	_, err := Resolve("nonexistent/gpt-4o", testModels())
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("got %v", err)
	}
}

// 已知 provider 下的未知 model 仍放行，让上游报真实原因。
func TestResolveKnownProviderUnknownModel(t *testing.T) {
	r, err := Resolve("opencode/ghost-model", testModels())
	if err != nil {
		t.Fatalf("should pass through: %v", err)
	}
	if r.ID != "ghost-model" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveEmptyModel(t *testing.T) {
	if _, err := Resolve("", testModels()); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- 思考强度映射 ----------

func modelWithVariants(ids ...string) upstream.OCModel {
	m := upstream.OCModel{ID: "m", ProviderID: "p"}
	for _, id := range ids {
		var v struct {
			ID       string          `json:"id"`
			Settings json.RawMessage `json:"settings,omitempty"`
		}
		v.ID = id
		m.Variants = append(m.Variants, v)
	}
	return m
}

func TestPickVariantExact(t *testing.T) {
	m := modelWithVariants("low", "high", "max")
	for _, want := range []string{"low", "high", "max"} {
		got, ok := PickVariant(m, want)
		if !ok || got != want {
			t.Errorf("PickVariant(%q) = (%q,%v)", want, got, ok)
		}
	}
}

func TestPickVariantEmptyMeansDefault(t *testing.T) {
	m := modelWithVariants("low", "high")
	for _, in := range []string{"", "  ", "default", "DEFAULT", "auto"} {
		got, ok := PickVariant(m, in)
		if !ok || got != "" {
			t.Errorf("PickVariant(%q) = (%q,%v), want (\"\",true)", in, got, ok)
		}
	}
}

func TestPickVariantAliases(t *testing.T) {
	m := modelWithVariants("minimal", "medium", "xhigh", "max")
	cases := map[string]string{
		"min": "minimal", "minimum": "minimal",
		"mid": "medium", "med": "medium", "normal": "medium",
		"x-high": "xhigh", "extra-high": "xhigh", "extra_high": "xhigh", "very-high": "xhigh",
		"maximum": "max", "highest": "max",
	}
	for in, want := range cases {
		got, ok := PickVariant(m, in)
		if !ok || got != want {
			t.Errorf("PickVariant(%q) = (%q,%v), want %q", in, got, ok, want)
		}
	}
}

// 标准档位在模型上不存在时，取最接近的。
func TestPickVariantNearest(t *testing.T) {
	m := modelWithVariants("low", "high") // 没有 medium
	if got, _ := PickVariant(m, "medium"); got != "low" {
		t.Errorf("medium 应就近取 low（并列取更弱），got %q", got)
	}
	m2 := modelWithVariants("medium", "max") // 没有 low
	if got, _ := PickVariant(m2, "low"); got != "medium" {
		t.Errorf("low 应就近取 medium，got %q", got)
	}
	m3 := modelWithVariants("low", "high") // 没有 none
	if got, _ := PickVariant(m3, "none"); got != "low" {
		t.Errorf("none 应退到最低档 low，got %q", got)
	}
}

// 布尔档位模型：非 none 一律 thinking。
func TestPickVariantBooleanThinking(t *testing.T) {
	m := modelWithVariants("none", "thinking")
	if got, _ := PickVariant(m, "none"); got != "none" {
		t.Errorf("none → %q", got)
	}
	for _, in := range []string{"low", "medium", "high", "max"} {
		if got, _ := PickVariant(m, in); got != "thinking" {
			t.Errorf("%q → %q, want thinking", in, got)
		}
	}
}

func TestPickVariantNoVariants(t *testing.T) {
	m := modelWithVariants()
	if _, ok := PickVariant(m, "high"); ok {
		t.Error("无 variants 的模型应返回 false，让调用方忽略该参数")
	}
}

func TestPickVariantUnknownEffort(t *testing.T) {
	m := modelWithVariants("low", "high")
	if _, ok := PickVariant(m, "supersonic"); ok {
		t.Error("未知档位名应返回 false")
	}
}

func TestVariantIDsSortedByStrength(t *testing.T) {
	m := modelWithVariants("max", "none", "high", "minimal", "low")
	got := VariantIDs(m)
	want := []string{"none", "minimal", "low", "high", "max"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestToOpenAIExposesLimits(t *testing.T) {
	m := upstream.OCModel{
		ID: "claude-sonnet-5-5", ProviderID: "opencode",
		Limit: struct {
			Context int `json:"context"`
			Input   int `json:"input"`
			Output  int `json:"output"`
		}{Context: 1000000, Output: 131072},
	}
	m.Capabilities.Input = []string{"text", "image", "pdf"}
	m.Variants = modelWithVariants("low", "high", "max").Variants

	o := ToOpenAI(m)
	if o.ID != "opencode/claude-sonnet-5-5" || o.OwnedBy != "opencode" {
		t.Fatalf("%+v", o)
	}
	if o.ContextLength != 1000000 || o.MaxOutputTokens != 131072 {
		t.Fatalf("limits: %+v", o)
	}
	if strings.Join(o.SupportedReasoningEfforts, ",") != "low,high,max" {
		t.Fatalf("efforts: %v", o.SupportedReasoningEfforts)
	}
	if strings.Join(o.InputModalities, ",") != "text,image,pdf" {
		t.Fatalf("modalities: %v", o.InputModalities)
	}
}

// ---------- 模型详情与过滤 ----------

func richModel() upstream.OCModel {
	m := upstream.OCModel{
		ID: "claude-sonnet-5-5", ProviderID: "opencode",
		Name: "Claude Sonnet 5.5", Family: "claude-sonnet",
		Status: "active", Enabled: true,
	}
	m.Capabilities.Tools = true
	m.Capabilities.Input = []string{"text", "image", "pdf"}
	m.Capabilities.Output = []string{"text"}
	m.Limit.Context = 1000000
	m.Limit.Output = 128000
	m.Time.Released = 1790553600000
	m.Variants = modelWithVariants("low", "high", "max").Variants
	m.Cost = append(m.Cost, upstream.OCCost{Input: 2, Output: 10})
	m.Cost[0].Cache.Read = 0.2
	m.Cost[0].Cache.Write = 2.5
	return m
}

func TestToDetail(t *testing.T) {
	d := ToDetail(richModel())
	if d.ID != "opencode/claude-sonnet-5-5" || d.Name != "Claude Sonnet 5.5" {
		t.Fatalf("%+v", d)
	}
	if d.ContextLength != 1000000 || d.MaxOutputTokens != 128000 {
		t.Fatalf("limits: %d/%d", d.ContextLength, d.MaxOutputTokens)
	}
	if !d.ToolCalling || d.Capabilities == nil || !d.Capabilities.Tools {
		t.Fatalf("capabilities: %+v", d.Capabilities)
	}
	if strings.Join(d.SupportedReasoningEfforts, ",") != "low,high,max" {
		t.Fatalf("efforts: %v", d.SupportedReasoningEfforts)
	}
	if len(d.Variants) != 3 || d.Variants[0].ID != "low" {
		t.Fatalf("variants: %+v", d.Variants)
	}
	if len(d.Pricing) != 1 || d.Pricing[0].InputPerMillion != 2 ||
		d.Pricing[0].OutputPerMillion != 10 ||
		d.Pricing[0].CacheReadPerMillion != 0.2 {
		t.Fatalf("pricing: %+v", d.Pricing)
	}
}
