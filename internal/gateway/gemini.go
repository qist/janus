package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Gemini's Code Assist API wraps the usual generateContent payload in an
// envelope that names the model and the cloud project. The adapter keeps that
// detail here so callers still speak chat completions.

type geminiPart struct {
	Text         string `json:"text,omitempty"`
	FunctionCall *struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args,omitempty"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string         `json:"name"`
		Response map[string]any `json:"response,omitempty"`
	} `json:"functionResponse,omitempty"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiTool struct {
	FunctionDeclarations []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"functionDeclarations"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig struct {
		Mode string `json:"mode"`
	} `json:"functionCallingConfig"`
}

type geminiInnerRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiEnvelope struct {
	Model   string `json:"model"`
	Project string `json:"project,omitempty"`
	// RequestID, UserAgent and RequestType are Antigravity's envelope
	// metadata; an empty field is omitted so Code Assist requests stay clean.
	RequestID   string             `json:"requestId,omitempty"`
	UserAgent   string             `json:"userAgent,omitempty"`
	RequestType string             `json:"requestType,omitempty"`
	Request     geminiInnerRequest `json:"request"`
}

// geminiMode selects the vendor dialect that shares the v1internal envelope.
// Antigravity accepts the same contents but wants its own request metadata and
// client user agent.
type geminiMode struct {
	antigravity bool
}

func (m geminiMode) decorate(envelope *geminiEnvelope) {
	if !m.antigravity {
		return
	}
	envelope.RequestID = "agent-" + randomToken(16)
	envelope.UserAgent = "antigravity"
	envelope.RequestType = "agent"
}

// antigravityUserAgent is what the official client sends. The version can be
// pinned with JANUS_ANTIGRAVITY_VERSION when a deployment has to match a
// specific build.
func antigravityUserAgent() string {
	version := strings.TrimSpace(os.Getenv("JANUS_ANTIGRAVITY_VERSION"))
	if version == "" {
		version = "1.10.0"
	}
	return "antigravity/hub/" + version + " windows/amd64"
}

func buildGeminiRequest(in chatRequestIn, mode geminiMode) geminiEnvelope {
	envelope := geminiEnvelope{
		Model: in.Model,
		Request: geminiInnerRequest{
			GenerationConfig: &geminiGenerationConfig{Temperature: in.Temperature},
		},
	}
	if max := in.maxOutput(); max > 0 {
		envelope.Request.GenerationConfig.MaxOutputTokens = max
	}

	// A tool result names the function it answers, not the call id; track the
	// mapping while walking the assistant turns.
	callNames := map[string]string{}
	var system []string

	for _, message := range in.Messages {
		switch message.Role {
		case "system", "developer":
			for _, part := range parseContent(message.Content) {
				if part.Text != "" {
					system = append(system, part.Text)
				}
			}
		case "assistant":
			content := geminiContent{Role: "model"}
			for _, part := range parseContent(message.Content) {
				if part.Text != "" {
					content.Parts = append(content.Parts, geminiPart{Text: part.Text})
				}
			}
			for _, call := range message.ToolCalls {
				callNames[call.ID] = call.Function.Name
				content.Parts = append(content.Parts, geminiPart{
					FunctionCall: &struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args,omitempty"`
					}{Name: call.Function.Name, Args: decodeArguments(call.Function.Arguments)},
				})
			}
			if len(content.Parts) > 0 {
				appendGeminiContent(&envelope.Request.Contents, content)
			}
		case "tool":
			name := callNames[message.ToolCallID]
			if name == "" {
				name = "tool"
			}
			appendGeminiContent(&envelope.Request.Contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{FunctionResponse: &struct {
					Name     string         `json:"name"`
					Response map[string]any `json:"response,omitempty"`
				}{Name: name, Response: map[string]any{"result": contentText(message.Content)}}}},
			})
		default:
			content := geminiContent{Role: "user"}
			for _, part := range parseContent(message.Content) {
				if part.IsImage {
					if part.Data == "" {
						continue
					}
					content.Parts = append(content.Parts, geminiPart{InlineData: &struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					}{MimeType: part.MediaType, Data: part.Data}})
					continue
				}
				if part.Text != "" {
					content.Parts = append(content.Parts, geminiPart{Text: part.Text})
				}
			}
			if len(content.Parts) > 0 {
				appendGeminiContent(&envelope.Request.Contents, content)
			}
		}
	}

	if len(system) > 0 {
		envelope.Request.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(system, "\n\n")}},
		}
	}
	for _, tool := range in.Tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		declaration := struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: schema}
		if len(envelope.Request.Tools) == 0 {
			envelope.Request.Tools = []geminiTool{{}}
		}
		envelope.Request.Tools[0].FunctionDeclarations = append(envelope.Request.Tools[0].FunctionDeclarations, declaration)
	}
	if len(envelope.Request.Tools) > 0 && mode.antigravity {
		// Antigravity validates tool arguments against the schema by default;
		// saying so explicitly keeps the same behaviour when tools are present.
		config := &geminiToolConfig{}
		config.FunctionCallingConfig.Mode = "VALIDATED"
		envelope.Request.ToolConfig = config
	}
	mode.decorate(&envelope)
	return envelope
}

// appendGeminiContent merges consecutive turns of the same role: Gemini expects
// one content per role rather than a list of message objects.
func appendGeminiContent(contents *[]geminiContent, content geminiContent) {
	if len(*contents) > 0 && (*contents)[len(*contents)-1].Role == content.Role {
		last := &(*contents)[len(*contents)-1]
		last.Parts = append(last.Parts, content.Parts...)
		return
	}
	*contents = append(*contents, content)
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// unwrapGemini accepts both the Code Assist envelope ({"response": {...}}) and a
// bare generateContent response.
func unwrapGemini(raw []byte) (geminiResponse, error) {
	var envelope struct {
		Response *geminiResponse `json:"response"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Response != nil {
		return *envelope.Response, nil
	}
	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return geminiResponse{}, fmt.Errorf("decode gemini response: %w", err)
	}
	return parsed, nil
}

func geminiToChat(parsed geminiResponse, model string) ([]byte, error) {
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("%s: %s", parsed.Error.Status, parsed.Error.Message)
	}
	if len(parsed.Candidates) == 0 {
		reason := ""
		if parsed.PromptFeedback != nil {
			reason = parsed.PromptFeedback.BlockReason
		}
		if reason != "" {
			return nil, fmt.Errorf("prompt blocked: %s", reason)
		}
		return nil, errors.New("response has no candidates")
	}

	candidate := parsed.Candidates[0]
	var texts []string
	var calls []any
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.FunctionCall != nil {
			args, _ := json.Marshal(part.FunctionCall.Args)
			calls = append(calls, map[string]any{
				"id":   "call_" + randomToken(6),
				"type": "function",
				"function": map[string]any{
					"name":      part.FunctionCall.Name,
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
	completion := map[string]any{
		"id":      "chatcmpl_" + randomToken(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": geminiFinish(candidate.FinishReason, len(calls) > 0),
		}},
		"usage": map[string]any{
			"prompt_tokens":     parsed.UsageMetadata.PromptTokenCount,
			"completion_tokens": parsed.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      parsed.UsageMetadata.PromptTokenCount + parsed.UsageMetadata.CandidatesTokenCount,
		},
	}
	return json.Marshal(completion)
}

func geminiFinish(reason string, hasCalls bool) string {
	if hasCalls {
		return "tool_calls"
	}
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	}
	return "stop"
}

func geminiEndpoint(base, method string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1internal") {
		return base + ":" + method
	}
	return base + "/v1internal:" + method
}

// chatGemini calls generateContent and renders a chat completion.
func (c *Client) chatGemini(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	return c.chatGeminiMode(ctx, model, body, geminiMode{})
}

// chatAntigravity is the same call with Antigravity's envelope metadata.
func (c *Client) chatAntigravity(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	return c.chatGeminiMode(ctx, model, body, geminiMode{antigravity: true})
}

func (c *Client) chatGeminiMode(ctx context.Context, model string, body []byte, mode geminiMode) ([]byte, Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	envelope := buildGeminiRequest(in, mode)
	envelope.Model = model

	resp, credentialID, err := c.sendBuild(ctx, model, geminiEndpoint(c.BaseURL, "generateContent"), false,
		func(credential Credential) ([]byte, error) {
			envelope.Project = c.geminiProject(ctx, credential, mode)
			return json.Marshal(envelope)
		})
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: read response: %w", c.Name, err)
	}
	parsed, err := unwrapGemini(responseBody)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	converted, err := geminiToChat(parsed, model)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	return converted, Usage{
		ProviderID:       c.Name,
		CredentialID:     credentialID,
		Model:            model,
		PromptTokens:     parsed.UsageMetadata.PromptTokenCount,
		CompletionTokens: parsed.UsageMetadata.CandidatesTokenCount,
		CachedTokens:     parsed.UsageMetadata.CachedContentTokenCount,
	}, nil
}

// streamGemini forwards Code Assist SSE as chat chunks.
func (c *Client) streamGemini(ctx context.Context, model string, body []byte, dst io.Writer) (Usage, error) {
	return c.streamGeminiMode(ctx, model, body, dst, geminiMode{})
}

// streamAntigravity is the streaming half of the Antigravity dialect.
func (c *Client) streamAntigravity(ctx context.Context, model string, body []byte, dst io.Writer) (Usage, error) {
	return c.streamGeminiMode(ctx, model, body, dst, geminiMode{antigravity: true})
}

func (c *Client) streamGeminiMode(ctx context.Context, model string, body []byte, dst io.Writer, mode geminiMode) (Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	envelope := buildGeminiRequest(in, mode)
	envelope.Model = model

	endpoint := geminiEndpoint(c.BaseURL, "streamGenerateContent") + "?alt=sse"
	resp, credentialID, err := c.sendBuild(ctx, model, endpoint, true,
		func(credential Credential) ([]byte, error) {
			envelope.Project = c.geminiProject(ctx, credential, mode)
			return json.Marshal(envelope)
		})
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()

	sink := newChunkSink(dst, "", model)
	used := Usage{ProviderID: c.Name, CredentialID: credentialID, Model: model}
	finish := ""

	reader := newSSEReader(resp.Body)
	for {
		event, err := reader.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return used, fmt.Errorf("gateway %s: read stream: %w", c.Name, err)
		}
		payload := strings.TrimSpace(string(event.Data))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		parsed, err := unwrapGemini([]byte(payload))
		if err != nil {
			continue
		}
		if parsed.Error != nil && parsed.Error.Message != "" {
			return used, fmt.Errorf("gateway %s: %s: %s", c.Name, parsed.Error.Status, parsed.Error.Message)
		}
		if parsed.UsageMetadata.PromptTokenCount > 0 {
			used.PromptTokens = parsed.UsageMetadata.PromptTokenCount
		}
		if parsed.UsageMetadata.CandidatesTokenCount > 0 {
			used.CompletionTokens = parsed.UsageMetadata.CandidatesTokenCount
		}
		if parsed.UsageMetadata.CachedContentTokenCount > 0 {
			used.CachedTokens = parsed.UsageMetadata.CachedContentTokenCount
		}
		for _, candidate := range parsed.Candidates {
			for _, part := range candidate.Content.Parts {
				if err := sink.Text(part.Text); err != nil {
					return used, err
				}
				if part.FunctionCall != nil {
					if err := sink.ToolStart("call_"+randomToken(6), part.FunctionCall.Name); err != nil {
						return used, err
					}
					args, _ := json.Marshal(part.FunctionCall.Args)
					if err := sink.ToolArgs(string(args)); err != nil {
						return used, err
					}
				}
			}
			if candidate.FinishReason != "" {
				finish = geminiFinish(candidate.FinishReason, false)
			}
		}
	}
	if err := sink.Finish(finish); err != nil {
		return used, err
	}
	sink.SetUsage(used.PromptTokens, used.CompletionTokens, used.CachedTokens, model)
	if err := sink.Close(); err != nil {
		return used, err
	}
	return used, nil
}

// geminiProject resolves the cloud project the v1internal envelope needs. The
// answer is stable for an account, so it is cached per credential.
func (c *Client) geminiProject(ctx context.Context, credential Credential, mode geminiMode) string {
	c.projectMu.Lock()
	if cached, ok := c.projects[credential.ID]; ok && time.Now().Before(cached.expires) {
		project := cached.project
		c.projectMu.Unlock()
		return project
	}
	c.projectMu.Unlock()

	// Best effort: a project that cannot be read leaves the field empty and the
	// generation call reports the real error. It must not be reported to the
	// pool as an account failure.
	loadBody := `{"metadata":{"pluginType":"GEMINI_CLI"}}`
	if mode.antigravity {
		loadBody = `{"metadata":{"ideType":"ANTIGRAVITY","pluginType":"GEMINI"}}`
	}
	raw, ok := c.geminiPost(ctx, credential, "loadCodeAssist", []byte(loadBody))
	if !ok {
		return ""
	}
	project := parseGeminiProject(raw)
	if project == "" && mode.antigravity {
		// A fresh account often has no project until onboarding runs once.
		project = c.onboardAntigravity(ctx, credential, parseGeminiTier(raw))
	}

	c.projectMu.Lock()
	if c.projects == nil {
		c.projects = map[string]cachedProject{}
	}
	// An empty answer is cached too, but briefly: retrying every request would
	// add a round trip to each call, while caching it for the full window would
	// hide a project that onboarding just created.
	ttl := 30 * time.Minute
	if project == "" {
		ttl = time.Minute
	}
	c.projects[credential.ID] = cachedProject{project: project, expires: time.Now().Add(ttl)}
	c.projectMu.Unlock()
	return project
}

// geminiPost performs one metadata call with the given credential. It returns
// the raw body only on success.
func (c *Client) geminiPost(ctx context.Context, credential Credential, method string, body []byte) ([]byte, bool) {
	request, err := c.protoRequest(ctx, geminiEndpoint(c.BaseURL, method), body, credential, false)
	if err != nil {
		return nil, false
	}
	resp, err := c.HTTP.Do(request)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// onboardAntigravity creates the Code Assist project for an account that has
// none yet, which is what the official client does on first run.
func (c *Client) onboardAntigravity(ctx context.Context, credential Credential, tier string) string {
	if tier == "" {
		return ""
	}
	body, err := json.Marshal(map[string]any{
		"tierId": tier,
		"metadata": map[string]any{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	})
	if err != nil {
		return ""
	}
	raw, ok := c.geminiPost(ctx, credential, "onboardUser", body)
	if !ok {
		return ""
	}
	// The operation may still be running; a project id is what we need, so an
	// incomplete answer simply means "try again on the next call".
	var payload struct {
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return projectFromAny(payload.Response["cloudaicompanionProject"])
}

func projectFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if id, ok := typed["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func parseGeminiProject(raw []byte) string {
	var payload struct {
		CloudAICompanionProject any `json:"cloudaicompanionProject"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return projectFromAny(payload.CloudAICompanionProject)
}

// parseGeminiTier reads the account's tier, preferring the paid one.
func parseGeminiTier(raw []byte) string {
	var payload struct {
		PaidTier struct {
			ID string `json:"id"`
		} `json:"paidTier"`
		CurrentTier struct {
			ID string `json:"id"`
		} `json:"currentTier"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	if payload.PaidTier.ID != "" {
		return payload.PaidTier.ID
	}
	return payload.CurrentTier.ID
}

type cachedProject struct {
	project string
	expires time.Time
}
