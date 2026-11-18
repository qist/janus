package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"janus/internal/auth"
	"janus/internal/gateway"
)

// This file is the inbound half of the Messages adapter: Claude Code and other
// Anthropic clients send Messages, janus converts to chat internally, and the
// outbound adapters decide what the upstream actually speaks.

type messagesRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system"`
	Messages    []messagesTurn  `json:"messages"`
	Tools       []messagesTool  `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Temperature *float64        `json:"temperature"`
	Stream      bool            `json:"stream"`
	Metadata    map[string]any  `json:"metadata"`
}

type messagesTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// parseMessagesBlocks accepts either a bare string or an array of blocks, which
// is what clients send for the common "just text" case.
func parseMessagesBlocks(raw json.RawMessage) []messagesBlock {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil && text != "" {
			return []messagesBlock{{Type: "text", Text: text}}
		}
		return nil
	}
	var blocks []messagesBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

func messagesSystemText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var parts []string
	for _, block := range parseMessagesBlocks(raw) {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// messagesToChat converts one Messages request into an OpenAI chat body. The
// upstream name is written into the body, so the outbound adapter sees a normal
// chat request.
func messagesToChat(req messagesRequest, upstream string) ([]byte, error) {
	messages := make([]map[string]any, 0, len(req.Messages)+1)
	if system := messagesSystemText(req.System); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	for _, turn := range req.Messages {
		blocks := parseMessagesBlocks(turn.Content)
		if turn.Role == "assistant" {
			var text strings.Builder
			var calls []map[string]any
			for _, block := range blocks {
				switch block.Type {
				case "text":
					text.WriteString(block.Text)
				case "tool_use":
					arguments := strings.TrimSpace(string(block.Input))
					if arguments == "" {
						arguments = "{}"
					}
					calls = append(calls, map[string]any{
						"id":   block.ID,
						"type": "function",
						"function": map[string]any{
							"name":      block.Name,
							"arguments": arguments,
						},
					})
				}
			}
			message := map[string]any{"role": "assistant"}
			if text.Len() > 0 {
				message["content"] = text.String()
			} else {
				message["content"] = nil
			}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			messages = append(messages, message)
			continue
		}

		// A user turn may carry text, images and tool results. Tool results
		// become standalone tool messages, which OpenAI expects directly after
		// the assistant turn that requested them.
		var text strings.Builder
		var parts []map[string]any
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": block.ToolUseID,
					"content":      messagesToolResultText(block.Content),
				})
			case "image":
				if block.Source == nil {
					continue
				}
				url := block.Source.URL
				if block.Source.Type == "base64" && block.Source.Data != "" {
					media := block.Source.MediaType
					if media == "" {
						media = "image/png"
					}
					url = "data:" + media + ";base64," + block.Source.Data
				}
				if url == "" {
					continue
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			case "text":
				text.WriteString(block.Text)
			}
		}
		if text.Len() > 0 {
			parts = append([]map[string]any{{"type": "text", "text": text.String()}}, parts...)
		}
		switch {
		case len(parts) == 0:
			continue
		case len(parts) == 1 && parts[0]["type"] == "text":
			messages = append(messages, map[string]any{"role": "user", "content": parts[0]["text"]})
		default:
			messages = append(messages, map[string]any{"role": "user", "content": parts})
		}
	}

	payload := map[string]any{
		"model":    upstream,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  schema,
				},
			})
		}
		payload["tools"] = tools
	}
	if choice := messagesToolChoice(req.ToolChoice); choice != nil {
		payload["tool_choice"] = choice
	}
	return json.Marshal(payload)
}

func messagesToolChoice(raw json.RawMessage) any {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if choice.Name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choice.Name},
			}
		}
	}
	return nil
}

func messagesToolResultText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []string
	for _, block := range parseMessagesBlocks(raw) {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// chatToMessages renders an OpenAI completion as a Messages response.
func chatToMessages(raw []byte, model string) ([]byte, error) {
	var parsed struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode chat completion: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("upstream returned no choices")
	}
	choice := parsed.Choices[0]

	content := make([]map[string]any, 0, 2)
	if choice.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": decodeToolInput(call.Function.Arguments),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	modelName := parsed.Model
	if modelName == "" {
		modelName = model
	}
	// Upstream ids come as both chatcmpl- and chatcmpl_; the Messages client only
	// needs them stable, so the prefix is normalised to msg_.
	id := strings.TrimPrefix(parsed.ID, "chatcmpl-")
	id = strings.TrimPrefix(id, "chatcmpl_")
	if id == "" {
		id = randomHex(16)
	}
	payload := map[string]any{
		"id":            "msg_" + id,
		"type":          "message",
		"role":          "assistant",
		"model":         modelName,
		"content":       content,
		"stop_reason":   messagesStopReason(choice.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  parsed.Usage.PromptTokens,
			"output_tokens": parsed.Usage.CompletionTokens,
		},
	}
	return json.Marshal(payload)
}

func decodeToolInput(arguments string) any {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(arguments), &out); err == nil {
		return out
	}
	return map[string]any{"_raw": arguments}
}

func messagesStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// messagesStream turns OpenAI chat chunks into Anthropic stream events.
type messagesStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string

	started    bool
	finished   bool
	blockIndex int
	blockType  string
	input      int
	output     int
}

func newMessagesStream(w http.ResponseWriter, model string) *messagesStream {
	flusher, _ := w.(http.Flusher)
	return &messagesStream{
		w:       w,
		flusher: flusher,
		id:      "msg_" + randomHex(12),
		model:   model,
	}
}

func (m *messagesStream) write(event string, payload any) error {
	if !m.started {
		m.w.Header().Set("Content-Type", "text/event-stream")
		m.w.Header().Set("Cache-Control", "no-cache")
		m.w.Header().Set("Connection", "keep-alive")
		m.w.Header().Set("X-Accel-Buffering", "no")
		m.w.WriteHeader(http.StatusOK)
		m.started = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(m.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if m.flusher != nil {
		m.flusher.Flush()
	}
	return nil
}

func (m *messagesStream) begin() error {
	if m.started {
		return nil
	}
	return m.write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            m.id,
			"type":          "message",
			"role":          "assistant",
			"model":         m.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (m *messagesStream) closeBlock() error {
	if m.blockType == "" {
		return nil
	}
	err := m.write("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": m.blockIndex,
	})
	m.blockIndex++
	m.blockType = ""
	return err
}

func (m *messagesStream) text(delta string) error {
	if delta == "" {
		return nil
	}
	if err := m.begin(); err != nil {
		return err
	}
	if m.blockType != "text" {
		if err := m.closeBlock(); err != nil {
			return err
		}
		if err := m.write("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         m.blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
		m.blockType = "text"
	}
	return m.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": m.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

func (m *messagesStream) toolStart(id, name string) error {
	if err := m.begin(); err != nil {
		return err
	}
	if err := m.closeBlock(); err != nil {
		return err
	}
	if err := m.write("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": m.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	m.blockType = "tool_use"
	return nil
}

func (m *messagesStream) toolArgs(delta string) error {
	if delta == "" || m.blockType != "tool_use" {
		return nil
	}
	return m.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": m.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
	})
}

func (m *messagesStream) setUsage(input, output int) {
	if input > 0 {
		m.input = input
	}
	if output > 0 {
		m.output = output
	}
}

func (m *messagesStream) finish(reason string) error {
	if m.finished {
		return nil
	}
	if err := m.begin(); err != nil {
		return err
	}
	if err := m.closeBlock(); err != nil {
		return err
	}
	m.finished = true
	if err := m.write("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": messagesStopReason(reason), "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  m.input,
			"output_tokens": m.output,
		},
	}); err != nil {
		return err
	}
	return m.write("message_stop", map[string]any{"type": "message_stop"})
}

func (m *messagesStream) handle(chunk gateway.ChatChunk) error {
	if chunk.Model != "" {
		m.model = chunk.Model
	}
	if chunk.Usage != nil {
		m.setUsage(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
	}
	if err := m.text(chunk.Content); err != nil {
		return err
	}
	for _, call := range chunk.ToolCalls {
		if call.Name != "" {
			if err := m.toolStart(call.ID, call.Name); err != nil {
				return err
			}
		}
		if err := m.toolArgs(call.Arguments); err != nil {
			return err
		}
	}
	if chunk.FinishReason != "" {
		return m.finish(chunk.FinishReason)
	}
	return nil
}

// authorizeMessages uses the Anthropic error envelope, which is what these
// clients parse; the OpenAI envelope is only valid on the chat endpoints.
func (s *Server) authorizeMessages(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	identity, status, code, message := s.checkChatKey(r)
	if code == "" {
		return identity, true
	}
	kind := "authentication_error"
	if status == http.StatusForbidden {
		kind = "permission_error"
	}
	writeMessagesError(w, status, kind, message)
	return auth.Identity{}, false
}

// handleMessages serves POST /v1/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authorizeMessages(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody()))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	client, ok := s.table()[model]
	if !ok {
		writeMessagesError(w, http.StatusNotFound, "not_found_error", "unknown model "+model)
		return
	}

	chatBody, err := messagesToChat(req, client.Model)
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	affinity := messagesAffinity(r, req)
	if affinity != "" {
		r = r.WithContext(gateway.WithAffinity(r.Context(), affinity))
	}

	if req.Stream {
		s.streamMessages(w, r, identity, client, model, chatBody)
		return
	}
	s.forwardMessages(w, r, identity, client, model, chatBody)
}

func messagesAffinity(r *http.Request, req messagesRequest) string {
	if value, ok := req.Metadata["user_id"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return sessionAffinity(r, nil)
}

func (s *Server) forwardMessages(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, exposed string, body []byte) {
	started := time.Now()
	raw, used, err := client.Chat(r.Context(), client.Model, body)
	if err != nil {
		status, code := failureOf(err)
		s.record(r, id, gateway.Usage{}, exposed, client.Model, false, status, code, started)
		writeMessagesError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	converted, err := chatToMessages(raw, exposed)
	if err != nil {
		s.record(r, id, used, exposed, client.Model, false, http.StatusBadGateway, "upstream_error", started)
		writeMessagesError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	s.record(r, id, used, exposed, client.Model, false, http.StatusOK, "", started)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, exposed string, body []byte) {
	stream := newMessagesStream(w, exposed)

	reader, writer := io.Pipe()
	done := make(chan struct{})
	var consumeErr error
	go func() {
		defer close(done)
		consumeErr = gateway.StreamChatChunks(reader, stream.handle)
		_ = reader.CloseWithError(consumeErr)
	}()

	started := time.Now()
	used, err := client.ChatStream(r.Context(), client.Model, body, writer)
	_ = writer.Close()
	<-done

	if err != nil {
		if !stream.started {
			status, code := failureOf(err)
			s.record(r, id, used, exposed, client.Model, true, status, code, started)
			writeMessagesError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "stream_interrupted", started)
		s.logf("messages stream for %s failed mid-flight: %v", client.Model, err)
		return
	}
	if consumeErr != nil {
		s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "stream_interrupted", started)
		s.logf("messages stream consumer failed for %s: %v", client.Model, consumeErr)
		return
	}
	if err := stream.finish(""); err != nil {
		s.logf("messages stream finish for %s: %v", client.Model, err)
	}
	s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "", started)
}

// handleCountTokens answers the preflight call Claude Code makes. It is a rough
// estimate on purpose: an exact count needs the upstream tokenizer, and the
// value is only used for client-side context budgeting.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMessages(w, r); !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody()))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	characters := len(messagesSystemText(req.System))
	for _, turn := range req.Messages {
		for _, block := range parseMessagesBlocks(turn.Content) {
			characters += len(block.Text)
			if len(block.Input) > 0 {
				characters += len(block.Input)
			}
			if len(block.Content) > 0 {
				characters += len(block.Content)
			}
		}
	}
	if len(req.Tools) > 0 {
		raw, _ := json.Marshal(req.Tools)
		characters += len(raw)
	}
	tokens := characters / 4
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

func writeMessagesError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": message,
		},
	})
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
