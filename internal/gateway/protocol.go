package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Protocol is the wire format an upstream speaks. janus always accepts OpenAI
// chat completions from callers and translates here, so a provider's protocol is
// an upstream detail, not something every client has to implement.
type Protocol string

const (
	// ProtocolChat is the OpenAI chat completions format, the default.
	ProtocolChat Protocol = "chat"
	// ProtocolResponses is the OpenAI Responses API used by the Codex backend.
	ProtocolResponses Protocol = "responses"
	// ProtocolAnthropic is the Messages API used by Claude Code.
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolGemini is the Code Assist generateContent envelope.
	ProtocolGemini Protocol = "gemini"
)

// ParseProtocol validates a configured protocol name. Empty means chat, because
// every provider created before this field existed speaks chat.
func ParseProtocol(value string) (Protocol, bool) {
	switch Protocol(strings.ToLower(strings.TrimSpace(value))) {
	case "", ProtocolChat:
		return ProtocolChat, true
	case ProtocolResponses:
		return ProtocolResponses, true
	case ProtocolAnthropic:
		return ProtocolAnthropic, true
	case ProtocolGemini:
		return ProtocolGemini, true
	}
	return "", false
}

// Version is the API version segment placed before a resource.
const (
	anthropicVersion = "2023-06-01"
	oauthBeta        = "oauth-2025-04-20"
)

// apiEndpoint joins a configured base URL and a resource. Operators enter both
// "https://api.openai.com" and "https://api.openai.com/v1", and vendor-specific
// planes such as "https://chatgpt.com/backend-api/codex" carry their own layout,
// so the version segment is added only when it is missing.
func apiEndpoint(base, resource, version string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case base == "":
		return "/" + version + "/" + resource
	case strings.HasSuffix(base, "/"+resource):
		return base
	case strings.HasSuffix(base, "/"+version):
		return base + "/" + resource
	case strings.Contains(base, "/backend-api/"):
		return base + "/" + resource
	default:
		return base + "/" + version + "/" + resource
	}
}

func setBearer(req *http.Request, credential Credential) {
	if credential.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	}
}

// applyAuth writes the headers one protocol needs. Anthropic API keys must not
// travel as a bearer token: the vendor rejects them, and OAuth tokens must not
// travel as x-api-key. Getting this backwards produces a 401 that looks like a
// dead account, so the distinction is explicit.
func (c *Client) applyAuth(req *http.Request, credential Credential) {
	switch c.Protocol {
	case ProtocolAnthropic:
		req.Header.Set("anthropic-version", anthropicVersion)
		if credential.OAuth {
			setBearer(req, credential)
			req.Header.Set("anthropic-beta", oauthBeta)
			return
		}
		if credential.APIKey != "" {
			req.Header.Set("x-api-key", credential.APIKey)
		}
	case ProtocolResponses:
		setBearer(req, credential)
		if credential.OAuth {
			// The Codex backend only accepts the Responses beta, expects an
			// originator, and uses the account id embedded in the access token.
			req.Header.Set("OpenAI-Beta", "responses=experimental")
			req.Header.Set("originator", "codex_cli_rs")
			if id := chatgptAccountID(credential.APIKey); id != "" {
				req.Header.Set("chatgpt-account-id", id)
			}
			req.Header.Set("session_id", sessionID(req.Context()))
		}
	default:
		setBearer(req, credential)
	}
}

// chatgptAccountID reads the account claim Codex puts in its access token. The
// token stays in memory; only the id travels upstream.
func chatgptAccountID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	auth, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(id)
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func sessionID(ctx context.Context) string {
	if id := Affinity(ctx); id != "" {
		return id
	}
	return "janus_" + randomToken(8)
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// sseEvent is one text/event-stream frame.
type sseEvent struct {
	Name string
	Data []byte
}

// sseReader reads frames without buffering the whole body, so a long stream is
// forwarded as it arrives.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 16<<10)}
}

func (s *sseReader) next() (sseEvent, error) {
	var event sseEvent
	var data []byte
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if len(data) > 0 {
				event.Data = data
				return event, nil
			}
			return sseEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(data) > 0 {
				event.Data = data
				return event, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
	}
}

// chatSink receives protocol-neutral stream events. Two implementations exist:
// one writes OpenAI chunks to the caller, the other aggregates a non-streaming
// completion because the upstream only streams.
type chatSink interface {
	Text(delta string) error
	ToolStart(id, name string) error
	ToolArgs(delta string) error
	SetUsage(prompt, completion, cached int, model string)
	Finish(reason string) error
}

// chunkSink translates upstream events into OpenAI chat.completion.chunk frames.
type chunkSink struct {
	dst     io.Writer
	id      string
	model   string
	created int64
	tools   int
	usage   Usage
}

func newChunkSink(dst io.Writer, id, model string) *chunkSink {
	if strings.TrimSpace(id) == "" {
		id = "chatcmpl_" + randomToken(12)
	}
	return &chunkSink{dst: dst, id: id, model: model, created: time.Now().Unix()}
}

func (s *chunkSink) emit(delta map[string]any, finish any) error {
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.dst, "data: %s\n\n", raw)
	return err
}

func (s *chunkSink) Text(delta string) error {
	if delta == "" {
		return nil
	}
	return s.emit(map[string]any{"content": delta}, nil)
}

func (s *chunkSink) ToolStart(id, name string) error {
	call := map[string]any{
		"index": s.tools,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": "",
		},
	}
	if id != "" {
		call["id"] = id
	}
	s.tools++
	return s.emit(map[string]any{"tool_calls": []any{call}}, nil)
}

func (s *chunkSink) ToolArgs(delta string) error {
	if delta == "" {
		return nil
	}
	index := s.tools - 1
	if index < 0 {
		index = 0
	}
	return s.emit(map[string]any{"tool_calls": []any{map[string]any{
		"index":    index,
		"function": map[string]any{"arguments": delta},
	}}}, nil)
}

func (s *chunkSink) SetUsage(prompt, completion, cached int, model string) {
	if model != "" {
		s.model = model
	}
	s.usage.PromptTokens = prompt
	s.usage.CompletionTokens = completion
	s.usage.CachedTokens = cached
}

func (s *chunkSink) Finish(reason string) error {
	if reason == "" {
		if s.tools > 0 {
			reason = "tool_calls"
		} else {
			reason = "stop"
		}
	}
	return s.emit(map[string]any{}, reason)
}

// Close writes the optional usage frame and the terminating marker. Clients
// that do not understand usage frames ignore a chunk with an empty choices list.
func (s *chunkSink) Close() error {
	if s.usage.PromptTokens > 0 || s.usage.CompletionTokens > 0 {
		chunk := map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     s.usage.PromptTokens,
				"completion_tokens": s.usage.CompletionTokens,
				"total_tokens":      s.usage.PromptTokens + s.usage.CompletionTokens,
			},
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(s.dst, "data: %s\n\n", raw); err != nil {
			return err
		}
	}
	_, err := io.WriteString(s.dst, "data: [DONE]\n\n")
	return err
}

// aggregateSink builds one chat completion from an upstream stream. It exists
// because the Codex backend refuses non-streaming requests.
type aggregateSink struct {
	id     string
	model  string
	text   strings.Builder
	tools  []aggregateTool
	usage  Usage
	finish string
}

type aggregateTool struct {
	ID        string
	Name      string
	Arguments string
}

func (s *aggregateSink) Text(delta string) error {
	s.text.WriteString(delta)
	return nil
}

func (s *aggregateSink) ToolStart(id, name string) error {
	s.tools = append(s.tools, aggregateTool{ID: id, Name: name})
	return nil
}

func (s *aggregateSink) ToolArgs(delta string) error {
	if len(s.tools) == 0 {
		// Arguments arrived before the item header; keep them rather than drop
		// the call, and let the header fill in later.
		s.tools = append(s.tools, aggregateTool{})
	}
	s.tools[len(s.tools)-1].Arguments += delta
	return nil
}

func (s *aggregateSink) SetUsage(prompt, completion, cached int, model string) {
	if model != "" {
		s.model = model
	}
	if prompt > 0 {
		s.usage.PromptTokens = prompt
	}
	if completion > 0 {
		s.usage.CompletionTokens = completion
	}
	if cached > 0 {
		s.usage.CachedTokens = cached
	}
}

func (s *aggregateSink) Finish(reason string) error {
	s.finish = reason
	return nil
}

// Result renders a chat completion in the format the caller asked for.
func (s *aggregateSink) Result(model string) []byte {
	if s.model == "" {
		s.model = model
	}
	if s.id == "" {
		s.id = "chatcmpl_" + randomToken(12)
	}
	message := map[string]any{"role": "assistant", "content": s.text.String()}
	if len(s.tools) > 0 {
		calls := make([]any, 0, len(s.tools))
		for _, tool := range s.tools {
			calls = append(calls, map[string]any{
				"id":   tool.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tool.Name,
					"arguments": tool.Arguments,
				},
			})
		}
		message["tool_calls"] = calls
		if message["content"] == "" {
			message["content"] = nil
		}
	}
	finish := s.finish
	if finish == "" {
		if len(s.tools) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     s.usage.PromptTokens,
			"completion_tokens": s.usage.CompletionTokens,
			"total_tokens":      s.usage.PromptTokens + s.usage.CompletionTokens,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"message":"encode completion","type":"janus_error"}}`)
	}
	return raw
}

// chatMessageIn and friends mirror the parts of the OpenAI chat format the
// adapters understand. Fields janus does not translate are dropped on purpose:
// silently forwarding a field the upstream ignores is worse than dropping it.
type chatMessageIn struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []toolCallIn    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type toolCallIn struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDefIn struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequestIn struct {
	Model               string          `json:"model"`
	Messages            []chatMessageIn `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Tools               []toolDefIn     `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

func (r chatRequestIn) maxOutput() int {
	switch {
	case r.MaxCompletionTokens > 0:
		return r.MaxCompletionTokens
	case r.MaxTokens > 0:
		return r.MaxTokens
	}
	return 0
}

// contentPart is one flattened content element. Only text and images are
// carried across protocols; audio and files are rejected by upstreams in
// different ways, so they are dropped rather than half-translated.
type contentPart struct {
	Text      string
	ImageURL  string
	MediaType string
	Data      string
	IsImage   bool
}

func parseContent(raw json.RawMessage) []contentPart {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil
		}
		return []contentPart{{Text: text}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]contentPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if part.Text != "" {
				out = append(out, contentPart{Text: part.Text})
			}
		case "image_url", "input_image":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				continue
			}
			url := part.ImageURL.URL
			item := contentPart{ImageURL: url, IsImage: true}
			if media, data, ok := decodeDataURL(url); ok {
				item.MediaType, item.Data = media, data
			}
			out = append(out, item)
		}
	}
	return out
}

// decodeDataURL splits "data:image/png;base64,AAA". Plain http URLs are left
// alone: the Anthropic adapter cannot fetch them without becoming a proxy.
func decodeDataURL(url string) (string, string, bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(url, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	if !strings.Contains(meta, ";base64") {
		return "", "", false
	}
	media := strings.TrimSuffix(meta, ";base64")
	if media == "" {
		media = "image/png"
	}
	return media, rest[comma+1:], true
}

// decodeArguments turns a tool-call argument string into the object Anthropic
// and Responses expect. Malformed JSON is kept under a key rather than dropped.
func decodeArguments(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out
	}
	return map[string]any{"_raw": raw}
}

func systemText(messages []chatMessageIn) string {
	var parts []string
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			for _, part := range parseContent(message.Content) {
				if strings.TrimSpace(part.Text) != "" {
					parts = append(parts, part.Text)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
