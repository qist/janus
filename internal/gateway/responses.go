package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// responsesRequest is the subset of the Responses API janus emits. Reasoning
// effort and include lists are left to the upstream default: forwarding an
// unknown parameter makes the Codex backend reject the whole request.
type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           []any           `json:"input"`
	Tools           []responsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Stream          bool            `json:"stream"`
	Store           bool            `json:"store"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// buildResponsesRequest converts an OpenAI chat body into a Responses body.
// System prompts become `instructions`, tool calls and results become typed
// input items, and images keep their data URL form.
func buildResponsesRequest(in chatRequestIn) responsesRequest {
	out := responsesRequest{
		Model:        in.Model,
		Instructions: systemText(in.Messages),
		Stream:       true, // the Codex backend only streams
		Store:        false,
	}
	if max := in.maxOutput(); max > 0 {
		out.MaxOutputTokens = max
	}

	for _, message := range in.Messages {
		switch message.Role {
		case "system", "developer":
			continue
		case "tool":
			out.Input = append(out.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  contentText(message.Content),
			})
		case "assistant":
			var content []any
			for _, part := range parseContent(message.Content) {
				if part.Text != "" {
					content = append(content, map[string]any{"type": "output_text", "text": part.Text})
				}
			}
			if len(content) > 0 {
				out.Input = append(out.Input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": content,
				})
			}
			for _, call := range message.ToolCalls {
				out.Input = append(out.Input, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				})
			}
		default:
			var content []any
			for _, part := range parseContent(message.Content) {
				if part.IsImage {
					content = append(content, map[string]any{
						"type":      "input_image",
						"image_url": part.ImageURL,
					})
					continue
				}
				if part.Text != "" {
					content = append(content, map[string]any{"type": "input_text", "text": part.Text})
				}
			}
			if len(content) == 0 {
				continue
			}
			out.Input = append(out.Input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": content,
			})
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
		out.Tools = append(out.Tools, responsesTool{
			Type:        "function",
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  schema,
		})
	}
	out.ToolChoice = responsesChoice(in.ToolChoice)
	return out
}

func responsesChoice(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null":
		return nil
	case `"auto"`:
		return "auto"
	case `"none"`:
		return "none"
	case `"required"`:
		return "required"
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err == nil && named.Function.Name != "" {
		return map[string]any{"type": "function", "name": named.Function.Name}
	}
	return nil
}

// chatResponses runs a Responses request and returns an OpenAI chat completion.
// The upstream call always streams because the Codex backend has no
// non-streaming mode, so the body is aggregated here.
func (c *Client) chatResponses(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	payload := buildResponsesRequest(in)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("gateway %s: encode responses request: %w", c.Name, err)
	}

	resp, credentialID, err := c.send(ctx, model, apiEndpoint(c.BaseURL, "responses", "v1"), raw, true)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()

	sink := &aggregateSink{model: model}
	used := Usage{ProviderID: c.Name, CredentialID: credentialID, Model: model}
	if err := translateResponses(resp.Body, sink, &used); err != nil {
		return nil, used, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	if sink.text.Len() == 0 && len(sink.tools) == 0 {
		return nil, used, fmt.Errorf("gateway %s: response has no output", c.Name)
	}
	sink.SetUsage(used.PromptTokens, used.CompletionTokens, used.CachedTokens, used.Model)
	return sink.Result(model), used, nil
}

// streamResponses translates Responses SSE into OpenAI chat chunks.
func (c *Client) streamResponses(ctx context.Context, model string, body []byte, dst io.Writer) (Usage, error) {
	var in chatRequestIn
	if err := json.Unmarshal(body, &in); err != nil {
		return Usage{}, fmt.Errorf("gateway %s: decode chat request: %w", c.Name, err)
	}
	payload := buildResponsesRequest(in)
	raw, err := json.Marshal(payload)
	if err != nil {
		return Usage{}, fmt.Errorf("gateway %s: encode responses request: %w", c.Name, err)
	}

	resp, credentialID, err := c.send(ctx, model, apiEndpoint(c.BaseURL, "responses", "v1"), raw, true)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()

	sink := newChunkSink(dst, "", model)
	used := Usage{ProviderID: c.Name, CredentialID: credentialID, Model: model}
	if err := translateResponses(resp.Body, sink, &used); err != nil {
		return used, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	sink.SetUsage(used.PromptTokens, used.CompletionTokens, used.CachedTokens, used.Model)
	if err := sink.Close(); err != nil {
		return used, err
	}
	return used, nil
}

// translateResponses walks the event stream once. Both the streaming and the
// aggregating path use it, so the two cannot drift apart.
func translateResponses(body io.Reader, sink chatSink, used *Usage) error {
	reader := newSSEReader(body)
	// itemIndex → whether deltas carried that function call's arguments.
	argDelta := map[int]bool{}
	finish := ""

	for {
		event, err := reader.next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read stream: %w", err)
		}
		if len(event.Data) == 0 {
			continue
		}

		var frame struct {
			Type     string `json:"type"`
			Response *struct {
				ID     string          `json:"id"`
				Model  string          `json:"model"`
				Status string          `json:"status"`
				Usage  *responsesUsage `json:"usage"`
			} `json:"response"`
			OutputIndex int `json:"output_index"`
			Item        *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Delta   string `json:"delta"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &frame); err != nil {
			continue
		}
		if frame.Type == "" {
			frame.Type = event.Name
		}

		switch frame.Type {
		case "response.created":
			if frame.Response != nil {
				if frame.Response.Model != "" {
					used.Model = frame.Response.Model
				}
				if sink, ok := sink.(*chunkSink); ok && frame.Response.ID != "" {
					sink.id = "chatcmpl_" + strings.TrimPrefix(frame.Response.ID, "resp_")
				}
			}
		case "response.output_item.added":
			if frame.Item == nil || frame.Item.Type != "function_call" {
				continue
			}
			id := frame.Item.CallID
			if id == "" {
				id = frame.Item.ID
			}
			if err := sink.ToolStart(id, frame.Item.Name); err != nil {
				return err
			}
		case "response.function_call_arguments.delta":
			argDelta[frame.OutputIndex] = true
			if err := sink.ToolArgs(frame.Delta); err != nil {
				return err
			}
		case "response.function_call_arguments.done":
			// Some backends send the whole argument string at the end.
			if !argDelta[frame.OutputIndex] && frame.Delta != "" {
				argDelta[frame.OutputIndex] = true
				if err := sink.ToolArgs(frame.Delta); err != nil {
					return err
				}
			}
		case "response.output_text.delta":
			if err := sink.Text(frame.Delta); err != nil {
				return err
			}
		case "response.output_item.done":
			if frame.Item == nil || frame.Item.Type != "function_call" {
				continue
			}
			if !argDelta[frame.OutputIndex] && frame.Item.Arguments != "" {
				argDelta[frame.OutputIndex] = true
				if err := sink.ToolArgs(frame.Item.Arguments); err != nil {
					return err
				}
			}
		case "response.completed", "response.incomplete":
			if frame.Response != nil && frame.Response.Usage != nil {
				readResponsesUsage(frame.Response.Usage, used)
			}
			finish = responsesFinish(frame.Type)
		case "response.failed":
			if frame.Response != nil && frame.Response.Usage != nil {
				readResponsesUsage(frame.Response.Usage, used)
			}
			return fmt.Errorf("upstream reported a failed response: %s", frame.Message)
		case "error":
			return fmt.Errorf("upstream error: %s", frame.Message)
		}
	}

	if err := sink.Finish(finish); err != nil {
		return err
	}
	return nil
}

// responsesResponse is the object carried by response.* events.
type responsesResponse struct {
	ID     string          `json:"id"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Usage  *responsesUsage `json:"usage"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func readResponsesUsage(raw *responsesUsage, used *Usage) {
	if raw.InputTokens > 0 {
		used.PromptTokens = raw.InputTokens
	}
	if raw.OutputTokens > 0 {
		used.CompletionTokens = raw.OutputTokens
	}
	if raw.InputTokensDetails != nil {
		used.CachedTokens = raw.InputTokensDetails.CachedTokens
	}
}

func responsesFinish(eventType string) string {
	if eventType == "response.incomplete" {
		return "length"
	}
	return "stop"
}

func nowUnix() int64 { return time.Now().Unix() }
