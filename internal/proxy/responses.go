package proxy

import (
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

// This file is the inbound half of the Responses adapter: Codex CLI and other
// OpenAI Responses clients send Responses payloads, janus converts them to chat
// internally, and the outbound adapters decide what the upstream speaks.

type responsesInput struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      json.RawMessage   `json:"instructions"`
	Tools             []responsesToolIn `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	MaxOutputTokens   int               `json:"max_output_tokens"`
	Temperature       *float64          `json:"temperature"`
	Stream            bool              `json:"stream"`
	Store             *bool             `json:"store"`
	PreviousResponse  string            `json:"previous_response_id"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Metadata          map[string]any    `json:"metadata"`
}

type responsesToolIn struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
}

// responsesText lifts instructions into a system prompt. The field is a string
// for most clients and an array of input_text parts for some.
func responsesText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, part := range parts {
			if strings.TrimSpace(part.Text) != "" {
				out = append(out, part.Text)
			}
		}
		return strings.TrimSpace(strings.Join(out, "\n\n"))
	}
	return ""
}

// responsesTextOf reads the text out of a content array, or a bare string.
func responsesTextOf(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text", "":
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

func responsesImages(raw json.RawMessage) []string {
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var urls []string
	for _, part := range parts {
		if part.Type == "input_image" && strings.TrimSpace(part.ImageURL) != "" {
			urls = append(urls, part.ImageURL)
		}
	}
	return urls
}

// responsesToChat converts one Responses request into an OpenAI chat body. The
// upstream name is written into the body, so outbound adapters see a normal
// chat request.
func responsesToChat(req responsesInput, upstream string) ([]byte, error) {
	messages := make([]map[string]any, 0, 4)
	if system := responsesText(req.Instructions); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	trimmed := strings.TrimSpace(string(req.Input))
	switch {
	case trimmed == "" || trimmed == "null":
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(req.Input, &text); err != nil {
			return nil, fmt.Errorf("decode input: %w", err)
		}
		messages = append(messages, map[string]any{"role": "user", "content": text})
	default:
		var items []responsesItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("decode input items: %w", err)
		}
		messages = append(messages, responsesItemsToMessages(items)...)
	}

	payload := map[string]any{
		"model":    upstream,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		payload["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if tool.Type != "function" || tool.Name == "" {
				continue
			}
			schema := tool.Parameters
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
		if len(tools) > 0 {
			payload["tools"] = tools
		}
	}
	if choice := responsesToolChoice(req.ToolChoice); choice != nil {
		payload["tool_choice"] = choice
	}
	return json.Marshal(payload)
}

func responsesToolChoice(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil
		}
		switch value {
		case "auto", "none", "required":
			return value
		}
		return nil
	}
	var choice responsesToolIn
	if err := json.Unmarshal(raw, &choice); err != nil || choice.Name == "" {
		return nil
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": choice.Name},
	}
}

// responsesItemsToMessages walks typed input items. Calls and their outputs
// become assistant tool_calls and role:tool messages, which is what the chat
// adapters expect regardless of the upstream protocol.
func responsesItemsToMessages(items []responsesItem) []map[string]any {
	var out []map[string]any
	for _, item := range items {
		switch item.Type {
		case "function_call":
			arguments := strings.TrimSpace(item.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			call := map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": arguments,
				},
			}
			// Attach to the previous assistant turn when it has no text, which
			// keeps parallel calls in one message as the chat API expects.
			if len(out) > 0 && out[len(out)-1]["role"] == "assistant" && out[len(out)-1]["content"] == nil {
				existing, _ := out[len(out)-1]["tool_calls"].([]map[string]any)
				out[len(out)-1]["tool_calls"] = append(existing, call)
				continue
			}
			out = append(out, map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": []map[string]any{call},
			})
		case "function_call_output":
			output := responsesTextOf(item.Output)
			if output == "" {
				output = strings.TrimSpace(string(item.Output))
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      strings.Trim(output, `"`),
			})
		default:
			role := item.Role
			if role == "" || role == "developer" {
				role = "user"
			}
			text := responsesTextOf(item.Content)
			images := responsesImages(item.Content)
			if len(images) == 0 {
				if text == "" {
					continue
				}
				out = append(out, map[string]any{"role": role, "content": text})
				continue
			}
			parts := make([]map[string]any, 0, len(images)+1)
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, url := range images {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
			out = append(out, map[string]any{"role": role, "content": parts})
		}
	}
	return out
}

// chatToResponse renders an OpenAI completion as a Responses object.
func chatToResponse(raw []byte, model string) ([]byte, error) {
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

	modelName := parsed.Model
	if modelName == "" {
		modelName = model
	}
	output := make([]map[string]any, 0, 2)
	if choice.Message.Content != "" || len(choice.Message.ToolCalls) == 0 {
		output = append(output, responsesMessageItem(choice.Message.Content))
	}
	for _, call := range choice.Message.ToolCalls {
		output = append(output, responsesFunctionCallItem(call.ID, call.Function.Name, call.Function.Arguments))
	}

	payload := map[string]any{
		"id":         "resp_" + randomHex(12),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     responsesStatus(choice.FinishReason),
		"model":      modelName,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  parsed.Usage.PromptTokens,
			"output_tokens": parsed.Usage.CompletionTokens,
			"total_tokens":  parsed.Usage.PromptTokens + parsed.Usage.CompletionTokens,
		},
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	return json.Marshal(payload)
}

func responsesStatus(finish string) string {
	switch finish {
	case "length":
		return "incomplete"
	case "content_filter":
		return "incomplete"
	}
	return "completed"
}

func responsesMessageItem(text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     "msg_" + randomHex(10),
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

func responsesFunctionCallItem(callID, name, arguments string) map[string]any {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"id":        "fc_" + randomHex(10),
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
		"status":    "completed",
	}
}

// responsesStream turns OpenAI chat chunks into Responses stream events.
type responsesStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string

	started    bool
	finished   bool
	itemIndex  int
	openItem   string // "" | "message" | "function_call"
	messageID  string
	toolID     string
	toolCallID string
	toolName   string
	status     string
	textBuf    strings.Builder
	args       strings.Builder
	input      int
	output     int
}

func newResponsesStream(w http.ResponseWriter, model string) *responsesStream {
	flusher, _ := w.(http.Flusher)
	return &responsesStream{
		w:       w,
		flusher: flusher,
		id:      "resp_" + randomHex(12),
		model:   model,
	}
}

func (s *responsesStream) write(event string, payload any) error {
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

func (s *responsesStream) envelope(status string) map[string]any {
	return map[string]any{
		"id":         s.id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      s.model,
		"output":     []any{},
		"usage":      nil,
	}
}

func (s *responsesStream) begin() error {
	if s.started {
		return nil
	}
	return s.write("response.created", map[string]any{
		"type":     "response.created",
		"response": s.envelope("in_progress"),
	})
}

func (s *responsesStream) closeItem() error {
	switch s.openItem {
	case "message":
		item := responsesMessageItem(s.textBuf.String())
		item["id"] = s.messageID
		if err := s.write("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": s.itemIndex,
			"item":         item,
		}); err != nil {
			return err
		}
		s.itemIndex++
		s.openItem = ""
	case "function_call":
		item := responsesFunctionCallItem(s.toolCallID, s.toolName, s.args.String())
		item["id"] = s.toolID
		if err := s.write("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": s.itemIndex,
			"item":         item,
		}); err != nil {
			return err
		}
		s.itemIndex++
		s.openItem = ""
	}
	return nil
}

func (s *responsesStream) text(delta string) error {
	if delta == "" {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	if s.openItem != "message" {
		if err := s.closeItem(); err != nil {
			return err
		}
		s.messageID = "msg_" + randomHex(10)
		s.openItem = "message"
		s.textBuf.Reset()
		item := responsesMessageItem("")
		item["id"] = s.messageID
		if err := s.write("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": s.itemIndex,
			"item":         item,
		}); err != nil {
			return err
		}
	}
	s.textBuf.WriteString(delta)
	return s.write("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       s.messageID,
		"output_index":  s.itemIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

func (s *responsesStream) toolStart(id, name string) error {
	if err := s.begin(); err != nil {
		return err
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	s.toolID = "fc_" + randomHex(10)
	s.toolCallID = id
	s.toolName = name
	s.openItem = "function_call"
	s.args.Reset()
	return s.write("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": s.itemIndex,
		"item": map[string]any{
			"type":      "function_call",
			"id":        s.toolID,
			"call_id":   id,
			"name":      name,
			"arguments": "",
			"status":    "in_progress",
		},
	})
}

func (s *responsesStream) toolArgs(delta string) error {
	if delta == "" || s.openItem != "function_call" {
		return nil
	}
	s.args.WriteString(delta)
	return s.write("response.function_call_arguments.delta", map[string]any{
		"type":         "response.function_call_arguments.delta",
		"item_id":      s.toolID,
		"output_index": s.itemIndex,
		"delta":        delta,
	})
}

func (s *responsesStream) setUsage(input, output int) {
	if input > 0 {
		s.input = input
	}
	if output > 0 {
		s.output = output
	}
}

func (s *responsesStream) finish(status string) error {
	if s.finished {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	s.finished = true
	if status == "" {
		status = s.status
	}
	if status == "" {
		status = "completed"
	}
	envelope := s.envelope(status)
	envelope["usage"] = map[string]any{
		"input_tokens":  s.input,
		"output_tokens": s.output,
		"total_tokens":  s.input + s.output,
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	return s.write(event, map[string]any{
		"type":     event,
		"response": envelope,
	})
}

func (s *responsesStream) handle(chunk gateway.ChatChunk) error {
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.setUsage(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
	}
	if err := s.text(chunk.Content); err != nil {
		return err
	}
	for _, call := range chunk.ToolCalls {
		if call.Name != "" {
			if err := s.toolStart(call.ID, call.Name); err != nil {
				return err
			}
		}
		if err := s.toolArgs(call.Arguments); err != nil {
			return err
		}
	}
	if chunk.FinishReason != "" {
		// Defer the terminal event: the usage counters usually arrive in the
		// next chunk, and a completed event with zero tokens is misleading.
		s.status = responsesStatus(chunk.FinishReason)
	}
	return nil
}

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authorize(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody()))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req responsesInput
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	client, ok := s.table()[model]
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", "unknown model "+model)
		return
	}

	chatBody, err := responsesToChat(req, client.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if affinity := responsesAffinity(r, req); affinity != "" {
		r = r.WithContext(gateway.WithAffinity(r.Context(), affinity))
	}

	if req.Stream {
		s.streamResponses(w, r, identity, client, model, chatBody)
		return
	}
	s.forwardResponses(w, r, identity, client, model, chatBody)
}

func responsesAffinity(r *http.Request, req responsesInput) string {
	if value, ok := req.Metadata["user_id"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if value := strings.TrimSpace(req.PreviousResponse); value != "" {
		return value
	}
	return sessionAffinity(r, nil)
}

func (s *Server) forwardResponses(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, exposed string, body []byte) {
	started := time.Now()
	raw, used, err := client.Chat(r.Context(), client.Model, body)
	if err != nil {
		status, code := failureOf(err)
		s.record(r, id, gateway.Usage{}, exposed, client.Model, false, status, code, started)
		s.upstreamError(w, err)
		return
	}
	converted, err := chatToResponse(raw, exposed)
	if err != nil {
		s.record(r, id, used, exposed, client.Model, false, http.StatusBadGateway, "upstream_error", started)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	s.record(r, id, used, exposed, client.Model, false, http.StatusOK, "", started)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, exposed string, body []byte) {
	stream := newResponsesStream(w, exposed)

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
			s.upstreamError(w, err)
			return
		}
		s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "stream_interrupted", started)
		s.logf("responses stream for %s failed mid-flight: %v", client.Model, err)
		return
	}
	if consumeErr != nil {
		s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "stream_interrupted", started)
		s.logf("responses stream consumer failed for %s: %v", client.Model, consumeErr)
		return
	}
	if err := stream.finish(""); err != nil {
		s.logf("responses stream finish for %s: %v", client.Model, err)
	}
	s.record(r, id, used, exposed, client.Model, true, http.StatusOK, "", started)
}
