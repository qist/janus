package httpx

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP 取客户端地址。默认只看 RemoteAddr；trustProxy=true 时才信任
// X-Forwarded-For / X-Real-IP（否则客户端可以随便伪造来绕过限流）。
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			return strings.TrimSpace(rip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Bearer 取 Authorization: Bearer <token> 里的 token。
func Bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ReadAllLimit 读取至多 max 字节。
func ReadAllLimit(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

// WriteJSON 按 application/json 写出一个 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// JSONString 把值序列化成字符串（忽略错误，仅用于内部构造请求体）。
func JSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ParseCIDRList 解析逗号分隔的 IP / CIDR 列表，忽略空项与非法项。
// 裸 IP 会按 /32（IPv4）或 /128（IPv6）处理。
func ParseCIDRList(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			bits := 128
			if a.Is4() {
				bits = 32
			}
			out = append(out, netip.PrefixFrom(a, bits))
		}
	}
	return out
}
