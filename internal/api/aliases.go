package api

import (
	"janus/internal/modelx"
	"janus/internal/protocol/openai"
	"janus/internal/protocol/shared"
)

// aliases.go —— HTTP 层对协议词汇的本地别名。
//
// 真正的类型定义在 internal/protocol（线格式）与 internal/modelx（目录投影），
// 这里只做别名，让 handler 里读起来仍是 ChatMessage / ChatResponse 这类说法。
// 本包不定义任何协议结构：改协议字段只需动 internal/protocol 一处。

// ---------- 请求 ----------

type (
	ChatRequest   = openai.ChatRequest
	StreamOptions = openai.StreamOptions
)

// parallelDefault 返回是否允许一轮并行返回多个 tool_calls（OpenAI 默认 true）。
func parallelDefault(p *bool) bool { return openai.ParallelDefault(p) }

// ---------- 消息词汇 ----------

type (
	ToolSpec     = shared.ToolSpec
	ToolFunction = shared.ToolFunction
	ChatMessage  = shared.ChatMessage
	ToolCall     = shared.ToolCall
	FunctionCall = shared.FunctionCall

	MessageContent = shared.MessageContent
	ContentPart    = shared.ContentPart
	ImageURL       = shared.ImageURL
)

// ---------- 响应 ----------

type (
	ChatResponse      = openai.ChatResponse
	Choice            = openai.Choice
	AssistantMsg      = openai.AssistantMsg
	ChatChunk         = openai.ChatChunk
	ChunkChoice       = openai.ChunkChoice
	Delta             = openai.Delta
	Usage             = openai.Usage
	TokenDetails      = openai.TokenDetails
	CompletionDetails = openai.CompletionDetails

	ModelObject = openai.ModelObject
	ModelList   = openai.ModelList
	ModelDetail = modelx.Detail

	openAIError     = openai.Error
	openAIErrorBody = openai.ErrorBody
)
