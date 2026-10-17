// Package openai 定义 OpenAI 线格式（Chat Completions / Models）的类型。
//
// 这里只有"对外协议长什么样"，不含任何转换或业务逻辑：
// 服务端 handler 与内部消息模型都引用它，但谁也不把协议细节写进自己的包。
package openai

import (
	"encoding/json"

	"janus/internal/protocol/shared"
)

// ---------- 请求 ----------

type ChatRequest struct {
	Model         string               `json:"model"`
	Messages      []shared.ChatMessage `json:"messages"`
	Stream        bool                 `json:"stream"`
	StreamOptions *StreamOptions       `json:"stream_options,omitempty"`
	MaxTokens     *int                 `json:"max_tokens,omitempty"`
	// MaxCompletionTokens 是 OpenAI 新字段；上游不支持输出上限，接受但忽略
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
	// ReasoningEffort 映射到上游 model variant（思考强度）
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Tools           []shared.ToolSpec `json:"tools,omitempty"`
	ToolChoice      json.RawMessage   `json:"tool_choice,omitempty"`
	User            string            `json:"user,omitempty"`
	// ParallelToolCalls=false 时桥会把同一轮里的多个工具调用串行返回
	// （每次只给客户端一个，等它回填结果再给下一个）。默认（缺省/true）并行返回。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// temperature / top_p / n / stop / logprobs 等 OpenCode 不支持的字段
	// 会被 encoding/json 静默忽略 —— 这正是我们要的宽容行为。
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ParallelDefault 返回是否允许一轮并行返回多个 tool_calls（OpenAI 默认 true）。
func ParallelDefault(p *bool) bool { return p == nil || *p }

// ---------- 响应 ----------

type ChatResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []Choice      `json:"choices"`
	Usage   *shared.Usage `json:"usage"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      AssistantMsg    `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type AssistantMsg struct {
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []shared.ToolCall `json:"tool_calls,omitempty"`
}

type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *shared.Usage `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string            `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	ReasoningContent *string           `json:"reasoning_content,omitempty"`
	ToolCalls        []shared.ToolCall `json:"tool_calls,omitempty"`
}

type (
	Usage             = shared.Usage
	TokenDetails      = shared.TokenDetails
	CompletionDetails = shared.CompletionDetails
)

// ---------- 模型目录 ----------

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

	// WorkBuddyCredits 是 WorkBuddy 模型的计费倍率（如 "x0.79"，来自上游目录
	// billing.credits）。非 workbuddy provider 的条目不带；x_ 前缀表明非标扩展。
	WorkBuddyCredits string `json:"x_workbuddy_credits,omitempty"`
}

type ModelList struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ---------- OpenAI 格式错误 ----------

type (
	Error     = shared.Error
	ErrorBody = shared.ErrorBody
)
