package shared

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"janus/internal/upstream"
)

// ---------- OpenAI 格式错误 ----------
//
// Chat Completions 与 Responses 共用同一种错误信封（Anthropic 有自己的），
// 所以放在协议公共层。

type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    string `json:"code,omitempty"`
}

type ErrorBody struct {
	Error Error `json:"error"`
}

// WriteError 按 OpenAI 的错误信封写出一个错误响应。
func WriteError(w http.ResponseWriter, status int, typ, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: Error{
		Message: msg,
		Type:    typ,
		Param:   nil,
		Code:    code,
	}})
}

// IsSessionGone 判断上游错误是否是"会话不存在"（OpenCode 重启/清理后常见）。
// 命中时应清掉本地对该 session 的引用，下次请求重建并重放完整历史。
func IsSessionGone(err error) bool {
	var ae *upstream.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound &&
		strings.Contains(strings.ToLower(ae.Msg), "session")
}

// MapUpstreamError 把 OpenCode 的错误翻译成 OpenAI 语义。
//
// base 是「当前实际在用的」上游地址：auto 发现模式下配置里的 URL 是空的/过时的，
// 连接错误信息必须带实时端点，否则排查时看到的是错地址。
func MapUpstreamError(err error, base string) (status int, typ, msg string) {
	if err == nil {
		return http.StatusInternalServerError, "api_error", "internal error"
	}

	var ae *upstream.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusForbidden &&
			strings.Contains(strings.ToLower(ae.Msg), "free tier"):
			// 免费档仅限官方客户端：透传原因 + 可操作提示
			return http.StatusForbidden, "api_error",
				ae.Msg + "；该免费模型仅限 OpenCode 官方客户端使用，请改用 opencode-go/* 订阅模型，或在 OpenCode 里配置自己的 provider"
		case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
			return http.StatusBadGateway, "api_error", "opencode server authentication failed"
		case ae.Status == http.StatusNotFound:
			return http.StatusBadGateway, "api_error", "opencode resource not found: " + ae.Msg
		case ae.Status == http.StatusConflict:
			return http.StatusConflict, "invalid_request_error", "opencode session busy: " + ae.Msg
		case ae.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_error", ae.Msg
		case ae.Status >= 500:
			return http.StatusBadGateway, "api_error", "opencode server error: " + ae.Msg
		default:
			return http.StatusBadGateway, "api_error", ae.Msg
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return 499, "api_error", "client disconnected"
	}
	if isConnErr(err) {
		return http.StatusServiceUnavailable, "api_error",
			"cannot reach opencode server at " + base
	}
	return http.StatusBadGateway, "api_error", err.Error()
}

func isConnErr(err error) bool {
	s := err.Error()
	for _, k := range []string{
		"connection refused", "no such host", "i/o timeout",
		"connection reset by peer", "dial tcp",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
