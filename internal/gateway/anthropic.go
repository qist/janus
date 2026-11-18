package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// anthropicDefaultMaxTokens is used when the caller sent no limit. Messages
// requires max_tokens; guessing the model maximum would be worse than a modest
// default, because Anthropic bills per generated token.
const anthropicDefaultMaxTokens = 8192

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicRequest struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	System      string               `json:"system,omitempty"`
	Messages    []anthropicMessage   `json:"messages"`
	Tools       []anthropicTool      `json:"tools,omitempty"`
	ToolChoice  *anthropicToolChoice `json:"tool_choice,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
	Stream      bool                 `json:"stream,omitempty"`
}

// buildAnthropicRequest converts an OpenAI chat body into a Messages body.
// System prompts move to the top-level field, tool calls and tool results
// become content blocks, and consecutive same-role turns are merged because
// Anthropic expects an alternating conversation.
func buildAnthropicRequest(in chatRequestIn, stream bool) (anthropicRequest, error) {
	maxTokens := in.maxOutput()
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	out := anthropicRequest{
		Model:       in.Model,
		MaxTokens:   maxTokens,
		System:      systemText(in.Messages),
		Temperature: in.Temperature,
		Stream:      stream,
	}

	for _, message := range in.Messages {
		switch message.Role {
		case "system", "developer":
			continue
		case "tool":
			blocks := []anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: message.ToolCallID,
				Content:   contentText(message.Content),
			}}
			appendAnthropicMessage(&out.Messages, "user", blocks)
		case "assistant":
			var blocks []anthropicBlock
			for _, part := range parseContent(message.Content) {
				if part.Text != "" {
					blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
				}
			}
			for _, call := range message.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    call.ID,
					Name:  call.Function.Name,
					Input: decodeArguments(call.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			appendAnthropicMessage(&out.Messages, "assistant", blocks)
		default:
			var blocks []anthropicBlock
			for _, part := range parseContent(message.Content) {
				if part.IsImage {
					if part.Data == "" {
						continue
					}
					blocks = append(blocks, anthropicBlock{
						Type:   "image",
						Source: &anthropicImageSource{Type: "base64", MediaType: part.MediaType, Data: part.Data},
					})
					continue
				}
				if part.Text != "" {
					blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
				}
			}
			if len(blocks) == 0 {
				continue
			}
			appendAnthropicMessage(&out.Messages, "user", blocks)
		}
	}

	// Anthropic rejects a conversation that starts with an assistant turn.
	if len(out.Messages) == 0 {
		out.Messages = []anthropicMessage{{Role: "user", Content: []anthropicBlock{{Type: "text", Text: "Continue."}}}}
	} else if out.Messages[0].Role != "user" {
		out.Messages = append([]anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "text", Text: "Continue."}},
		}}, out.Messages...)
	}

	for _, tool := range in.Tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice = anthropicChoice(in.ToolChoice)
	return out, nil
}

func appendAnthropicMessage(messages *[]anthropicMessage, role string, blocks []anthropicBlock) {
	if len(blocks) == 0 {
		return
	}
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == role {
		last := &(*messages)[len(*messages)-1]
		last.Content = append(last.Content, blocks...)
		return
	}
	*messages = append(*messages, anthropicMessage{Role: role, Content: blocks})
}

func anthropicChoice(raw json.RawMessage) *anthropicToolChoice {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null":
		return nil
	case `"auto"`:
		return &anthropicToolChoice{Type: "auto"}
	case `"none"`:
		return &anthropicToolChoice{Type: "none"}
	case `"required"`:
		return &anthropicToolChoice{Type: "any"}
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err == nil && named.Function.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: named.Function.Name}
	}
	return nil
}

// contentText flattens a tool result body. OpenAI sends a string; some clients
// send parts, which are joined.
func contentText(raw json.RawMessage) string {
	parts := parseContent(raw)
	if len(parts) == 0 {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type anthropicResponse struct {
	ID         string           `json:"id"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Content    []anthropicBlock `json:"content"`
	Usage      anthropicUsage   `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// chatAnthropic sends one non-streaming Messages request and returns an OpenAI
// chat completion.
func (c *Client) chatAnthropic(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	payload, err := buildAnthropicRequest(in, false)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: encode messages request: %w", c.Name, err)
	}

	resp, credentialID, err := c.send(ctx, model, apiEndpoint(c.BaseURL, "messages", "v1"), raw, false)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: read response: %w", c.Name, err)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: decode messages response: %w", c.Name, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, Usage{}, fmt.Errorf("gateway %s: %s: %s", c.Name, parsed.Error.Type, parsed.Error.Message)
	}

	used := Usage{
		ProviderID:       c.Name,
		CredentialID:     credentialID,
		PromptTokens:     parsed.Usage.InputTokens,
		CompletionTokens: parsed.Usage.OutputTokens,
		CachedTokens:     parsed.Usage.CacheReadInputTokens,
	}
	if parsed.Model != "" {
		used.Model = parsed.Model
	} else {
		used.Model = model
	}
	return anthropicToChat(parsed, model), used, nil
}

// anthropicToChat renders the Messages response as a chat completion, including
// tool_use blocks as tool_calls so an agent loop keeps working.
func anthropicToChat(parsed anthropicResponse, model string) []byte {
	var texts []string
	var calls []any
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			calls = append(calls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": string(args),
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if message["content"] == "" {
			message["content"] = nil
		}
	}
	finish := "stop"
	switch parsed.StopReason {
	case "max_tokens":
		finish = "length"
	case "tool_use":
		finish = "tool_calls"
	case "refusal":
		finish = "content_filter"
	}
	modelName := parsed.Model
	if modelName == "" {
		modelName = model
	}
	completion := map[string]any{
		"id":      "chatcmpl_" + strings.TrimPrefix(defaultID(parsed.ID), "msg_"),
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     parsed.Usage.InputTokens,
			"completion_tokens": parsed.Usage.OutputTokens,
			"total_tokens":      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		},
	}
	raw, err := json.Marshal(completion)
	if err != nil {
		return []byte(`{"error":{"message":"encode completion","type":"janus_error"}}`)
	}
	return raw
}

// streamAnthropic translates Messages SSE into OpenAI chat chunks.
func (c *Client) streamAnthropic(ctx context.Context, model string, body []byte, dst io.Writer) (Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	payload, err := buildAnthropicRequest(in, true)
	if err != nil {
		return Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Usage{}, fmt.Errorf("gateway %s: encode messages request: %w", c.Name, err)
	}

	resp, credentialID, err := c.send(ctx, model, apiEndpoint(c.BaseURL, "messages", "v1"), raw, true)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()

	sink := newChunkSink(dst, "", model)
	used := Usage{ProviderID: c.Name, CredentialID: credentialID, Model: model}
	blocks := map[int]string{} // index → "text" | "tool_use"
	tools := map[int]string{}  // index → tool id
	toolNames := map[int]string{}
	finished := false

	reader := newSSEReader(resp.Body)
	for {
		event, err := reader.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return used, fmt.Errorf("gateway %s: read stream: %w", c.Name, err)
		}
		if len(event.Data) == 0 {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
				ID    string         `json:"id"`
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
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
			Usage *anthropicUsage `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &frame); err != nil {
			continue
		}
		if frame.Type == "" {
			frame.Type = event.Name
		}

		switch frame.Type {
		case "message_start":
			if frame.Message != nil && frame.Message.Usage.InputTokens > 0 {
				used.PromptTokens = frame.Message.Usage.InputTokens
				used.CachedTokens = frame.Message.Usage.CacheReadInputTokens
			}
		case "content_block_start":
			if frame.ContentBlock == nil {
				continue
			}
			switch frame.ContentBlock.Type {
			case "text":
				blocks[frame.Index] = "text"
			case "tool_use":
				blocks[frame.Index] = "tool_use"
				tools[frame.Index] = frame.ContentBlock.ID
				toolNames[frame.Index] = frame.ContentBlock.Name
				if err := sink.ToolStart(frame.ContentBlock.ID, frame.ContentBlock.Name); err != nil {
					return used, err
				}
			}
		case "content_block_delta":
			if frame.Delta == nil {
				continue
			}
			switch frame.Delta.Type {
			case "text_delta":
				if err := sink.Text(frame.Delta.Text); err != nil {
					return used, err
				}
			case "input_json_delta":
				if err := sink.ToolArgs(frame.Delta.PartialJSON); err != nil {
					return used, err
				}
			}
		case "message_delta":
			if frame.Usage != nil && frame.Usage.OutputTokens > 0 {
				used.CompletionTokens = frame.Usage.OutputTokens
			}
			reason := ""
			if frame.Delta != nil {
				switch frame.Delta.StopReason {
				case "max_tokens":
					reason = "length"
				case "tool_use":
					reason = "tool_calls"
				case "refusal":
					reason = "content_filter"
				case "end_turn", "stop_sequence":
					reason = "stop"
				}
			}
			if err := sink.Finish(reason); err != nil {
				return used, err
			}
			finished = true
		case "message_stop":
			if !finished {
				if err := sink.Finish(""); err != nil {
					return used, err
				}
				finished = true
			}
		case "error":
			if frame.Error != nil {
				return used, fmt.Errorf("gateway %s: %s: %s", c.Name, frame.Error.Type, frame.Error.Message)
			}
		}
	}

	if !finished {
		if err := sink.Finish(""); err != nil {
			return used, err
		}
	}
	sink.SetUsage(used.PromptTokens, used.CompletionTokens, used.CachedTokens, used.Model)
	if err := sink.Close(); err != nil {
		return used, err
	}
	return used, nil
}

func defaultID(value string) string {
	if strings.TrimSpace(value) == "" {
		return randomToken(12)
	}
	return value
}
