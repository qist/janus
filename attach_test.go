package main

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

func TestFilterAttachmentsByModel(t *testing.T) {
	img := OCFileAttach{URI: "data:image/png;base64,AAAA", Name: "a.png"}
	pdf := OCFileAttach{URI: "data:application/pdf;base64,AAAA", Name: "a.pdf"}

	textOnly := &OCModel{}
	textOnly.Capabilities.Input = []string{"text"}
	kept, dropped := filterAttachmentsByModel([]OCFileAttach{img, pdf}, textOnly)
	if len(kept) != 0 || len(dropped) != 2 {
		t.Fatalf("纯文本模型应丢掉全部附件: kept=%d dropped=%d", len(kept), len(dropped))
	}

	multi := &OCModel{}
	multi.Capabilities.Input = []string{"text", "image"}
	kept, dropped = filterAttachmentsByModel([]OCFileAttach{img, pdf}, multi)
	if len(kept) != 1 || len(dropped) != 1 || attachModal(kept[0]) != "image" {
		t.Fatalf("支持 image 的模型只保留图片: kept=%+v dropped=%d", kept, len(dropped))
	}

	// 模型未知（nil）→ 全保留，不阻断
	kept, dropped = filterAttachmentsByModel([]OCFileAttach{img}, nil)
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("nil 模型应全保留: kept=%d dropped=%d", len(kept), len(dropped))
	}
}
