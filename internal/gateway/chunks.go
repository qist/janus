package gateway

import (
	"bytes"
	"encoding/json"
	"io"
)

// ToolCallDelta is one piece of a streamed tool call. Name usually arrives with
// the first piece and Arguments is appended across pieces.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ChatChunk is one parsed delta of an OpenAI-style stream, already normalised
// by the protocol adapters. Callers that expose a different wire format (for
// example the inbound Messages endpoint) consume this instead of re-parsing SSE.
type ChatChunk struct {
	ID           string
	Model        string
	Content      string
	ToolCalls    []ToolCallDelta
	FinishReason string
	Usage        *Usage
}

// StreamChatChunks reads data: frames until [DONE] or EOF. A malformed frame is
// skipped: one bad delta must not abort a stream that is otherwise usable.
func StreamChatChunks(r io.Reader, fn func(ChatChunk) error) error {
	reader := newSSEReader(r)
	for {
		event, err := reader.next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		payload := bytes.TrimSpace(event.Data)
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			return nil
		}

		var frame struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *usageWire `json:"usage"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue
		}

		chunk := ChatChunk{ID: frame.ID, Model: frame.Model}
		for _, choice := range frame.Choices {
			if choice.Delta.Content != "" {
				chunk.Content += choice.Delta.Content
			}
			for _, call := range choice.Delta.ToolCalls {
				chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
					Index:     call.Index,
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				})
			}
			if choice.FinishReason != nil {
				chunk.FinishReason = *choice.FinishReason
			}
		}
		if frame.Usage != nil {
			used := Usage{
				PromptTokens:     frame.Usage.PromptTokens,
				CompletionTokens: frame.Usage.CompletionTokens,
			}
			if frame.Usage.PromptTokensDetails != nil {
				used.CachedTokens = frame.Usage.PromptTokensDetails.CachedTokens
			}
			chunk.Usage = &used
		}
		if chunk.Content == "" && len(chunk.ToolCalls) == 0 && chunk.FinishReason == "" && chunk.Usage == nil {
			continue
		}
		if err := fn(chunk); err != nil {
			return err
		}
	}
}
