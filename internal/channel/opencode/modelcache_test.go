package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
	"janus/internal/upstream"
)

// ---------- 模型列表缓存健壮性 ----------

// 上游 200 + 空数组时不能把已有缓存清空。
func TestModelCacheKeepsOldOnEmptyList(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/model") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer empty.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/model") {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","providerID":"p","enabled":true}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"m1","providerID":"p"}}`))
	}))
	defer good.Close()

	cache := NewCache(time.Minute)

	upGood := upstream.New(config.Config{Upstream: good.URL, Username: "o", Password: "p"}, logx.NewLogger("error"))
	list, err := cache.Get(context.Background(), upGood, "/d", true)
	if err != nil || len(list) != 1 {
		t.Fatalf("首拉失败: %v %d", err, len(list))
	}

	// 现在上游返回空列表（超时刷新）
	upEmpty := upstream.New(config.Config{Upstream: empty.URL, Username: "o", Password: "p"}, logx.NewLogger("error"))
	got, err := cache.Get(context.Background(), upEmpty, "/d", true)
	if err != nil {
		t.Fatalf("空列表不该让整体失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("空列表覆盖了旧缓存: %+v", got)
	}
}

// 没有任何旧缓存 + 空列表 → 必须报错，不能返回空列表让模型"消失"。
func TestModelCacheEmptyListWithNoCacheIsError(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer empty.Close()

	cache := NewCache(time.Minute)
	up := upstream.New(config.Config{Upstream: empty.URL, Username: "o", Password: "p"}, logx.NewLogger("error"))
	if _, err := cache.Get(context.Background(), up, "/d", true); err == nil {
		t.Fatal("空列表且无缓存时应报错")
	}
}
