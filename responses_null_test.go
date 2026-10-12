package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 客户端（Trae 等）按规范解析 Responses 对象：output / tools 必须是数组，不能是 null。
func TestResponsesObjectArraysNotNull(t *testing.T) {
	var s Server
	obj := s.newResponsesResponse(ResponsesRequest{Model: "janus"}, "m", "k", time.Now())
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, key := range []string{`"output":null`, `"tools":null`} {
		if strings.Contains(js, key) {
			t.Fatalf("响应对象不应包含 %s: %s", key, js)
		}
	}
	if !strings.Contains(js, `"output":[]`) || !strings.Contains(js, `"tools":[]`) {
		t.Fatalf("output/tools 应为空数组: %s", js)
	}
}
