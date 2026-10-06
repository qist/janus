package main

import (
	"net/http"
	"runtime"
)

// useragent.go —— 出站请求的 User-Agent。
//
// Go 的 http.Client 默认发 `Go-http-client/1.1`，不少网关 / CDN / 风控会把它当成
// 脚本流量直接拦掉（403/429）。这里统一换成一个标识清晰、可追溯的 UA：
//
//	janus/<version> (<os>/<arch>; +https://github.com/qist/janus)
//
// 版本来自 -ldflags 注入（见 Makefile）；未注入时是 dev。
// 可用 BRIDGE_USER_AGENT 覆盖（例如要伪装成浏览器 UA）。

// outboundUA 是当前生效的默认 User-Agent，LoadConfig 里可被 BRIDGE_USER_AGENT 覆盖。
var outboundUA = "janus/" + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + "; +https://github.com/qist/janus)"

// outboundUserAgent 返回默认 User-Agent。
func outboundUserAgent() string { return outboundUA }

// uaTransport 在出站请求没显式设置 User-Agent 时补上默认 UA；
// 已显式设置的一律保留（例如附件下载故意用的浏览器式 UA）。
type uaTransport struct{ base http.RoundTripper }

func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Header.Get("User-Agent") != "" {
		return base.RoundTrip(req)
	}
	// clone 一份再写头，避免改动可能被复用/重试的原始请求。
	r2 := req.Clone(req.Context())
	r2.Header.Set("User-Agent", outboundUserAgent())
	return base.RoundTrip(r2)
}

// withUserAgent 把任意 Transport（nil 也行）包成"自动补 UA"的 Transport。
func withUserAgent(base http.RoundTripper) http.RoundTripper {
	return uaTransport{base: base}
}
