package api

import (
	"encoding/json"
	"testing"
)

func TestResponsesContentParts(t *testing.T) {
	text, parts := responsesContentParts(json.RawMessage(`[
	  {"type":"input_text","text":"看图"},
	  {"type":"input_image","image_url":"https://x/y.png"},
	  {"type":"input_image","image_url":{"url":"data:image/png;base64,AAAA"}}
	]`))
	if text != "看图" || len(parts) != 2 {
		t.Fatalf("text=%q parts=%+v", text, parts)
	}
	if parts[0].ImageURL == nil || parts[0].ImageURL.URL != "https://x/y.png" {
		t.Fatalf("string 形态 image_url 解析失败: %+v", parts[0])
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("object 形态 image_url 解析失败: %+v", parts[1])
	}

	// 纯字符串 content
	t2, p2 := responsesContentParts(json.RawMessage(`"hi"`))
	if t2 != "hi" || p2 != nil {
		t.Fatalf("字符串形态: %q %v", t2, p2)
	}
}
