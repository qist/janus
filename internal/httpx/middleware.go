package httpx

import (
	"net/http"
	"strings"
	"time"

	"janus/internal/logx"
	"janus/internal/metricx"
)

// RequestID 给每个响应带上 X-Request-Id，方便和上游日志对账。
// 客户端给了就沿用（便于端到端串联 trace），没给就生成一个。
func RequestID(newID func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

// AccessLog 记录每个进入 mux 的请求。耗时对流式端点是"直到流结束"，
// 正好用来定位客户端提前断流（表现为 llm response empty stream）。
// 静态/探活端点（healthz、metrics、UI）噪音大，跳过。
func AccessLog(log *logx.Logger, trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LogPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &Recorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(rec, r)
		ua := r.Header.Get("User-Agent")
		if len(ua) > 60 {
			ua = ua[:60]
		}
		log.Infof("http: %s %s status=%d dur=%s bytes=%d ip=%q ua=%q",
			r.Method, r.URL.Path, rec.Status,
			time.Since(start).Round(time.Millisecond), rec.N, ClientIP(r, trustProxy), ua)
	})
}

// Metrics 把每个请求的状态码与耗时记进 Prometheus 指标。
// /metrics 自身不计数，避免抓取把统计搅乱。
func Metrics(m *metricx.Metrics, next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &Recorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.ObserveRequest(metricx.Endpoint(r.URL.Path), rec.Status, time.Since(start))
		if rec.Status >= 500 {
			m.IncUpstreamErr()
		}
	})
}

// CORSOptions 是跨源策略的入参。
//
//	AllowOrigin 为空 → 不发任何 CORS 头（= 拒绝跨源浏览器访问）
//	AllowOrigin=*    → Allow-Origin: *（不带 credentials）
//	AllowOrigin=a,b  → 命中列表的 Origin 才放行（带 credentials）
//
// IsPublic 标出无需鉴权的静态路径：这些路径不受 AllowOrigin 限制
// （页面本身不含敏感数据，鉴权仍在数据端点）。DenyOrigin 用于把被拒的
// 预检写成该协议自己的错误信封。
type CORSOptions struct {
	AllowOrigin string
	IsPublic    func(path string) bool
	DenyOrigin  func(w http.ResponseWriter, r *http.Request)
}

// CORS 按 CORSOptions 收紧跨源访问。
//
// 默认**不发任何 CORS 头**（= 拒绝跨源浏览器访问）。之前的默认是"回显请求方
// Origin 且带 credentials"，等于对任意站点开放，是审计里的一条安全缺口。
// 桌面客户端（Trae/CodeBuddy）不受 CORS 约束，所以收紧默认值不影响它们。
func CORS(o CORSOptions, next http.Handler) http.Handler {
	allowAll := o.AllowOrigin == "*"
	explicit := !allowAll && strings.TrimSpace(o.AllowOrigin) != ""

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 不限定方法，否则 OPTIONS 预检会被下面的 origin 校验拦掉。
		public := o.IsPublic != nil && o.IsPublic(r.URL.Path)

		allowed := false
		if public {
			allowed = true
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
		} else if origin != "" {
			switch {
			case allowAll:
				allowed = true
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case explicit && originAllowed(origin, o.AllowOrigin):
				allowed = true
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		if allowed {
			if !public {
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, X-Session-ID, X-OpenCode-Session, X-OpenCode-Agent, X-OpenCode-Directory, X-Api-Key")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, X-RateLimit-Limit-Requests, X-RateLimit-Remaining-Requests, X-RateLimit-Reset-Requests, X-Janus-Session, X-Janus-Conversation")
		}

		if r.Method == http.MethodOptions {
			if origin != "" && !allowed {
				// 明确的预检失败比"静默无头"更好排查
				if o.DenyOrigin != nil {
					o.DenyOrigin(w, r)
				} else {
					http.Error(w, "origin not allowed", http.StatusForbidden)
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(origin, allowed string) bool {
	for _, a := range strings.Split(allowed, ",") {
		if strings.TrimSpace(a) == origin {
			return true
		}
	}
	return false
}
