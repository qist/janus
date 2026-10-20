package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"janus/internal/agent"
	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/httpx"
	"janus/internal/modelx"
)

// passthrough.go —— 模型面直连透传（docs/REFACTOR.md §3.4，P1）。
//
// 与「IDE → janus → OpenCode 会话桥」并存的两条链路：
//
//	agent key（harness 自己跑 agent loop）  → 直连厂商，只做选路与凭据注入
//	client key（IDE 要会话/工具/权限）       → 走原有 OpenCode 会话桥
//
// 也可以显式指定 agent=direct（X-OpenCode-Agent 头或运行期设置）强制直连。
//
// 透传规则：只把请求体里的 model 换成路由解析出的 UpstreamID，其余字段与
// 响应字节原样转发；鉴权头由 providers/credentials 生成，绝不透传客户端的 key。

// proxyHTTP 返回直连厂商用的 HTTP client（nil 时退化为默认 client）。
func (s *Server) proxyHTTP() *http.Client {
	if s.proxyc != nil {
		return s.proxyc
	}
	return http.DefaultClient
}

// directDefaultModel 返回直连模式下的默认模型引用：
// 面板选择（DB）> 运行期设置 default_model。
func (s *Server) directDefaultModel() string {
	if v := strings.TrimSpace(s.runtimeDefaultModel()); v != "" {
		return v
	}
	return strings.TrimSpace(s.cfg.DefaultModel)
}

// routeDirect 解析一次直连请求的落点。
//
// 除正常路由外补两级兜底（§3.1 第 4/6 步的 P1 简化版）：
//   - default / auto / 空模型 / janus → 默认模型；
//   - Anthropic 档位名（claude-sonnet-4-5 之类）→ 默认模型。
//
// 这样 Claude Code 与各种只填 "default" 的客户端无需先配路由表就能用。
func (s *Server) routeDirect(p Principal, protocol, model, hint string) (gateway.RouteResult, error) {
	in := gateway.RouteInput{
		Model:          model,
		Protocol:       protocol,
		CredentialHint: hint,
		ProviderIDs:    p.Providers,
		CredentialIDs:  p.Credentials,
	}
	res, err := s.router.Route(in)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, gateway.ErrModelNotFound) {
		return gateway.RouteResult{}, err
	}

	// 只有在「模型名不是真实模型」时才回落，避免把拼错的模型名悄悄改掉。
	fallback := ""
	switch {
	case isDefaultAlias(model), isJanusAlias(model):
		fallback = s.directDefaultModel()
	case protocol == gateway.ProtocolAnthropic && modelx.TierOf(model) != "":
		fallback = s.directDefaultModel()
	}
	if fallback == "" {
		return gateway.RouteResult{}, err
	}
	in.Model = fallback
	alt, aerr := s.router.Route(in)
	if aerr != nil {
		// 默认模型也没配好：把原始错误还给客户端，别报成默认模型不存在。
		return gateway.RouteResult{}, err
	}
	s.log.Debugf("passthrough: %q → default %s (%s)", model, fallback, alt.Rule)
	return alt, nil
}

// listCatalogModels 用数据库里的模型目录回答 /v1/models（直连模式）。
// 客户端拿到的是 "provider/upstream" 或裸 upstream id，可直接回填给 chat 请求。
func (s *Server) listCatalogModels(w http.ResponseWriter, p Principal) {
	var st *db.Store
	if s.router != nil {
		st = s.router.Store()
	}
	if st == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error", "persistence is disabled", "disabled")
		return
	}
	allow := map[string]bool{}
	for _, id := range p.Providers {
		allow[id] = true
	}
	out := ModelList{Object: "list", Data: []ModelObject{}}
	for _, m := range st.ListModels("", true) {
		if len(allow) > 0 && !allow[m.ProviderID] {
			continue
		}
		out.Data = append(out.Data, ModelObject{
			ID:      m.ID,
			Object:  "model",
			Created: m.CreatedAt,
			OwnedBy: m.ProviderID,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// catalogModelDetail 把目录行投影成 /v1/models/{id} 的详情形状。
func catalogModelDetail(m db.Model) map[string]any {
	out := catalogModelView(m)
	out["object"] = "model"
	out["created"] = m.CreatedAt
	out["owned_by"] = m.ProviderID
	return out
}

// listCatalogAnthropicModels 用数据库目录回答 Anthropic 格式的模型列表
// （直连模式下 Claude Code 的模型选择器用）。
//
// 与 OpenAI 版不同，这里的 id 用「裸 upstream id」：Claude Code 会把选中的
// id 原样回填到 /v1/messages，而裸 id 在路由层能命中目录唯一项。
// display_name 里带上 provider 与真实目标，方便用户确认映射。
func (s *Server) listCatalogAnthropicModels(w http.ResponseWriter, p Principal) {
	var st *db.Store
	if s.router != nil {
		st = s.router.Store()
	}
	if st == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "persistence is disabled")
		return
	}
	allow := map[string]bool{}
	for _, id := range p.Providers {
		allow[id] = true
	}

	type item struct{ id, display string }
	var items []item
	seen := map[string]bool{}
	add := func(id, display string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		items = append(items, item{id, display})
	}

	// 1) 目录条目
	for _, m := range st.ListModels("", true) {
		if len(allow) > 0 && !allow[m.ProviderID] {
			continue
		}
		name := strings.TrimSpace(m.DisplayName)
		if name == "" {
			name = m.UpstreamID
		}
		add(m.UpstreamID, m.UpstreamID+" → "+m.ProviderID+"/"+name)
	}
	// 2) 路由表里的 claude/anthropic 别名
	for _, rt := range st.ListRoutes("") {
		if !rt.Enabled {
			continue
		}
		alias := strings.TrimSuffix(strings.TrimSpace(rt.Alias), "*")
		lk := strings.ToLower(alias)
		if !strings.HasPrefix(lk, "claude") && !strings.HasPrefix(lk, "anthropic") {
			continue
		}
		display := alias + " → " + rt.Target
		add(alias, display)
	}
	// 3) 内置档位别名：显示实际会落到哪个模型（没配默认模型就只显示别名）
	for _, t := range modelx.TierAliases {
		display := t.Alias
		if res, err := s.routeDirect(p, gateway.ProtocolAnthropic, t.Alias, ""); err == nil {
			display = t.Alias + " → " + res.ProviderID + "/" + res.UpstreamID
		}
		add(t.Alias, display)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	data := make([]map[string]any, 0, len(items))
	for _, it := range items {
		data = append(data, map[string]any{
			"type": "model", "id": it.id, "display_name": it.display,
			"created_at": "2025-01-01T00:00:00Z",
		})
	}
	first, last := "", ""
	if len(data) > 0 {
		first = data[0]["id"].(string)
		last = data[len(data)-1]["id"].(string)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": false, "first_id": first, "last_id": last,
	})
}

// protoFail 按各协议写出错误（OpenAI / Anthropic 信封不同）。
type protoFail func(status int, typ, msg, code string)

// directMode 判断本次请求是否走直连厂商，而不是 OpenCode 会话桥。
func (s *Server) directMode(r *http.Request, p Principal) bool {
	// 显式选择 runtime 时以它为准：X-Janus-Runtime: codex / claude-code 必须
	// 走 CLI 会话桥，不能因为外层是 agent key 就被当成直连透传。
	if name := strings.ToLower(strings.TrimSpace(r.Header.Get(runtimeHeader))); name != "" {
		return name == agent.NameDirect
	}
	if p.Static || s.router == nil || s.router.Store() == nil {
		return false
	}
	if p.Kind == db.KeyKindAgent {
		return true
	}
	agent := firstNonEmpty(r.Header.Get("X-OpenCode-Agent"), s.cfg.Agent)
	return strings.EqualFold(agent, "direct")
}

// proxyModel 把一次模型请求直连转发到厂商。
// 返回 false 表示「未接管」（路由层不可用），调用方应回落到会话桥。
func (s *Server) proxyModel(w http.ResponseWriter, r *http.Request, p Principal, protocol string, body []byte, fail protoFail) bool {
	if s.router == nil {
		return false
	}
	st := s.router.Store()
	if st == nil {
		return false
	}

	model, stream, err := decodeWireModel(body)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error(), "")
		return true
	}

	// 本地记账：解码完成后开始计一条；失败也记，便于按渠道看错误率与耗时。
	started := time.Now()
	ev := db.UsageEvent{
		Ts: started.Unix(), Model: model, Stream: stream,
		KeyID: p.Key.ID, KeyKind: p.Kind, Protocol: protocol,
	}
	status, errCode := 0, ""
	var tail tailBuffer
	failRec := func(code int, typ, msg, c string) {
		status, errCode = code, c
		fail(code, typ, msg, c)
	}
	defer func() {
		if status == 0 {
			status = http.StatusOK
		}
		ev.Status, ev.ErrorCode = status, errCode
		ev.DurationMs = time.Since(started).Milliseconds()
		s.commitUsage(st, ev, tail.Bytes())
	}()

	res, err := s.routeDirect(p, protocol, model,
		firstNonEmpty(r.Header.Get("X-Janus-Credential"), r.Header.Get("X-Janus-Account")))
	if err != nil {
		writeRouteFailure(failRec, err)
		return true
	}
	ev.ProviderID, ev.CredentialID, ev.UpstreamModel = res.ProviderID, res.CredentialID, res.UpstreamID

	prov, ok := st.GetProvider(res.ProviderID)
	if !ok || !prov.Enabled {
		failRec(http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("provider %q is not available", res.ProviderID), "model_not_found")
		return true
	}
	// 客户端协议与厂商协议不一致时走结构翻译（§3.4 translate.go）；
	// 没有翻译实现的组合直接拒绝，避免把请求原样发给说不通的厂商。
	upstreamProto := protocol
	if protocol != "" && prov.Type != "" && prov.Type != protocol {
		if !gateway.CanTranslate(protocol, prov.Type) {
			failRec(http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("provider %s speaks %s but this endpoint is %s; route this model to a %s provider",
					prov.ID, prov.Type, protocol, protocol),
				"protocol_mismatch")
			return true
		}
		upstreamProto = prov.Type
	}
	ev.Protocol = upstreamProto
	cred, ok := st.GetCredential(res.CredentialID)
	if !ok || !cred.Enabled {
		failRec(http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("credential for provider %s is not available", prov.ID), "no_available_credential")
		return true
	}
	// WorkBuddy 渠道走专用路径：桌面身份头 + refreshToken 续期 + 强制 stream。
	if prov.AuthKind == "workbuddy" {
		hs, hc := s.proxyWorkBuddy(w, r, model, protocol, body, stream, failRec, st, prov, cred, res, &tail)
		status, errCode = hs, hc
		return true
	}
	headers, err := credentialHTTP(prov, cred)
	if err != nil {
		failRec(http.StatusBadGateway, "api_error", err.Error(), "invalid_credential")
		return true
	}
	endpoint := upstreamEndpoint(prov, upstreamProto)
	if endpoint == "" {
		failRec(http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("provider %s has no base_url", prov.ID), "invalid_provider")
		return true
	}

	payload := body
	if upstreamProto != protocol {
		payload, err = gateway.TranslateRequest(protocol, upstreamProto, body)
		if err != nil {
			failRec(http.StatusBadRequest, "invalid_request_error",
				"cannot translate request: "+err.Error(), "invalid_request")
			return true
		}
	}
	forwarded, err := replaceWireModel(payload, res.UpstreamID)
	if err != nil {
		failRec(http.StatusBadRequest, "invalid_request_error", "cannot rewrite model: "+err.Error(), "")
		return true
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(forwarded))
	if err != nil {
		failRec(http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return true
	}
	copyProxyHeaders(req.Header, r.Header)
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(forwarded))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	s.log.Infof("passthrough: model=%q → %s/%s (%s) stream=%v rule=%s",
		model, res.ProviderID, res.UpstreamID, cred.Label, stream, res.Rule)

	resp, err := s.proxyHTTP().Do(req)
	if err != nil {
		st.MarkCredentialError(cred.ID, err.Error(), time.Now().Add(30*time.Second).Unix())
		failRec(http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error(), "upstream_error")
		return true
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		st.MarkCredentialUsed(cred.ID)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		// 凭据/上游问题：进冷却窗口，路由层随后会跳过它。
		st.MarkCredentialError(cred.ID,
			fmt.Sprintf("upstream http %d", resp.StatusCode), cooldownUntil(resp.StatusCode))
		errCode = "upstream_http_" + strconv.Itoa(resp.StatusCode)
	default:
		// 400/404/422 是请求本身的问题（模型名、参数、上下文超限），
		// 惩罚凭据只会让下一轮无谓地换 key，这里不记。
		if resp.StatusCode >= 400 {
			errCode = "upstream_http_" + strconv.Itoa(resp.StatusCode)
		}
	}

	// 响应里的 model 字段按 settings.model_echo 回显（§3.4）。
	echo := s.directEchoModel(model, res)
	translated := upstreamProto != protocol

	if translated && resp.StatusCode >= 300 {
		// 错误体也要换信封：客户端只认得自己那套 error 形状。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tail.Write(raw)
		copyProxyResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(gateway.TranslateError(protocol, resp.StatusCode, raw))
		return true
	}

	copyProxyResponseHeaders(w.Header(), resp.Header)
	if translated {
		// 翻译后的响应体形状变了，长度类头不再成立。
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
	}
	w.WriteHeader(resp.StatusCode)

	if stream {
		// 流式：逐行转发并 flush，保持首包延迟；仅按需改写 model 字段。
		// TeeReader 把尾部字节留给 usage 解析（usage 总在最后的事件里）。
		tapped := io.TeeReader(resp.Body, &tail)
		if translated {
			copyTranslatedStream(w, tapped, upstreamProto, protocol, echo)
			return true
		}
		copyStream(w, tapped, echo)
		return true
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		status, errCode = http.StatusBadGateway, "read_error"
		return true
	}
	tail.Write(data)
	if translated {
		if out, terr := gateway.TranslateResponse(upstreamProto, protocol, data, echo); terr == nil {
			data = out
		}
	} else if echo != "" {
		data = rewriteResponseModel(data, echo)
	}
	_, _ = w.Write(data)
	return true
}

// copyTranslatedStream 边翻译边转发 SSE，并逐事件 flush。
func copyTranslatedStream(w http.ResponseWriter, body io.Reader, from, to, echo string) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := gateway.TranslateStream(pw, body, from, to, echo)
		_ = pw.CloseWithError(err)
	}()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				_ = pr.Close()
				<-done
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			_ = pr.Close()
			<-done
			return
		}
	}
}

// directEchoModel 返回响应里应当回显的 model 名：
//
//	request → 客户端请求里的原名（如 claude-sonnet-4-5）
//	real    → provider/upstream（如 anthropic/claude-sonnet-4-5）
func (s *Server) directEchoModel(requested string, res gateway.RouteResult) string {
	if s.cfg.ModelEcho == "request" {
		return strings.TrimSpace(requested)
	}
	if res.ProviderID == "" || res.UpstreamID == "" {
		return ""
	}
	return res.ProviderID + "/" + res.UpstreamID
}

// cooldownUntil 返回凭据冷却截止时间（Unix 秒）。
//
// 鉴权类错误多半是 key 失效，冷却久一点；限流按厂商建议的窗口退避；
// 5xx 可能只是上游抖动，短冷却即可（也让路由层有机会试下一份凭据）。
func cooldownUntil(status int) int64 {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return time.Now().Add(5 * time.Minute).Unix()
	case status == http.StatusTooManyRequests:
		return time.Now().Add(time.Minute).Unix()
	case status >= 500:
		return time.Now().Add(30 * time.Second).Unix()
	default:
		return 0
	}
}

// writeRouteFailure 把路由错误映射成 HTTP 语义（见 REFACTOR §3.3）。
func writeRouteFailure(fail protoFail, err error) {
	var amb *gateway.AmbiguousError
	switch {
	case errors.As(err, &amb):
		fail(http.StatusConflict, "invalid_request_error", err.Error(), "ambiguous_model")
	case errors.Is(err, gateway.ErrNoCredential):
		fail(http.StatusServiceUnavailable, "api_error", err.Error(), "no_available_credential")
	case errors.Is(err, gateway.ErrInvalidRoute):
		fail(http.StatusBadRequest, "invalid_request_error", err.Error(), "invalid_route")
	default:
		fail(http.StatusNotFound, "invalid_request_error", err.Error(), "model_not_found")
	}
}

// openAIFail 按 OpenAI 的错误体写错误。
func openAIFail(w http.ResponseWriter) protoFail {
	return func(status int, typ, msg, code string) {
		writeOpenAIError(w, status, typ, msg, code)
	}
}

// anthropicFail 按 Anthropic 的错误类型写错误体。
func anthropicFail(w http.ResponseWriter) protoFail {
	return func(status int, typ, msg, _ string) {
		switch status {
		case http.StatusNotFound:
			typ = "not_found_error"
		case http.StatusBadRequest, http.StatusConflict:
			typ = "invalid_request_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case http.StatusUnauthorized:
			typ = "authentication_error"
		}
		writeAnthropicError(w, status, typ, msg)
	}
}

// upstreamEndpoint 拼出厂商的模型端点。
//
// 三种 base_url 写法都要支持：
//
//	https://api.openai.com/v1        → + /chat/completions
//	https://api.anthropic.com        → + /v1/messages
//	https://host/v1/messages         → 已经是完整端点，原样使用
func upstreamEndpoint(p db.Provider, protocol string) string {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return ""
	}
	var path string
	switch protocol {
	case gateway.ProtocolAnthropic:
		path = "/messages"
	case gateway.ProtocolOpenAIResponses:
		path = "/responses"
	default:
		path = "/chat/completions"
	}
	if strings.HasSuffix(base, path) {
		return base
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// 不是完整 URL（测试里常见 base 为裸主机）：按字符串拼接兜底。
		if strings.HasSuffix(base, "/v1") || strings.Contains(base, "/v1/") {
			return base + path
		}
		return base + "/v1" + path
	}
	// 只看路径部分判断是否已带版本段，避免 host 里的 "v1" 干扰。
	p0 := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(p0, "/v1") || strings.Contains(p0, "/v1/") {
		u.Path = p0 + path
	} else {
		u.Path = p0 + "/v1" + path
	}
	return u.String()
}

// decodeWireModel 取出请求体的 model 与 stream，不做完整结构解析。
func decodeWireModel(body []byte) (model string, stream bool, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", false, err
	}
	if raw, ok := obj["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if raw, ok := obj["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	return model, stream, nil
}

// replaceWireModel 只替换 model 字段，其余字节原样保留。
//
// 不用 map[string]json.RawMessage 反序列化再序列化：那会丢掉键顺序、
// 把数字规范化（1.0 → 1）、压缩空白。厂商侧有些实现会校验原始 payload
// 或依赖字段顺序做缓存键，所以这里做「定位 + 就地替换」。
func replaceWireModel(body []byte, upstreamID string) ([]byte, error) {
	return replaceWireModelDepth(body, upstreamID, 1)
}

// replaceWireModelDepth 在 maxDepth 层对象内定位 model 并替换其值。
// SSE 的 Anthropic message_start 里 model 在 message 对象内（第 2 层），
// 所以流式改写需要 depth=2。
func replaceWireModelDepth(body []byte, upstreamID string, maxDepth int) ([]byte, error) {
	start, end, ok, err := findJSONFieldDepth(body, "model", maxDepth)
	if err != nil {
		return nil, err
	}
	val, err := json.Marshal(upstreamID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 没有 model 字段（少见）：按顶层对象插入到开头。
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, errors.New("request body must be a JSON object")
		}
		out := make([]byte, 0, len(body)+len(val)+10)
		out = append(out, '{')
		out = append(out, `"model":`...)
		out = append(out, val...)
		rest := bytes.TrimSpace(trimmed[1:])
		if len(rest) > 0 && rest[0] != '}' {
			out = append(out, ',')
		}
		out = append(out, rest...)
		return out, nil
	}
	out := make([]byte, 0, len(body)-(end-start)+len(val))
	out = append(out, body[:start]...)
	out = append(out, val...)
	out = append(out, body[end:]...)
	return out, nil
}

// findJSONField 在顶层对象里定位某个键的值区间 [start,end)。
func findJSONField(body []byte, key string) (start, end int, found bool, err error) {
	return findJSONFieldDepth(body, key, 1)
}

// findJSONFieldDepth 在最多 maxDepth 层嵌套对象里定位键值区间。
//
// 用一个极简扫描器而不是完整反序列化：只跟踪字符串/转义/嵌套深度，
// 足以跳过任意嵌套值，同时保持原字节不变。
func findJSONFieldDepth(body []byte, key string, maxDepth int) (start, end int, found bool, err error) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return 0, 0, false, errors.New("request body must be a JSON object")
	}
	i++
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return 0, 0, false, errors.New("unexpected end of JSON object")
		}
		if body[i] == '}' {
			return 0, 0, false, nil
		}
		if body[i] == ',' {
			i++
			continue
		}
		if body[i] != '"' {
			return 0, 0, false, errors.New("malformed JSON object: expected key")
		}
		nameEnd, err := skipJSONString(body, i)
		if err != nil {
			return 0, 0, false, err
		}
		var name string
		if err := json.Unmarshal(body[i:nameEnd], &name); err != nil {
			return 0, 0, false, err
		}
		i = skipJSONSpace(body, nameEnd)
		if i >= len(body) || body[i] != ':' {
			return 0, 0, false, errors.New("malformed JSON object: expected ':'")
		}
		i = skipJSONSpace(body, i+1)
		valStart := i
		valEnd, err := skipJSONValue(body, i)
		if err != nil {
			return 0, 0, false, err
		}
		if name == key {
			return valStart, valEnd, true, nil
		}
		if maxDepth > 1 && valStart < len(body) && body[valStart] == '{' {
			if s, e, ok, nerr := findJSONFieldDepth(body[valStart:valEnd], key, maxDepth-1); nerr == nil && ok {
				return valStart + s, valStart + e, true, nil
			}
		}
		i = valEnd
	}
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString 返回字符串字面量的结束位置（不含引号后的下标）。
func skipJSONString(b []byte, i int) (int, error) {
	if i >= len(b) || b[i] != '"' {
		return 0, errors.New("malformed JSON: expected string")
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, errors.New("malformed JSON: unterminated string")
}

// skipJSONValue 返回任意 JSON 值的结束位置。
func skipJSONValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errors.New("unexpected end of JSON value")
	}
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		open, closeCh := b[i], byte('}')
		if open == '[' {
			closeCh = ']'
		}
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				next, err := skipJSONString(b, j)
				if err != nil {
					return 0, err
				}
				j = next - 1
			case open:
				depth++
			case closeCh:
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errors.New("malformed JSON: unbalanced brackets")
	default:
		j := i
		for j < len(b) {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return j, nil
			}
			j++
		}
		return j, nil
	}
}

// rewriteResponseModel 改写响应体里的顶层 model 字段（非流式）。
func rewriteResponseModel(body []byte, name string) []byte {
	if name == "" {
		return body
	}
	out, err := replaceWireModel(body, name)
	if err != nil {
		return body
	}
	return out
}

// copyProxyHeaders 转发客户端头，剥掉 hop-by-hop 与 janus 自己的鉴权头。
func copyProxyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "host", "content-length", "connection",
			"keep-alive", "proxy-authorization", "proxy-connection", "te", "trailer",
			"transfer-encoding", "upgrade", "cookie", "accept-encoding":
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "x-janus-") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyProxyResponseHeaders 转发上游响应头，剥掉 hop-by-hop 与长度类头。
func copyProxyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "content-length", "connection", "keep-alive", "transfer-encoding",
			"trailer", "upgrade":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyStream 逐行转发 SSE 并 flush，保证首包即时可见。
//
// echo 为空时是纯字节转发；非空时只改每个 data: 事件里的 model 字段，
// 其余字节（含注释、event: 行、[DONE]）原样透传。
func copyStream(w http.ResponseWriter, body io.Reader, echo string) {
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	if echo == "" {
		buf := make([]byte, 32<<10)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				flush()
			}
			if err != nil {
				return
			}
		}
	}

	// 按行处理：SSE 事件以空行分隔，单行长度不设上限（个别厂商会整包发一行）。
	r := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			out := rewriteSSELine(line, echo)
			if _, werr := w.Write([]byte(out)); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

// rewriteSSELine 改写单行 SSE 里的 model 字段；非 data 行原样返回。
func rewriteSSELine(line, echo string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" || payload[0] != '{' {
		return line
	}
	out, err := replaceWireModelDepth([]byte(payload), echo, 2)
	if err != nil {
		return line
	}
	// 保留原有行尾（\n 或 \r\n），避免破坏 SSE 解析。
	suffix := line[len(trimmed):]
	return "data: " + string(out) + suffix
}
