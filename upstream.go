package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OpenCode 上游客户端。所有请求走 Basic: opencode:<OPENCODE_SERVER_PASSWORD>。
//
// base/auth 可在运行期热替换：OpenCode 桌面端每次重启都会换端口和密码，
// 所以 endpoint 受 RWMutex 保护，由 discover.go 探测到新端点后调用 SetEndpoint。
type Upstream struct {
	mu     sync.RWMutex
	base   string
	auth   string
	user   string
	pass   string
	client *http.Client
	log    *Logger
}

func NewUpstream(cfg Config, log *Logger) *Upstream {
	u := &Upstream{
		client: &http.Client{
			// 不设整体 Timeout：prompt / wait 可能长时间挂起，
			// 超时由调用方的 context 控制。
			Transport: withUserAgent(&http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			}),
		},
		log: log,
	}
	u.SetEndpoint(cfg.Upstream, cfg.Username, cfg.Password)
	return u
}

// SetEndpoint 热替换上游地址与凭据。
func (u *Upstream) SetEndpoint(base, user, pass string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	creds := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	u.mu.Lock()
	u.base, u.user, u.pass = base, user, pass
	u.auth = "Basic " + creds
	u.mu.Unlock()
}

// Endpoint 返回当前 (base, user, pass) 快照。
func (u *Upstream) Endpoint() (base, user, pass string) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.base, u.user, u.pass
}

func (u *Upstream) endpoint() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.base
}

func (u *Upstream) authHeader() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.auth
}

// Ping 探活：只有真正返回 2xx 才算通。
func (u *Upstream) Ping(ctx context.Context) error {
	return u.do(ctx, http.MethodGet, "/api/info", nil, nil, nil)
}

type APIError struct {
	Status int
	Tag    string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("opencode %d %s: %s", e.Status, e.Tag, e.Msg)
}

// shortTimeout 是「短操作」的兜底超时。
//
// Upstream 的 http.Client 刻意不设 Timeout（prompt / wait 可能长时间挂起），
// 于是模型的列举、会话增删改这些短操作一旦遇到上游不可达（例如 SYN 被丢包而不是
// 立刻 RST），就会挂到内核的 connect 超时（实测 ~127s，内部调用两次后达 270s）。
// 调用方通常只传 r.Context()，没有 deadline，所以这里补一个。
const (
	shortTimeout  = 30 * time.Second
	promptTimeout = 120 * time.Second
)

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// do 用于「短操作」：模型列表、会话增删改、消息拉取等。
func (u *Upstream) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	ctx, cancel := withDefaultTimeout(ctx, shortTimeout)
	defer cancel()
	return u.doRaw(ctx, method, path, query, body, out)
}

// doLong 用于可能长时间挂起的调用（prompt、wait）。
func (u *Upstream) doLong(ctx context.Context, d time.Duration, method, path string, query url.Values, body any, out any) error {
	ctx, cancel := withDefaultTimeout(ctx, d)
	defer cancel()
	return u.doRaw(ctx, method, path, query, body, out)
}

func (u *Upstream) doRaw(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	uPath := u.endpoint() + path
	if len(query) > 0 {
		uPath += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, uPath, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", u.authHeader())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		ae := &APIError{Status: resp.StatusCode}
		var e struct {
			Tag     string `json:"_tag"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil {
			ae.Tag, ae.Msg = e.Tag, e.Message
		}
		if ae.Msg == "" {
			ae.Msg = strings.TrimSpace(string(raw))
		}
		return ae
	}

	if out == nil {
		return nil
	}
	// out 非 nil 说明调用方期望 JSON。空响应体不能当成功 —— 否则会把
	// nil/零值悄悄写进缓存（实测：上游偶发空 body 让模型列表整个"消失"）。
	if len(raw) == 0 {
		return fmt.Errorf("empty response body from %s %s", method, path)
	}
	return json.Unmarshal(raw, out)
}

// ---------- 模型 ----------

type OCModel struct {
	ID         string   `json:"id"`
	ModelID    string   `json:"modelID"`
	ProviderID string   `json:"providerID"`
	Name       string   `json:"name"`
	Enabled    bool     `json:"enabled"`
	Status     string   `json:"status"`
	Family     string   `json:"family"`
	Package    string   `json:"package"`
	Cost       []OCCost `json:"cost"`
	Limit      struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Variants []struct {
		ID       string          `json:"id"`
		Settings json.RawMessage `json:"settings,omitempty"`
	} `json:"variants"`
	Capabilities struct {
		Tools  bool     `json:"tools"`
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"capabilities"`
	Time struct {
		Released int64 `json:"released"`
	} `json:"time"`
}

func (m OCModel) TimeReleased() int64 {
	if m.Time.Released > 0 {
		return m.Time.Released
	}
	return time.Now().UnixMilli()
}

type OCModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// OCCost 是模型的一个价格档位（上游可按上下文长度分档）。
type OCCost struct {
	Tier *struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier,omitempty"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Cache  struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
	} `json:"cache"`
}

func (r OCModelRef) String() string {
	s := r.ProviderID + "/" + r.ID
	if r.Variant != "" && r.Variant != "default" {
		s += ":" + r.Variant
	}
	return s
}

// ListModels 注意：必须带 directory，否则返回空数组。
func (u *Upstream) ListModels(ctx context.Context, directory string) ([]OCModel, error) {
	q := url.Values{}
	if directory != "" {
		q.Set("directory", directory)
	}
	var wrap struct {
		Data []OCModel `json:"data"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/model", q, nil, &wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// DefaultModel 取上游当前默认模型。注意实测该值全局一致，不随 directory 变化。
func (u *Upstream) DefaultModel(ctx context.Context, directory string) (*OCModel, error) {
	q := url.Values{}
	if directory != "" {
		q.Set("directory", directory)
	}
	var wrap struct {
		Data OCModel `json:"data"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/model/default", q, nil, &wrap); err != nil {
		return nil, err
	}
	if wrap.Data.ID == "" || wrap.Data.ProviderID == "" {
		return nil, fmt.Errorf("upstream returned an empty default model")
	}
	return &wrap.Data, nil
}

// ---------- MCP（把客户端的 tools 暴露给 agent）----------

// AddMCP 注册/更新一个 remote MCP server。
//
// OpenCode 的 MCP 注册是按 directory 维度存的，所以必须带 location[directory]。
// execTimeout 是「agent 调用该 MCP 工具后允许等多久」，必须足够长 ——
// 工具的真实执行要等客户端在后续请求里回填结果。
func (u *Upstream) AddMCP(ctx context.Context, directory, name, mcpURL string,
	headers map[string]string, execTimeout time.Duration) error {

	q := url.Values{}
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	cfg := map[string]any{
		"type": "remote",
		"url":  mcpURL,
	}
	if len(headers) > 0 {
		cfg["headers"] = headers
	}
	if execTimeout > 0 {
		cfg["timeout"] = map[string]any{
			"startup":   15000,
			"catalog":   15000,
			"execution": execTimeout.Milliseconds(),
		}
	}
	err := u.do(ctx, http.MethodPut, "/api/experimental/mcp/"+url.PathEscape(name), q,
		map[string]any{"config": cfg}, nil)
	if err != nil {
		// 上游这个接口出错时常常是 500 + 空 body，包一层上下文才有可读性
		return fmt.Errorf("register MCP server %q for directory %q: %w", name, directory, err)
	}
	return nil
}

// RemoveMCP 注销一个 MCP server。
func (u *Upstream) RemoveMCP(ctx context.Context, directory, name string) error {
	q := url.Values{}
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	if err := u.do(ctx, http.MethodDelete, "/api/experimental/mcp/"+url.PathEscape(name), q, nil, nil); err != nil {
		return fmt.Errorf("remove MCP server %q for directory %q: %w", name, directory, err)
	}
	return nil
}

// ListMCP 返回当前已注册的 MCP server 名字。
func (u *Upstream) ListMCP(ctx context.Context, directory string) ([]string, error) {
	q := url.Values{}
	if directory != "" {
		// OpenCode 只认 location[directory]；传 directory 会被忽略并落到默认
		// 位置（/root），janitor 因此永远扫不到真正注册的那些 ob-* 条目。
		q.Set("location[directory]", directory)
	}
	var wrap struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/mcp", q, nil, &wrap); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(wrap.Data))
	for _, s := range wrap.Data {
		out = append(out, s.Name)
	}
	return out, nil
}

// ReplyPermission 应答 OpenCode 的权限请求。
// decision: once（仅本次）/ always（记住）/ reject（拒绝）。
// 不应答的话，等待审批的工具会一直挂住（headless 桥没有 UI 去点）。
func (u *Upstream) ReplyPermission(ctx context.Context, sessionID, requestID, decision string) error {
	if sessionID == "" || requestID == "" {
		return fmt.Errorf("permission reply: missing session or request id")
	}
	path := "/api/session/" + url.PathEscape(sessionID) + "/permission/" + url.PathEscape(requestID) + "/reply"
	body := map[string]string{"decision": decision}
	return u.do(ctx, http.MethodPost, path, nil, body, nil)
}

// OCWebResult 是 OpenCode websearch 的一条结果。
type OCWebResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// WebSearch 调用 OpenCode 的 websearch（供 Claude Code 的 web_search 服务端工具使用）。
func (u *Upstream) WebSearch(ctx context.Context, directory, query string) ([]OCWebResult, string, error) {
	q := url.Values{}
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	var wrap struct {
		Data struct {
			ProviderID string        `json:"providerID"`
			Results    []OCWebResult `json:"results"`
		} `json:"data"`
	}
	if err := u.do(ctx, http.MethodPost, "/api/websearch", q, map[string]string{"query": query}, &wrap); err != nil {
		return nil, "", err
	}
	return wrap.Data.Results, wrap.Data.ProviderID, nil
}

// OCPermission 是 OpenCode 待审批权限请求的一条。
type OCPermission struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

// ListPermissions 列出某位置下所有待审批的权限请求。
// 权限事件可能落在两次请求之间的空档里（agent 异步跑自己的工具），
// 单靠事件流会漏，需要定时扫描兜底。
func (u *Upstream) ListPermissions(ctx context.Context, directory string) ([]OCPermission, error) {
	q := url.Values{}
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	var wrap struct {
		Data []OCPermission `json:"data"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/permission/request", q, nil, &wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// ---------- 会话 ----------

type OCSession struct {
	ID        string      `json:"id"`
	ProjectID string      `json:"projectID"`
	Agent     string      `json:"agent"`
	Model     *OCModelRef `json:"model"`
	Title     string      `json:"title,omitempty"`
	Location  struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
		Idle    int64 `json:"idle"`
	} `json:"time"`
	Outcome string `json:"outcome,omitempty"`
}

type CreateSessionReq struct {
	Title    string      `json:"title,omitempty"`
	Agent    string      `json:"agent,omitempty"`
	Model    *OCModelRef `json:"model,omitempty"`
	Location *struct {
		Directory string `json:"directory"`
	} `json:"location,omitempty"`
}

func (u *Upstream) CreateSession(ctx context.Context, in CreateSessionReq) (*OCSession, error) {
	var wrap struct {
		Data OCSession `json:"data"`
	}
	if err := u.do(ctx, http.MethodPost, "/api/session", nil, in, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

func (u *Upstream) GetSession(ctx context.Context, sid string) (*OCSession, error) {
	var wrap struct {
		Data OCSession `json:"data"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/session/"+sid, nil, nil, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

func (u *Upstream) DeleteSession(ctx context.Context, sid string) error {
	return u.do(ctx, http.MethodDelete, "/api/session/"+sid, nil, nil, nil)
}

func (u *Upstream) SetModel(ctx context.Context, sid string, m OCModelRef) error {
	return u.do(ctx, http.MethodPost, "/api/session/"+sid+"/model", nil,
		map[string]any{"model": m}, nil)
}

func (u *Upstream) SetAgent(ctx context.Context, sid, agent string) error {
	return u.do(ctx, http.MethodPost, "/api/session/"+sid+"/agent", nil,
		map[string]any{"agent": agent}, nil)
}

func (u *Upstream) Interrupt(ctx context.Context, sid string) error {
	return u.do(ctx, http.MethodPost, "/api/session/"+sid+"/interrupt", nil, nil, nil)
}

type OCPromptReq struct {
	Text     string         `json:"text"`
	Files    []OCFileAttach `json:"files,omitempty"`
	Delivery string         `json:"delivery,omitempty"`
	Resume   *bool          `json:"resume,omitempty"`
}

// OCFileAttach 对应 PromptInput.FileAttachment，uri 是必填项。
// data: URI 上游会解析成 source={type:inline} + base64 data。
type OCFileAttach struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type OCPromptResp struct {
	Data struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Time      struct {
			Created int64 `json:"created"`
		} `json:"time"`
	} `json:"data"`
}

func (u *Upstream) Prompt(ctx context.Context, sid string, in OCPromptReq) (*OCPromptResp, error) {
	if in.Delivery == "" {
		in.Delivery = "queue"
	}
	var wrap OCPromptResp
	if err := u.doLong(ctx, promptTimeout, http.MethodPost, "/api/session/"+sid+"/prompt", nil, in, &wrap); err != nil {
		return nil, err
	}
	return &wrap, nil
}

// ---------- 消息 ----------

type OCTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Reason int `json:"reasoning"`
	Cache  struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type OCContentItem struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	ID    string          `json:"id,omitempty"`
	State json.RawMessage `json:"state,omitempty"`
}

type OCMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64 `json:"created"`
		Streamed  int64 `json:"streamed"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Agent   string          `json:"agent,omitempty"`
	Model   *OCModelRef     `json:"model,omitempty"`
	Text    string          `json:"text,omitempty"`
	Content []OCContentItem `json:"content,omitempty"`
	Finish  string          `json:"finish,omitempty"`
	Tokens  *OCTokens       `json:"tokens,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Text 拼接所有 type=text 的分片。
func (m *OCMessage) PlainText() string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 && m.Text != "" {
		return m.Text
	}
	return sb.String()
}

// Reasoning 拼接所有 type=reasoning 的分片。
func (m *OCMessage) Reasoning() string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == "reasoning" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// ToolCalls 把 agent 自己的工具调用抽出来（Phase 1 只用于日志/注释）。
func (m *OCMessage) ToolNames() []string {
	var out []string
	for _, c := range m.Content {
		if c.Type == "tool" && c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}

type OCMessagePage struct {
	Data   []OCMessage `json:"data"`
	Cursor struct {
		Previous string `json:"previous"`
		Next     string `json:"next"`
	} `json:"cursor"`
}

// ListMessages order=asc 从头取；用 limit 控制分页大小。
func (u *Upstream) ListMessages(ctx context.Context, sid, order string, limit int) ([]OCMessage, error) {
	q := url.Values{}
	if order != "" {
		q.Set("order", order)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var wrap OCMessagePage
	if err := u.do(ctx, http.MethodGet, "/api/session/"+sid+"/message", q, nil, &wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// WaitUntilIdle 阻塞到会话空闲。上游把它做成一次性查询（非阻塞），
// 所以这里只做一次调用，由调用方决定是否轮询。
func (u *Upstream) WaitUntilIdle(ctx context.Context, sid string) error {
	// wait 会阻塞到 agent loop 空闲，超时必须由调用方控制，不能再套短超时。
	return u.doRaw(ctx, http.MethodPost, "/api/experimental/session/"+sid+"/wait", nil,
		map[string]any{}, nil)
}
