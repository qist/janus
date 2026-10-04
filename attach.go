package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// 附件提取：把 OpenAI 的 image_url 内容转成 OpenCode 的 files 列表。
//
// 上游只接受 data: URI（实测 https:// 会返回 400 "Unsupported attachment URI"），
// 所以 http(s) URL 必须先拉下来转 base64。

const (
	maxAttachBytes = 12 << 20 // 单个附件上限，超过就放弃并降级为纯文本
	maxAttachCount = 8        // 单次 prompt 最多带几个附件
)

// ExtractAttachments 从增量消息里取出图片，返回 (files, 无法处理的 URL 列表)。
// http(s) 资源会实时下载转码；下载失败或超限的 URL 进第二个返回值，
// 由调用方决定是否降级。
func ExtractAttachments(ctx context.Context, delta []ChatMessage, hc *http.Client) ([]OCFileAttach, []string) {
	var files []OCFileAttach
	var failed []string

	for _, m := range delta {
		if m.Role != "user" {
			continue
		}
		for _, u := range m.Content.Images() {
			if len(files) >= maxAttachCount {
				failed = append(failed, u)
				continue
			}
			fa, err := toAttachment(ctx, u, hc)
			if err != nil {
				failed = append(failed, u)
				continue
			}
			files = append(files, fa)
		}
	}
	return files, failed
}

func toAttachment(ctx context.Context, raw string, hc *http.Client) (OCFileAttach, error) {
	switch {
	case strings.HasPrefix(raw, "data:"):
		return fromDataURI(raw)

	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		return fetchURL(ctx, raw, hc)

	default:
		// file:// 等本地路径：上游不认，交给调用方降级
		return OCFileAttach{}, fmt.Errorf("unsupported image uri scheme: %s", trimForErr(raw))
	}
}

// fromDataURI 解析 "data:<mime>;base64,<payload>"。
func fromDataURI(raw string) (OCFileAttach, error) {
	// base64 payload 里不会有 '?'，直接截断 header
	i := strings.Index(raw, ",")
	if i < 0 {
		return OCFileAttach{}, fmt.Errorf("malformed data uri (no comma)")
	}
	meta := raw[len("data:"):i]
	mime := strings.TrimSuffix(strings.Split(meta, ";")[0], ";")
	if !strings.HasSuffix(meta, ";base64") {
		return OCFileAttach{}, fmt.Errorf("only base64 data uris are supported")
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	payload := raw[i+1:]
	if est := len(payload) * 3 / 4; est > maxAttachBytes {
		return OCFileAttach{}, fmt.Errorf("attachment too large (~%d bytes)", est)
	}
	// 验证 base64 合法，避免把坏数据塞给上游
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		// data URL 常用 URL-safe 变体，再试一次
		if _, err2 := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "=")); err2 != nil {
			return OCFileAttach{}, fmt.Errorf("invalid base64 payload")
		}
	}
	return OCFileAttach{URI: raw, Name: "image" + extFor(mime)}, nil
}

// fetchURL 下载远程图片并转成 data URI。
func fetchURL(ctx context.Context, u string, hc *http.Client) (OCFileAttach, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return OCFileAttach{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Janus/1.0)")

	resp, err := hc.Do(req)
	if err != nil {
		return OCFileAttach{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return OCFileAttach{}, fmt.Errorf("download %s: HTTP %d", trimForErr(u), resp.StatusCode)
	}
	// Content-Length 预检，避免拉个大文件进来
	if resp.ContentLength > maxAttachBytes {
		return OCFileAttach{}, fmt.Errorf("attachment too large (%d bytes)", resp.ContentLength)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachBytes+1))
	if err != nil {
		return OCFileAttach{}, err
	}
	if len(body) > maxAttachBytes {
		return OCFileAttach{}, fmt.Errorf("attachment too large (> %d bytes)", maxAttachBytes)
	}
	if len(body) == 0 {
		return OCFileAttach{}, fmt.Errorf("empty attachment body")
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" || mime == "application/octet-stream" {
		mime = mimeFromExt(u)
	}
	mime = strings.TrimSpace(strings.Split(mime, ";")[0])

	name := path.Base(mustPath(u))
	if name == "" || name == "." || name == "/" {
		name = "image" + extFor(mime)
	}

	return OCFileAttach{
		URI:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(body),
		Name: name,
	}, nil
}

func mustPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

func mimeFromExt(raw string) string {
	e := strings.ToLower(path.Ext(mustPath(raw)))
	switch e {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	default:
		return "image/png"
	}
}

func extFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}

func trimForErr(s string) string {
	if len(s) > 96 {
		return s[:96] + "…"
	}
	return s
}

// attachFailureNote 把没能附带的 URL 以文字形式补进 prompt，
// 模型至少知道有张图存在，而不是误以为用户没发图。
func attachFailureNote(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[注意] 以下图片未能附带，无法查看：\n")
	for _, u := range failed {
		sb.WriteString("- ")
		sb.WriteString(trimForErr(u))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
