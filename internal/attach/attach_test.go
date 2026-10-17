package attach

import (
	"context"
	"strings"
	"testing"

	"janus/internal/protocol/shared"
	"janus/internal/upstream"
)

const redPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestFromDataURI(t *testing.T) {
	fa, err := FromDataURI("data:image/png;base64," + redPNG)
	if err != nil {
		t.Fatal(err)
	}
	if fa.Name != "image.png" {
		t.Errorf("name = %q", fa.Name)
	}
	if !strings.HasPrefix(fa.URI, "data:image/png;base64,") {
		t.Errorf("uri = %q", fa.URI)
	}
}

func TestFromDataURIMissingComma(t *testing.T) {
	if _, err := FromDataURI("data:image/png;base64"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFromDataURINotBase64(t *testing.T) {
	if _, err := FromDataURI("data:image/png,rawdata"); err == nil {
		t.Fatal("expected error for non-base64 payload")
	}
}

func TestFromDataURIBadBase64(t *testing.T) {
	if _, err := FromDataURI("data:image/png;base64,!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestExtractDataURIImages(t *testing.T) {
	msgs := []shared.ChatMessage{{
		Role: "user",
		Content: shared.MessageContent{
			IsArray: true,
			Text:    "what color",
			Parts: []shared.ContentPart{
				{Type: "text", Text: "what color"},
				{Type: "image_url", ImageURL: &shared.ImageURL{URL: "data:image/png;base64," + redPNG}},
			},
		},
	}}
	files, failed := Extract(context.Background(), msgs, nil)
	if len(files) != 1 || len(failed) != 0 {
		t.Fatalf("files=%d failed=%d", len(files), len(failed))
	}
}

// 上游实测只认 data:，其余 scheme 要进 failed 让调用方降级。
func TestExtractUnsupportedScheme(t *testing.T) {
	msgs := []shared.ChatMessage{{
		Role: "user",
		Content: shared.MessageContent{
			IsArray: true,
			Parts:   []shared.ContentPart{{Type: "image_url", ImageURL: &shared.ImageURL{URL: "file:///etc/passwd"}}},
		},
	}}
	files, failed := Extract(context.Background(), msgs, nil)
	if len(files) != 0 || len(failed) != 1 {
		t.Fatalf("files=%d failed=%d", len(files), len(failed))
	}
}

func TestExtractIgnoresNonUserRoles(t *testing.T) {
	msgs := []shared.ChatMessage{{
		Role: "assistant",
		Content: shared.MessageContent{IsArray: true, Parts: []shared.ContentPart{
			{Type: "image_url", ImageURL: &shared.ImageURL{URL: "data:image/png;base64," + redPNG}},
		}},
	}}
	files, _ := Extract(context.Background(), msgs, nil)
	if len(files) != 0 {
		t.Fatalf("should only attach images from user messages, got %d", len(files))
	}
}

func TestExtractCapsCount(t *testing.T) {
	var msgs []shared.ChatMessage
	for i := 0; i < maxAttachCount+5; i++ {
		msgs = append(msgs, shared.ChatMessage{
			Role: "user",
			Content: shared.MessageContent{IsArray: true, Parts: []shared.ContentPart{
				{Type: "image_url", ImageURL: &shared.ImageURL{URL: "data:image/png;base64," + redPNG}},
			}},
		})
	}
	files, failed := Extract(context.Background(), msgs, nil)
	if len(files) != maxAttachCount {
		t.Fatalf("files = %d, want %d", len(files), maxAttachCount)
	}
	if len(failed) != 5 {
		t.Fatalf("failed = %d, want 5", len(failed))
	}
}

func TestAttachFailureNote(t *testing.T) {
	if FailureNote(nil) != "" {
		t.Error("empty failed list should produce no note")
	}
	n := FailureNote([]string{"https://example.com/a.png"})
	if !strings.Contains(n, "未能附带") || !strings.Contains(n, "example.com") {
		t.Errorf("got %q", n)
	}
}

func TestMimeFromExt(t *testing.T) {
	cases := map[string]string{
		"https://x/y/a.jpg":  "image/jpeg",
		"https://x/y/a.jpeg": "image/jpeg",
		"https://x/y/a.gif":  "image/gif",
		"https://x/y/a.webp": "image/webp",
		"https://x/y/a.png":  "image/png",
	}
	for in, want := range cases {
		if got := mimeFromExt(in); got != want {
			t.Errorf("mimeFromExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtFor(t *testing.T) {
	if extFor("image/png") != ".png" || extFor("image/jpeg") != ".jpg" {
		t.Error("extFor mismatch")
	}
	if extFor("application/octet-stream") != "" {
		t.Error("unknown mime should give empty ext")
	}
}

func TestFilterByModel(t *testing.T) {
	img := upstream.OCFileAttach{URI: "data:image/png;base64,AAAA", Name: "a.png"}
	pdf := upstream.OCFileAttach{URI: "data:application/pdf;base64,AAAA", Name: "a.pdf"}

	textOnly := &upstream.OCModel{}
	textOnly.Capabilities.Input = []string{"text"}
	kept, dropped := FilterByModel([]upstream.OCFileAttach{img, pdf}, textOnly)
	if len(kept) != 0 || len(dropped) != 2 {
		t.Fatalf("纯文本模型应丢掉全部附件: kept=%d dropped=%d", len(kept), len(dropped))
	}

	multi := &upstream.OCModel{}
	multi.Capabilities.Input = []string{"text", "image"}
	kept, dropped = FilterByModel([]upstream.OCFileAttach{img, pdf}, multi)
	if len(kept) != 1 || len(dropped) != 1 || modal(kept[0]) != "image" {
		t.Fatalf("支持 image 的模型只保留图片: kept=%+v dropped=%d", kept, len(dropped))
	}

	// 模型未知（nil）→ 全保留，不阻断
	kept, dropped = FilterByModel([]upstream.OCFileAttach{img}, nil)
	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("nil 模型应全保留: kept=%d dropped=%d", len(kept), len(dropped))
	}
}
