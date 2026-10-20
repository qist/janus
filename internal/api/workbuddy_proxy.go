package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"janus/internal/channel/workbuddy"
	"janus/internal/db"
	"janus/internal/gateway"
)

// workbuddy_proxy.go —— WorkBuddy 渠道的直连透传。
//
// provider.auth_kind == "workbuddy" 时，凭据是 WorkBuddy 明文 JSON
// （accessToken / refreshToken / domain / uid / enterpriseId）。请求必须补齐
// 桌面身份头并强制 stream，这些都在 workbuddy 包里完成；本文件只负责：
//
//  1. 把客户端的 model 换成路由解析出的上游模型；
//  2. 客户端协议不是 openai-chat 时先翻译请求、后翻译响应；
//  3. accessToken 过期（401）时由 workbuddy 客户端自动 refresh 并重试，
//     新凭据通过 SetOnRefresh 回写数据库；
//  4. 上游固定返回 SSE：客户端要非流式时，把 SSE 聚合成一个 chat.completion。
func (s *Server) proxyWorkBuddy(
	w http.ResponseWriter,
	r *http.Request,
	model, protocol string,
	body []byte,
	stream bool,
	fail protoFail,
	st *db.Store,
	prov db.Provider,
	cred db.Credential,
	res gateway.RouteResult,
	tail *tailBuffer,
) (int, string) {
	plain, err := db.DecryptBlob(cred.Secret)
	if err != nil {
		fail(http.StatusBadGateway, "api_error", "cannot decrypt credential: "+err.Error(), "invalid_credential")
		return http.StatusBadGateway, "invalid_credential"
	}
	var secret map[string]any
	if err := json.Unmarshal(plain, &secret); err != nil {
		fail(http.StatusBadGateway, "api_error", "workbuddy credential is not a JSON object", "invalid_credential")
		return http.StatusBadGateway, "invalid_credential"
	}
	wbCred, err := workbuddy.CredentialFromSecret(secret)
	if err != nil {
		fail(http.StatusBadGateway, "api_error", err.Error(), "invalid_credential")
		return http.StatusBadGateway, "invalid_credential"
	}

	client := workbuddy.NewClient(wbCred, "")
	client.SetOnRefresh(func(next *workbuddy.Credential) {
		if _, uerr := st.UpdateCredential(cred.ID, nil, next.Secret(), nil, nil); uerr != nil {
			s.log.Warnf("workbuddy: persist refreshed credential %s failed: %v", cred.ID, uerr)
		}
	})

	// 客户端协议 → OpenAI chat（WorkBuddy 只支持 chat completions）。
	payload := body
	if protocol != "" && protocol != gateway.ProtocolOpenAIChat {
		if !gateway.CanTranslate(protocol, gateway.ProtocolOpenAIChat) {
			fail(http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("provider %s speaks openai-chat but this endpoint is %s", prov.ID, protocol),
				"protocol_mismatch")
			return http.StatusBadRequest, "protocol_mismatch"
		}
		payload, err = gateway.TranslateRequest(protocol, gateway.ProtocolOpenAIChat, body)
		if err != nil {
			fail(http.StatusBadRequest, "invalid_request_error", "cannot translate request: "+err.Error(), "invalid_request")
			return http.StatusBadRequest, "invalid_request"
		}
	}
	forwarded, err := replaceWireModel(payload, res.UpstreamID)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "cannot rewrite model: "+err.Error(), "")
		return http.StatusBadRequest, "invalid_request"
	}
	forwarded = workbuddy.PrepareBody(forwarded)

	s.log.Infof("passthrough(workbuddy): model=%q → %s/%s (%s) stream=%v",
		model, res.ProviderID, res.UpstreamID, cred.Label, stream)

	resp, err := client.Chat(r.Context(), forwarded)
	if err != nil {
		st.MarkCredentialError(cred.ID, err.Error(), time.Now().Add(30*time.Second).Unix())
		fail(http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error(), "upstream_error")
		return http.StatusBadGateway, "upstream_error"
	}
	defer resp.Body.Close()
	status := resp.StatusCode

	switch {
	case status >= 200 && status < 300:
		st.MarkCredentialUsed(cred.ID)
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusTooManyRequests, status >= 500:
		st.MarkCredentialError(cred.ID,
			fmt.Sprintf("upstream http %d", status), cooldownUntil(status))
	}

	echo := s.directEchoModel(model, res)
	translated := protocol != "" && protocol != gateway.ProtocolOpenAIChat

	if status >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tail.Write(raw)
		copyProxyResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if translated {
			raw = gateway.TranslateError(protocol, status, raw)
		}
		_, _ = w.Write(raw)
		return status, "upstream_http_" + strconv.Itoa(status)
	}

	copyProxyResponseHeaders(w.Header(), resp.Header)
	// WorkBuddy 固定返回 SSE；非流式请求要重新聚合成 JSON，长度类头不再成立。
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(status)

	if stream {
		tapped := io.TeeReader(resp.Body, tail)
		if translated {
			copyTranslatedStream(w, tapped, gateway.ProtocolOpenAIChat, protocol, echo)
		} else {
			copyStream(w, tapped, echo)
		}
		return status, ""
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return http.StatusBadGateway, "read_error"
	}
	tail.Write(raw)
	out := aggregateOpenAIStream(raw)
	if translated {
		if conv, terr := gateway.TranslateResponse(gateway.ProtocolOpenAIChat, protocol, out, echo); terr == nil {
			out = conv
		}
	} else if echo != "" {
		out = rewriteResponseModel(out, echo)
	}
	_, _ = w.Write(out)
	return status, ""
}

// wbDeltaTool 是流式 tool_calls 的累加器。
type wbDeltaTool struct {
	id   string
	name string
	args strings.Builder
}

// aggregateOpenAIStream 把 OpenAI chat 的 SSE 流合并成一个非流式响应。
func aggregateOpenAIStream(raw []byte) []byte {
	var (
		id, model string
		created   int64
		finish    string
		usage     map[string]any
		content   strings.Builder
		reasoning strings.Builder
		tools     = map[int]*wbDeltaTool{}
		order     []int
	)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal(payload, &chunk) != nil {
			continue
		}
		if id == "" {
			if v, ok := chunk["id"].(string); ok {
				id = v
			}
		}
		if model == "" {
			if v, ok := chunk["model"].(string); ok {
				model = v
			}
		}
		if created == 0 {
			if v, ok := chunk["created"].(float64); ok {
				created = int64(v)
			}
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, ch := range choices {
			cm, _ := ch.(map[string]any)
			if v, ok := cm["finish_reason"].(string); ok && v != "" {
				finish = v
			}
			delta, _ := cm["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				content.WriteString(v)
			}
			if v, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(v)
			}
			tcs, _ := delta["tool_calls"].([]any)
			for _, t := range tcs {
				tm, _ := t.(map[string]any)
				idx := 0
				if v, ok := tm["index"].(float64); ok {
					idx = int(v)
				}
				tool, ok := tools[idx]
				if !ok {
					tool = &wbDeltaTool{}
					tools[idx] = tool
					order = append(order, idx)
				}
				if v, ok := tm["id"].(string); ok && v != "" {
					tool.id = v
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					if v, ok := fn["name"].(string); ok && v != "" {
						tool.name = v
					}
					if v, ok := fn["arguments"].(string); ok {
						tool.args.WriteString(v)
					}
				}
			}
		}
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, idx := range order {
			t := tools[idx]
			calls = append(calls, map[string]any{
				"id": t.id, "type": "function",
				"function": map[string]any{"name": t.name, "arguments": t.args.String()},
			})
		}
		message["tool_calls"] = calls
		if content.Len() == 0 {
			delete(message, "content")
		}
	}
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	b, _ := json.Marshal(out)
	return b
}
