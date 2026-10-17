package api

import (
	"net/http"

	"janus/internal/ui"
)

// handleUI 返回内置的用量面板页面（静态资源在 internal/ui，随包 embed）。
// 页面本身免鉴权：里面不含任何敏感信息，真正的数据端点仍然要 key。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	ui.Serve(w)
}
