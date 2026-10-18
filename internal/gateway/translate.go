package gateway

// translate.go —— anthropic-messages ↔ openai-chat 结构翻译（REFACTOR §3.4）。
//
// 只在「客户端协议 ≠ provider.type」时启用，且只做结构映射，不改语义：
//   - 请求：system / messages / tools / tool_choice / 采样参数逐项对应；
//   - 响应：content blocks ↔ message.content + tool_calls，stop_reason ↔ finish_reason；
//   - 流式：Anthropic 事件序列 ↔ OpenAI chunk 序列。
//
// 无法一一对应的字段（如 Anthropic 的 top_k、OpenAI 的 logprobs）直接丢弃，
// 不猜测、不伪造。openai-responses 是第三套线格式，暂不参与翻译。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"janus/internal/protocol/shared"
)

// defaultAnthropicMaxTokens 是 OpenAI → Anthropic 时缺失 max_tokens 的兜底。
const defaultAnthropicMaxTokens = 4096

// CanTranslate 判断两条线格式之间是否有翻译实现。
func CanTranslate(from, to string) bool {
	return (from == ProtocolAnthropic && to == ProtocolOpenAIChat) ||
		(from == ProtocolOpenAIChat && to == ProtocolAnthropic)
}

// ---------- 线格式类型（只覆盖翻译需要的字段） ----------

type anthRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthMessage   `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	Source *anthImageSource `json:"source,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthResponse struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Content    []anthBlock   `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      anthropicUage `json:"usage"`
}

type anthropicUage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type oaiRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Tools               []oaiTool       `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type oaiResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []wireToolCall  `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type oaiChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string         `json:"role,omitempty"`
			Content   *string        `json:"content,omitempty"`
			ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ---------- 请求 ----------

// TranslateRequest 把客户端线格式的请求体翻译成厂商线格式。
func TranslateRequest(from, to string, body []byte) ([]byte, error) {
	switch {
	case from == ProtocolAnthropic && to == ProtocolOpenAIChat:
		return anthropicToOpenAIRequest(body)
	case from == ProtocolOpenAIChat && to == ProtocolAnthropic:
		return openAIToAnthropicRequest(body)
	}
	return nil, fmt.Errorf("unsupported translation %s → %s", from, to)
}

func anthropicToOpenAIRequest(body []byte) ([]byte, error) {
	var in anthRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := oaiRequest{
		Model:       in.Model,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if in.MaxTokens > 0 {
		n := in.MaxTokens
		out.MaxTokens = &n
	}
	if len(in.StopSequences) > 0 {
		out.Stop, _ = json.Marshal(in.StopSequences)
	}

	if sys := anthropicSystemText(in.System); strings.TrimSpace(sys) != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: rawJSONString(sys)})
	}

	for _, m := range in.Messages {
		out.Messages = append(out.Messages, anthropicMessageToOpenAI(m)...)
	}

	for _, t := range in.Tools {
		// 服务端工具（web_search 等）没有对应的 OpenAI function，跳过。
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		if t.Name == "" {
			continue
		}
		var tool oaiTool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.InputSchema
		if len(tool.Function.Parameters) == 0 {
			tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, tool)
	}
	if len(out.Tools) == 0 {
		out.Tools = nil
	}
	out.ToolChoice = anthropicToolChoiceToOpenAI(in.ToolChoice)
	return json.Marshal(out)
}

// anthropicMessageToOpenAI 把一条 Anthropic 消息拆成一条或多条 OpenAI 消息
// （tool_result 要单独成为 role:"tool"）。
func anthropicMessageToOpenAI(m anthMessage) []oaiMessage {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []oaiMessage{{Role: orDefault(m.Role, "user"), Content: rawJSONString(s)}}
	}
	var blocks []anthBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}

	if m.Role == "assistant" {
		var text strings.Builder
		var calls []wireToolCall
		for _, blk := range blocks {
			switch blk.Type {
			case "", "text":
				text.WriteString(blk.Text)
			case "tool_use":
				args := string(blk.Input)
				if args == "" || args == "null" {
					args = "{}"
				}
				var call wireToolCall
				call.ID = blk.ID
				call.Type = "function"
				call.Function.Name = blk.Name
				call.Function.Arguments = args
				calls = append(calls, call)
			}
		}
		if text.Len() == 0 && len(calls) == 0 {
			return nil
		}
		return []oaiMessage{{Role: "assistant", Content: rawJSONString(text.String()), ToolCalls: calls}}
	}

	var out []oaiMessage
	var text strings.Builder
	var parts []oaiContentPart
	for _, blk := range blocks {
		switch blk.Type {
		case "", "text":
			text.WriteString(blk.Text)
		case "image":
			if u := anthropicImageURI(blk); u != "" {
				var p oaiContentPart
				p.Type = "image_url"
				p.ImageURL = &struct {
					URL string `json:"url"`
				}{URL: u}
				parts = append(parts, p)
			}
		case "tool_result":
			out = append(out, oaiMessage{
				Role:       "tool",
				ToolCallID: blk.ToolUseID,
				Content:    rawJSONString(anthropicToolResultText(blk.Content)),
			})
		}
	}
	if text.Len() > 0 || len(parts) > 0 {
		if len(parts) == 0 {
			out = append(out, oaiMessage{Role: "user", Content: rawJSONString(text.String())})
		} else {
			if text.Len() > 0 {
				parts = append([]oaiContentPart{{Type: "text", Text: text.String()}}, parts...)
			}
			b, _ := json.Marshal(parts)
			out = append(out, oaiMessage{Role: "user", Content: b})
		}
	}
	return out
}

func openAIToAnthropicRequest(body []byte) ([]byte, error) {
	var in oaiRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	out := anthRequest{
		Model:       in.Model,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		MaxTokens:   defaultAnthropicMaxTokens,
	}
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		out.MaxTokens = *in.MaxTokens
	}
	if in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0 {
		out.MaxTokens = *in.MaxCompletionTokens
	}
	out.StopSequences = openAIStopSequences(in.Stop)

	var system []string
	var pending []anthBlock
	flush := func() {
		if len(pending) == 0 {
			return
		}
		b, _ := json.Marshal(pending)
		out.Messages = append(out.Messages, anthMessage{Role: "user", Content: b})
		pending = nil
	}

	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			if s := openAIContentText(m.Content); s != "" {
				system = append(system, s)
			}
		case "tool":
			pending = append(pending, anthBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   rawJSONString(openAIContentText(m.Content)),
			})
		case "assistant":
			flush()
			blocks := openAIAssistantBlocks(m)
			if len(blocks) == 0 {
				continue
			}
			b, _ := json.Marshal(blocks)
			out.Messages = append(out.Messages, anthMessage{Role: "assistant", Content: b})
		default:
			flush()
			blocks := openAIUserBlocks(m.Content)
			if len(blocks) == 0 {
				continue
			}
			b, _ := json.Marshal(blocks)
			out.Messages = append(out.Messages, anthMessage{Role: "user", Content: b})
		}
	}
	flush()

	if len(system) > 0 {
		out.System = rawJSONString(strings.Join(system, "\n"))
	}
	for _, t := range in.Tools {
		if t.Function.Name == "" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthTool{
			Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema,
		})
	}
	out.ToolChoice = openAIToolChoiceToAnthropic(in.ToolChoice)
	return json.Marshal(out)
}

func openAIAssistantBlocks(m oaiMessage) []anthBlock {
	var out []anthBlock
	if text := openAIContentText(m.Content); text != "" {
		out = append(out, anthBlock{Type: "text", Text: text})
	}
	for _, tc := range m.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, anthBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args),
		})
	}
	return out
}

func openAIUserBlocks(raw json.RawMessage) []anthBlock {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []anthBlock{{Type: "text", Text: s}}
	}
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []anthBlock
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			if p.Text != "" {
				out = append(out, anthBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			if src := imageSourceFromURL(p.ImageURL.URL); src != nil {
				out = append(out, anthBlock{Type: "image", Source: src})
			}
		}
	}
	return out
}

func imageSourceFromURL(u string) *anthImageSource {
	if strings.HasPrefix(u, "data:") {
		// data:<media-type>;base64,<payload>
		rest := strings.TrimPrefix(u, "data:")
		i := strings.Index(rest, ",")
		if i < 0 {
			return nil
		}
		meta, data := rest[:i], rest[i+1:]
		mediaType := strings.TrimSuffix(meta, ";base64")
		if mediaType == "" {
			mediaType = "image/png"
		}
		return &anthImageSource{Type: "base64", MediaType: mediaType, Data: data}
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return &anthImageSource{Type: "url", URL: u}
	}
	return nil
}

// ---------- 响应 ----------

// TranslateResponse 把厂商线格式的响应体翻译回客户端线格式。
// from = provider.type，to = 客户端协议；model 非空时覆盖响应里的 model。
func TranslateResponse(from, to string, body []byte, model string) ([]byte, error) {
	switch {
	case from == ProtocolAnthropic && to == ProtocolOpenAIChat:
		return anthropicToOpenAIResponse(body, model)
	case from == ProtocolOpenAIChat && to == ProtocolAnthropic:
		return openAIToAnthropicResponse(body, model)
	}
	return nil, fmt.Errorf("unsupported translation %s → %s", from, to)
}

func anthropicToOpenAIResponse(body []byte, model string) ([]byte, error) {
	var in anthResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	var text strings.Builder
	var calls []wireToolCall
	for _, c := range in.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := string(c.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			var call wireToolCall
			call.ID = c.ID
			call.Type = "function"
			call.Function.Name = c.Name
			call.Function.Arguments = args
			calls = append(calls, call)
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	out := map[string]any{
		"id":      orDefault(in.ID, "chatcmpl-"+shared.NewID()[:24]),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   pickModel(model, in.Model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg,
			"finish_reason": anthropicFinishToOpenAI(in.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     in.Usage.InputTokens,
			"completion_tokens": in.Usage.OutputTokens,
			"total_tokens":      in.Usage.InputTokens + in.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

func openAIToAnthropicResponse(body []byte, model string) ([]byte, error) {
	var in oaiResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	var content []anthBlock
	stop := "end_turn"
	if len(in.Choices) > 0 {
		ch := in.Choices[0]
		if text := openAIContentText(ch.Message.Content); text != "" {
			content = append(content, anthBlock{Type: "text", Text: text})
		}
		for _, tc := range ch.Message.ToolCalls {
			args := tc.Function.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			content = append(content, anthBlock{
				Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args),
			})
		}
		stop = openAIFinishToAnthropic(ch.FinishReason)
	}
	if content == nil {
		content = []anthBlock{}
	}
	inTok, outTok := 0, 0
	if in.Usage != nil {
		inTok, outTok = in.Usage.PromptTokens, in.Usage.CompletionTokens
	}
	out := map[string]any{
		"id":            orDefault(in.ID, "msg_"+shared.NewID()[:24]),
		"type":          "message",
		"role":          "assistant",
		"model":         pickModel(model, in.Model),
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	}
	return json.Marshal(out)
}

// ---------- 流式 ----------

// TranslateStream 把厂商 SSE 逐事件翻译成客户端 SSE 并写入 w。
// 调用方负责按需 flush（通常传入带 flush 的 writer）。
func TranslateStream(w io.Writer, r io.Reader, from, to, model string) error {
	switch {
	case from == ProtocolOpenAIChat && to == ProtocolAnthropic:
		return openAIToAnthropicStream(w, r, model)
	case from == ProtocolAnthropic && to == ProtocolOpenAIChat:
		return anthropicToOpenAIStream(w, r, model)
	}
	return fmt.Errorf("unsupported stream translation %s → %s", from, to)
}

type sseEvent struct {
	name string
	data string
}

// readSSE 读一个完整事件（以空行分隔），忽略注释与其他字段。
func readSSE(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var data []string
	for {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			return ev, err
		}
		t := strings.TrimRight(line, "\r\n")
		if t == "" {
			if len(data) > 0 || ev.name != "" {
				ev.data = strings.Join(data, "\n")
				return ev, nil
			}
			if err != nil {
				return ev, err
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "event:"):
			ev.name = strings.TrimSpace(strings.TrimPrefix(t, "event:"))
		case strings.HasPrefix(t, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(t, "data:")))
		}
		if err != nil {
			if len(data) > 0 {
				ev.data = strings.Join(data, "\n")
				return ev, nil
			}
			return ev, err
		}
	}
}

type anthEmitter struct{ w io.Writer }

func (e anthEmitter) event(typ string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", typ, b)
}

func openAIToAnthropicStream(w io.Writer, r io.Reader, model string) error {
	br := bufio.NewReaderSize(r, 64<<10)
	em := anthEmitter{w: w}

	id := "msg_" + shared.NewID()[:24]
	started := false
	nextBlock := 0
	textOpen := false
	textIdx := 0
	toolBlock := map[int]int{} // OpenAI tool index → Anthropic block index
	var toolOrder []int
	finish := ""
	inTok, outTok := 0, 0

	closeText := func() {
		if textOpen {
			em.event("content_block_stop", map[string]any{"index": textIdx})
			textOpen = false
		}
	}

	for {
		ev, err := readSSE(br)
		if ev.data != "" && ev.data != "[DONE]" {
			var ch oaiChunk
			if json.Unmarshal([]byte(ev.data), &ch) == nil {
				if !started {
					started = true
					if ch.ID != "" {
						id = "msg_" + strings.TrimPrefix(ch.ID, "chatcmpl-")
					}
					em.event("message_start", map[string]any{"message": map[string]any{
						"id": id, "type": "message", "role": "assistant",
						"model":   pickModel(model, ch.Model),
						"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
						"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
					}})
				}
				if ch.Usage != nil {
					inTok, outTok = ch.Usage.PromptTokens, ch.Usage.CompletionTokens
				}
				for _, c := range ch.Choices {
					if c.Delta.Content != nil && *c.Delta.Content != "" {
						if !textOpen {
							textIdx = nextBlock
							nextBlock++
							em.event("content_block_start", map[string]any{
								"index": textIdx, "content_block": map[string]any{"type": "text", "text": ""}})
							textOpen = true
						}
						em.event("content_block_delta", map[string]any{
							"index": textIdx, "delta": map[string]any{"type": "text_delta", "text": *c.Delta.Content}})
					}
					for _, tc := range c.Delta.ToolCalls {
						closeText()
						// 非流式 tool_calls 不带 index，这里用 0 兜底。
						key := 0
						if tc.Index != nil {
							key = *tc.Index
						}
						idx, ok := toolBlock[key]
						if !ok {
							idx = nextBlock
							nextBlock++
							toolBlock[key] = idx
							toolOrder = append(toolOrder, key)
							em.event("content_block_start", map[string]any{
								"index": idx, "content_block": map[string]any{
									"type": "tool_use", "id": tc.ID, "name": tc.Function.Name,
									"input": map[string]any{}}})
						}
						if tc.Function.Arguments != "" {
							em.event("content_block_delta", map[string]any{
								"index": idx, "delta": map[string]any{
									"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
						}
					}
					if c.FinishReason != nil && *c.FinishReason != "" {
						finish = *c.FinishReason
					}
				}
			}
		}
		if ev.data == "[DONE]" {
			break
		}
		if err != nil {
			break
		}
	}

	if !started {
		// 上游一个事件都没有：不要伪造 message_start，交给调用方收尾。
		return nil
	}
	closeText()
	for _, i := range toolOrder {
		em.event("content_block_stop", map[string]any{"index": toolBlock[i]})
	}
	em.event("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": openAIFinishToAnthropic(finish), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	})
	em.event("message_stop", nil)
	return nil
}

func anthropicToOpenAIStream(w io.Writer, r io.Reader, model string) error {
	br := bufio.NewReaderSize(r, 64<<10)

	id := "chatcmpl-" + shared.NewID()[:24]
	created := time.Now().Unix()
	modelName := model
	sentRole := false
	finishSent := false
	toolIdx := map[int]int{} // Anthropic block index → OpenAI tool index
	nextTool := 0
	inTok, outTok := 0, 0

	emit := func(delta map[string]any, finish any, usage map[string]any) {
		ch := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": modelName,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			ch["usage"] = usage
		}
		b, err := json.Marshal(ch)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}

	for {
		ev, err := readSSE(br)
		if ev.data != "" && ev.data != "[DONE]" {
			var e struct {
				Type    string `json:"type"`
				Message *struct {
					ID    string `json:"id"`
					Model string `json:"model"`
				} `json:"message"`
				Index        int `json:"index"`
				ContentBlock *struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
				Delta *struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					StopReason  string `json:"stop_reason"`
				} `json:"delta"`
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(ev.data), &e) == nil {
				switch e.Type {
				case "message_start":
					if e.Message != nil && modelName == "" {
						modelName = e.Message.Model
					}
					emit(map[string]any{"role": "assistant", "content": ""}, nil, nil)
					sentRole = true
				case "content_block_start":
					if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
						idx := nextTool
						nextTool++
						toolIdx[e.Index] = idx
						emit(map[string]any{"tool_calls": []any{map[string]any{
							"index": idx, "id": e.ContentBlock.ID, "type": "function",
							"function": map[string]any{"name": e.ContentBlock.Name, "arguments": ""},
						}}}, nil, nil)
					}
				case "content_block_delta":
					if e.Delta != nil {
						switch e.Delta.Type {
						case "text_delta":
							if e.Delta.Text != "" {
								emit(map[string]any{"content": e.Delta.Text}, nil, nil)
							}
						case "input_json_delta":
							if e.Delta.PartialJSON != "" {
								idx, ok := toolIdx[e.Index]
								if !ok {
									idx = 0
								}
								emit(map[string]any{"tool_calls": []any{map[string]any{
									"index":    idx,
									"function": map[string]any{"arguments": e.Delta.PartialJSON},
								}}}, nil, nil)
							}
						}
					}
				case "message_delta":
					if e.Usage != nil {
						inTok, outTok = e.Usage.InputTokens, e.Usage.OutputTokens
					}
					fr := "stop"
					if e.Delta != nil {
						fr = anthropicFinishToOpenAI(e.Delta.StopReason)
					}
					var usage map[string]any
					if e.Usage != nil {
						usage = map[string]any{
							"prompt_tokens": inTok, "completion_tokens": outTok,
							"total_tokens": inTok + outTok,
						}
					}
					emit(map[string]any{}, fr, usage)
					finishSent = true
				}
			}
		}
		if ev.name == "message_stop" || ev.data == "[DONE]" {
			break
		}
		if err != nil {
			break
		}
	}

	if sentRole && !finishSent {
		emit(map[string]any{}, "stop", nil)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	return nil
}

// ---------- 错误 ----------

// TranslateError 把厂商错误体翻译成客户端线格式，尽量保留原文 message。
func TranslateError(to string, status int, body []byte) []byte {
	msg := errorMessage(body)
	if msg == "" {
		msg = fmt.Sprintf("upstream returned http %d", status)
	}
	typ := errorType(status)
	if to == ProtocolAnthropic {
		out, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": typ, "message": msg},
		})
		return out
	}
	out, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
	return out
}

// errorMessage 从 OpenAI / Anthropic 两种错误体里取 message。
func errorMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err == nil {
		if v.Error.Message != "" {
			return v.Error.Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

func errorType(status int) string {
	switch status {
	case 400, 404, 409, 422:
		return "invalid_request_error"
	case 401, 403:
		return "authentication_error"
	case 429:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// ---------- 小工具 ----------

func rawJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func pickModel(echo, fallback string) string {
	if strings.TrimSpace(echo) != "" {
		return echo
	}
	return fallback
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "" || blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

func anthropicImageURI(blk anthBlock) string {
	if blk.Source == nil {
		return ""
	}
	if blk.Source.URL != "" {
		return blk.Source.URL
	}
	if blk.Source.Data != "" {
		mt := blk.Source.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + blk.Source.Data
	}
	return ""
}

func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "" || blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func openAIContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func openAIStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	return many
}

func anthropicToolChoiceToOpenAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var in struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	switch in.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "tool":
		if in.Name == "" {
			return nil
		}
		out, _ := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": in.Name},
		})
		return out
	}
	return nil
}

func openAIToolChoiceToAnthropic(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		}
		return nil
	}
	var in struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	if in.Function.Name == "" {
		return nil
	}
	out, _ := json.Marshal(map[string]any{"type": "tool", "name": in.Function.Name})
	return out
}

func anthropicFinishToOpenAI(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	case "end_turn", "stop_sequence", "":
		return "stop"
	default:
		return "stop"
	}
}

func openAIFinishToAnthropic(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "", "stop":
		return "end_turn"
	default:
		return "end_turn"
	}
}
