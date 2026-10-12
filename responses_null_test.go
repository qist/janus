package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 客户端（Trae 等）按规范解析 Responses 对象：output / tools 必须是数组，
// 可选字段为空时必须整个省略（OpenAI 官方不输出 null）。
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
	// 可选字段为空时必须省略，不能输出 null——严格解析器（实测 Trae）
	// 按 OpenAI 官方格式校验，遇到 null 会判定流异常并报"empty stream"。
	for _, key := range []string{`"previous_response_id":null`, `"instructions":null`, `"error":null`, `"usage":null`, `"incomplete_details":null`} {
		if strings.Contains(js, key) {
			t.Fatalf("空可选字段应省略而非 %s: %s", key, js)
		}
	}
}
