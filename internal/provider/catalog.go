package provider

import (
	"sort"
	"strings"
)

// Channel categories. They only drive grouping in the console; routing never
// depends on them.
const (
	ChannelVendor       = "vendor"       // first-party API
	ChannelRelay        = "relay"        // aggregator or reseller
	ChannelSubscription = "subscription" // consumer plan reached through OAuth
	ChannelLocal        = "local"        // self-hosted gateway
)

// Channel is one entry of the upstream catalogue. It is static data, not a
// stored resource: providers reference a channel by id only for display and for
// pre-filling the "add upstream" form.
//
// Protocols lists what the upstream speaks natively (chat = OpenAI chat
// completions, responses = OpenAI Responses, anthropic = Messages, gemini =
// generateContent). Kind is what janus can actually speak to it today, so a
// channel with gemini in Protocols but Kind=custom is visible in the catalogue
// without pretending the gateway supports it yet.
type Channel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Category  string   `json:"category"`
	Kind      Kind     `json:"kind"`
	Protocols []string `json:"protocols"`
	Auth      []string `json:"auth"`
	// Login names the one-click sign-in this channel has today: "oauth" for the
	// built-in authorization-code flows, "device_code" for GitHub. Empty means
	// the channel supports OAuth in principle but janus needs an operator
	// supplied client (custom oauth config) before a button can work.
	Login       string   `json:"login,omitempty"`
	BaseURL     string   `json:"base_url"`
	AltBaseURLs []string `json:"alt_base_urls,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// channelCatalog is the single source of truth for the upstream list. Entries
// were compared against sub2api's platform registry and magpie's provider
// presets; only stable, documented endpoints are included, and none of their
// code is copied.
var channelCatalog = []Channel{
	// --- consumer subscriptions (OAuth) ---
	{
		ID: "openai-codex", Name: "OpenAI Codex (ChatGPT plan)", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"responses", "chat"}, Auth: []string{"oauth"},
		Login:   "oauth",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		Note:    "OAuth only; Responses protocol, needs the Codex adapter.",
	},
	{
		ID: "claude-code", Name: "Claude Code (Anthropic plan)", Category: ChannelSubscription,
		Kind: KindAnthropic, Protocols: []string{"anthropic"}, Auth: []string{"oauth"},
		Login:   "oauth",
		BaseURL: "https://api.anthropic.com",
		Note:    "OAuth token needs the anthropic-beta oauth header.",
	},
	{
		ID: "workbuddy", Name: "WorkBuddy / CodeBuddy (Tencent plan)", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"oauth"},
		BaseURL: "https://copilot.tencent.com/v2",
		Note:    "Tencent CodeBuddy plan. Uses X-User-Id style headers and a vendor-specific refresh endpoint; import an existing login or configure a custom OAuth flow.",
	},
	{
		ID: "workbuddy-ai", Name: "WorkBuddy AI (international)", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"oauth"},
		BaseURL: "https://www.workbuddy.ai/v2",
		Note:    "Same shape as the China build, separate account store.",
	},
	{
		ID: "github-copilot", Name: "GitHub Copilot", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"device_code"},
		Login:   "device_code",
		BaseURL: "https://api.githubcopilot.com",
		Note:    "GitHub device flow, then exchange for a short-lived Copilot token.",
	},
	{
		ID: "google-code-assist", Name: "Google Code Assist / Gemini CLI", Category: ChannelSubscription,
		Kind: KindCustom, Protocols: []string{"gemini"}, Auth: []string{"oauth"},
		BaseURL:     "https://cloudcode-pa.googleapis.com",
		AltBaseURLs: []string{"https://daily-cloudcode-pa.googleapis.com"},
		Note:        "Gemini protocol; a Gemini adapter and a custom OAuth client are required before login can work.",
	},
	{
		ID: "antigravity", Name: "Google Antigravity", Category: ChannelSubscription,
		Kind: KindCustom, Protocols: []string{"gemini"}, Auth: []string{"oauth"},
		BaseURL: "https://cloudcode-pa.googleapis.com",
		Note:    "Same Code Assist quota as Gemini CLI; needs a custom OAuth client.",
	},
	{
		ID: "command-code", Name: "Command Code", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL: "https://api.commandcode.ai/provider/v1",
	},
	{
		ID: "cline", Name: "Cline", Category: ChannelSubscription,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key", "oauth"},
		BaseURL: "https://api.cline.bot/api/v1",
		Note:    "Cline's OAuth lives in its editor extension; use an API key or configure a custom OAuth client.",
	},

	// --- first-party vendors ---
	{
		ID: "openai", Name: "OpenAI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses"}, Auth: []string{"api_key"},
		BaseURL: "https://api.openai.com/v1",
	},
	{
		ID: "anthropic", Name: "Anthropic", Category: ChannelVendor,
		Kind: KindAnthropic, Protocols: []string{"anthropic"}, Auth: []string{"api_key"},
		BaseURL: "https://api.anthropic.com",
	},
	{
		ID: "google-ai-studio", Name: "Google AI Studio", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "gemini"}, Auth: []string{"api_key"},
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
	},
	{
		ID: "deepseek", Name: "DeepSeek", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.deepseek.com/v1",
		AltBaseURLs: []string{"https://api.deepseek.com/anthropic"},
	},
	{
		ID: "moonshot", Name: "Moonshot Kimi", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.moonshot.cn/v1",
		AltBaseURLs: []string{"https://api.moonshot.ai/v1"},
	},
	{
		ID: "kimi-code", Name: "Kimi Coding Plan", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL: "https://api.kimi.com/coding/v1",
	},
	{
		ID: "zhipu", Name: "Zhipu GLM", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://open.bigmodel.cn/api/paas/v4",
		AltBaseURLs: []string{"https://open.bigmodel.cn/api/coding/paas/v4", "https://open.bigmodel.cn/api/anthropic"},
	},
	{
		ID: "minimax", Name: "MiniMax", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.minimaxi.com/v1",
		AltBaseURLs: []string{"https://api.minimaxi.com/anthropic"},
	},
	{
		ID: "qwen", Name: "Alibaba Qwen (DashScope intl)", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		AltBaseURLs: []string{"https://dashscope.aliyuncs.com/compatible-mode/v1"},
	},
	{
		ID: "volcengine-ark", Name: "Volcengine Ark", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://ark.cn-beijing.volces.com/api/v3",
		AltBaseURLs: []string{"https://ark.cn-beijing.volces.com/api/coding/v3"},
	},
	{
		ID: "tencent-hunyuan", Name: "Tencent Hunyuan", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.hunyuan.cloud.tencent.com/v1",
	},
	{
		ID: "baidu-qianfan", Name: "Baidu Qianfan", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://qianfan.baidubce.com/v2",
	},
	{
		ID: "longcat", Name: "Meituan LongCat", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.longcat.chat/openai",
		AltBaseURLs: []string{"https://api.longcat.chat/anthropic"},
	},
	{
		ID: "siliconflow", Name: "SiliconFlow", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.siliconflow.cn/v1",
		AltBaseURLs: []string{"https://api.siliconflow.com/v1"},
	},
	{
		ID: "stepfun", Name: "StepFun", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.stepfun.com/v1",
	},
	{
		ID: "lingyi", Name: "01.AI Yi", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.lingyiwanwu.com/v1",
	},
	{
		ID: "baichuan", Name: "Baichuan", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.baichuan-ai.com/v1",
	},
	{
		ID: "spark", Name: "iFlytek Spark", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://spark-api-open.xf-yun.com/v1",
	},
	{
		ID: "xai", Name: "xAI Grok", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses"}, Auth: []string{"api_key"},
		BaseURL: "https://api.x.ai/v1",
	},
	{
		ID: "mistral", Name: "Mistral AI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL:     "https://api.mistral.ai/v1",
		AltBaseURLs: []string{"https://codestral.mistral.ai/v1"},
	},
	{
		ID: "cohere", Name: "Cohere", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.cohere.ai/compatibility/v1",
	},
	{
		ID: "perplexity", Name: "Perplexity", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.perplexity.ai",
	},
	{
		ID: "groq", Name: "Groq", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.groq.com/openai/v1",
	},
	{
		ID: "together", Name: "Together AI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.together.xyz/v1",
	},
	{
		ID: "fireworks", Name: "Fireworks AI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.fireworks.ai/inference/v1",
	},
	{
		ID: "cerebras", Name: "Cerebras", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.cerebras.ai/v1",
	},
	{
		ID: "deepinfra", Name: "DeepInfra", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.deepinfra.com/v1/openai",
	},
	{
		ID: "novita", Name: "Novita AI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.novita.ai/v3/openai",
	},
	{
		ID: "hyperbolic", Name: "Hyperbolic", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.hyperbolic.xyz/v1",
	},
	{
		ID: "sambanova", Name: "SambaNova", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.sambanova.ai/v1",
	},
	{
		ID: "nebius", Name: "Nebius AI Studio", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.studio.nebius.ai/v1",
	},
	{
		ID: "scaleway", Name: "Scaleway Generative APIs", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.scaleway.ai/v1",
	},
	{
		ID: "modelscope", Name: "ModelScope", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api-inference.modelscope.cn/v1",
	},
	{
		ID: "cloudflare", Name: "Cloudflare Workers AI", Category: ChannelVendor,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1",
		Note:    "Replace {account_id} with the account id.",
	},

	// --- relays / gateways ---
	{
		ID: "openrouter", Name: "OpenRouter", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses"}, Auth: []string{"api_key"},
		BaseURL: "https://openrouter.ai/api/v1",
	},
	{
		ID: "vercel", Name: "Vercel AI Gateway", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses", "anthropic"}, Auth: []string{"api_key"},
		BaseURL: "https://ai-gateway.vercel.sh/v1",
	},
	{
		ID: "opencode-go", Name: "OpenCode Go", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL:     "https://opencode.ai/zen/go/v1",
		AltBaseURLs: []string{"https://opencode.ai/zen/v1"},
	},
	{
		ID: "opencode-zen", Name: "OpenCode Zen", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat", "anthropic"}, Auth: []string{"api_key", "oauth"},
		BaseURL: "https://opencode.ai/zen/v1",
		Note:    "OpenCode keeps provider sign-ins in auth.json; import them with source=opencode.",
	},
	{
		ID: "typesafe", Name: "TypeSafe", Category: ChannelRelay,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "https://api.typesafe.ai/v1/systemone",
	},

	// --- self-hosted ---
	{
		ID: "ollama", Name: "Ollama", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"none"},
		BaseURL: "http://127.0.0.1:11434/v1",
	},
	{
		ID: "lmstudio", Name: "LM Studio", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"none"},
		BaseURL: "http://127.0.0.1:1234/v1",
	},
	{
		ID: "vllm", Name: "vLLM / SGLang", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "http://127.0.0.1:8000/v1",
	},
	{
		ID: "litellm", Name: "LiteLLM Proxy", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "http://127.0.0.1:4000/v1",
	},
	{
		ID: "one-api", Name: "one-api / new-api", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat"}, Auth: []string{"api_key"},
		BaseURL: "http://127.0.0.1:3000/v1",
	},
	{
		ID: "custom", Name: "Custom (any OpenAI-compatible)", Category: ChannelLocal,
		Kind: KindOpenAI, Protocols: []string{"chat", "responses"}, Auth: []string{"api_key", "none"},
		BaseURL: "",
	},
}

// Channels returns the catalogue sorted by category then id. The slice is a
// copy so callers cannot mutate the registry.
func Channels() []Channel {
	out := make([]Channel, len(channelCatalog))
	copy(out, channelCatalog)
	rank := map[string]int{ChannelVendor: 0, ChannelSubscription: 1, ChannelRelay: 2, ChannelLocal: 3}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Category] != rank[out[j].Category] {
			return rank[out[i].Category] < rank[out[j].Category]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ChannelByID looks up one entry. The lookup is case-insensitive because ids
// arrive from a form.
func ChannelByID(id string) (Channel, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, channel := range channelCatalog {
		if channel.ID == id {
			return channel, true
		}
	}
	return Channel{}, false
}

// KnownChannel reports whether id names a catalogue entry. An empty id is
// accepted because providers created before the catalogue existed carry none.
func KnownChannel(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return true
	}
	_, ok := ChannelByID(id)
	return ok
}
