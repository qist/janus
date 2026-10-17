package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"janus/internal/config"
	"janus/internal/logx"
)

// out 非 nil 但响应体为空 → 视为错误。
func TestUpstreamEmptyBodyIsError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // 200 + 空 body
	}))
	defer stub.Close()

	up := New(config.Config{Upstream: stub.URL, Username: "o", Password: "p"}, logx.NewLogger("error"))
	var out struct {
		Data []OCModel `json:"data"`
	}
	if err := up.do(context.Background(), http.MethodGet, "/api/model", nil, nil, &out); err == nil {
		t.Fatal("空响应体应报错，而不是静默留下零值")
	}
	// out 为 nil 的调用（如 interrupt）不受影响
	if err := up.do(context.Background(), http.MethodPost, "/api/x", nil, nil, nil); err != nil {
		t.Fatalf("out=nil 不该报错: %v", err)
	}
}
