package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUATransport(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	c := &http.Client{Transport: withUserAgent(nil)}

	// 没显式设置 → 补上 janus UA
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(got, "janus/") {
		t.Fatalf("默认 UA = %q, 期望以 janus/ 开头", got)
	}

	// 显式设置 → 原样保留
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("User-Agent", "my-ua/1")
	resp2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if got != "my-ua/1" {
		t.Fatalf("显式 UA 被覆盖: %q", got)
	}
}

func TestUserAgentOverride(t *testing.T) {
	old := outboundUA
	defer func() { outboundUA = old }()

	outboundUA = "custom/9"
	if got := outboundUserAgent(); got != "custom/9" {
		t.Fatalf("outboundUserAgent = %q", got)
	}
}
