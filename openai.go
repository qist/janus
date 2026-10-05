package main

import (
	"encoding/json"
	"strings"
)

// ---------- 请求 ----------

type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	// MaxCompletionTokens 是 OpenAI 新字段；上游不支持输出上限，接受但忽略
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort 映射到上游 model variant（思考强度）
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           []ToolSpec      `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	User            string          `json:"user,omitempty"`
	// ParallelToolCalls=false 时桥会把同一轮里的多个工具调用串行返回
	// （每次只给客户端一个，等它回填结果再给下一个）。默认（缺省/true）并行返回。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// temperature / top_p / n / stop / logprobs 等 OpenCode 不支持的字段
	// 会被 encoding/json 静默忽略 —— 这正是我们要的宽容行为。
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// parallelDefault 返回是否允许一轮并行返回多个 tool_calls（OpenAI 默认 true）。
func parallelDefault(p *bool) bool { return p == nil || *p }

type ToolSpec struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 是 OpenAI 的 function 定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ChatMessage struct {
	Role             string         `json:"role"`
	Content          MessageContent `json:"content"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function *FunctionCall `json:"function,omitempty"`
	// Index 只在流式 delta 里出现（OpenAI 用它对应并发工具调用的序号）。
	// 非流式响应的 message.tool_calls 不带 index，所以用指针区分"未设置"。
	Index *int `json:"index,omitempty"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// MessageContent 兼容 content 为 string | array | null 三种形态。
type MessageContent struct {
	Text    string
	Parts   []ContentPart
	IsArray bool
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func (m *MessageContent) UnmarshalJSON(b []byte) error {
	*m = MessageContent{}
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.Text = s
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(b, &parts); err == nil {
		m.Parts = parts
		m.IsArray = true
		m.Text = partsText(parts)
		return nil
	}
	// 未知形态：整段当作文本保留，绝不丢数据。
	m.Text = strings.TrimSpace(string(b))
	return nil
}

func (m MessageContent) MarshalJSON() ([]byte, error) {
	if m.IsArray {
		return json.Marshal(m.Parts)
	}
	return json.Marshal(m.Text)
}

func partsText(parts []ContentPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// Images 提取全部图片 data URI。
func (m MessageContent) Images() []string {
	var out []string
	for _, p := range m.Parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			out = append(out, p.ImageURL.URL)
		}
	}
	return out
}

// ---------- 响应 ----------

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      AssistantMsg    `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type AssistantMsg struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          *string    `json:"content,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *TokenDetails      `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionDetails `json:"completion_tokens_details,omitempty"`

	// DeepSeek 风格的缓存明细（不少国产客户端/网关用它统计缓存命中率）。
	// 语义：prompt_tokens = hit + miss。
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
}

type TokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// CacheCreationTokens 为写入缓存的 token（上游 cache.write）。OpenAI 无此字段，
	// 但网关普遍透出，标准客户端会忽略。
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type CompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// 以下为扩展字段（OpenAI 标准没有，但网关普遍提供，标准客户端会忽略）
	ContextLength             int      `json:"context_length,omitempty"`
	MaxOutputTokens           int      `json:"max_output_tokens,omitempty"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	InputModalities           []string `json:"input_modalities,omitempty"`

	// Free 标记该模型所有价格档位都是 0（上游的免费模型）。
	Free bool `json:"free,omitempty"`
}

type ModelList struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ---------- OpenAI 格式错误 ----------

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    string `json:"code,omitempty"`
}

type openAIErrorBody struct {
	Error openAIError `json:"error"`
}

func strPtr(s string) *string { return &s }
