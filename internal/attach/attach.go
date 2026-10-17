// Package attach 把客户端消息里的图片附件提取成上游可接收的 files 列表。
//
// 上游只接受 data: URI（实测 https:// 会返回 400 "Unsupported attachment URI"），
// 所以 http(s) URL 必须先下载再转 base64。本包与 HTTP 端点无关，三个对外
// API 的 handler 都调用它。
package attach

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

	"janus/internal/protocol/shared"
	"janus/internal/upstream"
	"janus/internal/useragent"
	"janus/internal/version"
)

const (
	maxAttachBytes = 12 << 20 // 单个附件上限，超过就放弃并降级为纯文本
	maxAttachCount = 8        // 单次 prompt 最多带几个附件
)

// Extract 从增量消息里取出图片，返回 (files, 无法处理的 URL 列表)。
// http(s) 资源会实时下载转码；下载失败或超限的 URL 进第二个返回值，
// 由调用方决定是否降级。
func Extract(ctx context.Context, delta []shared.ChatMessage, hc *http.Client) ([]upstream.OCFileAttach, []string) {
	var files []upstream.OCFileAttach
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

func toAttachment(ctx context.Context, raw string, hc *http.Client) (upstream.OCFileAttach, error) {
	switch {
	case strings.HasPrefix(raw, "data:"):
		return FromDataURI(raw)

	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		return fetchURL(ctx, raw, hc)

	default:
		// file:// 等本地路径：上游不认，交给调用方降级
		return upstream.OCFileAttach{}, fmt.Errorf("unsupported image uri scheme: %s", trimForErr(raw))
	}
}

// FromDataURI 解析 "data:<mime>;base64,<payload>"。
func FromDataURI(raw string) (upstream.OCFileAttach, error) {
	// base64 payload 里不会有 '?'，直接截断 header
	i := strings.Index(raw, ",")
	if i < 0 {
		return upstream.OCFileAttach{}, fmt.Errorf("malformed data uri (no comma)")
	}
	meta := raw[len("data:"):i]
	mime := strings.TrimSuffix(strings.Split(meta, ";")[0], ";")
	if !strings.HasSuffix(meta, ";base64") {
		return upstream.OCFileAttach{}, fmt.Errorf("only base64 data uris are supported")
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	payload := raw[i+1:]
	if est := len(payload) * 3 / 4; est > maxAttachBytes {
		return upstream.OCFileAttach{}, fmt.Errorf("attachment too large (~%d bytes)", est)
	}
	// 验证 base64 合法，避免把坏数据塞给上游
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		// data URL 常用 URL-safe 变体，再试一次
		if _, err2 := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "=")); err2 != nil {
			return upstream.OCFileAttach{}, fmt.Errorf("invalid base64 payload")
		}
	}
	return upstream.OCFileAttach{URI: raw, Name: "image" + extFor(mime)}, nil
}

// fetchURL 下载远程图片并转成 data URI。
func fetchURL(ctx context.Context, u string, hc *http.Client) (upstream.OCFileAttach, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second, Transport: useragent.Wrap(nil)}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return upstream.OCFileAttach{}, err
	}
	// 图片来自用户给的任意 URL，用浏览器式 UA 兼容只放行浏览器的站点。
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; janus/"+version.Version+")")

	resp, err := hc.Do(req)
	if err != nil {
		return upstream.OCFileAttach{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return upstream.OCFileAttach{}, fmt.Errorf("download %s: HTTP %d", trimForErr(u), resp.StatusCode)
	}
	// Content-Length 预检，避免拉个大文件进来
	if resp.ContentLength > maxAttachBytes {
		return upstream.OCFileAttach{}, fmt.Errorf("attachment too large (%d bytes)", resp.ContentLength)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachBytes+1))
	if err != nil {
		return upstream.OCFileAttach{}, err
	}
	if len(body) > maxAttachBytes {
		return upstream.OCFileAttach{}, fmt.Errorf("attachment too large (> %d bytes)", maxAttachBytes)
	}
	if len(body) == 0 {
		return upstream.OCFileAttach{}, fmt.Errorf("empty attachment body")
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

	return upstream.OCFileAttach{
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

// FailureNote 把没能附带的 URL 以文字形式补进 prompt，
// 模型至少知道有张图存在，而不是误以为用户没发图。
func FailureNote(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[注意] 以下附件未能附带，无法查看：\n")
	for _, u := range failed {
		sb.WriteString("- ")
		sb.WriteString(trimForErr(u))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// modal 返回附件需要的输入模态（image / pdf / audio / video），
// 无法判断时返回 ""（视为通用，不做限制）。
func modal(f upstream.OCFileAttach) string {
	u := strings.ToLower(f.URI)
	if strings.HasPrefix(u, "data:") {
		i := strings.Index(u, ";")
		if i < 0 {
			i = strings.Index(u, ",")
		}
		if i > len("data:") {
			mime := u[len("data:"):i]
			if j := strings.Index(mime, "/"); j > 0 {
				return mime[:j]
			}
		}
		return ""
	}
	switch strings.ToLower(path.Ext(f.Name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
		return "image"
	case ".pdf":
		return "pdf"
	}
	return ""
}

// modelSupportsModal 判断模型 capabilities.input 是否包含该模态。
// m 为 nil 或模态未知时返回 true（不阻断）。
func modelSupportsModal(m *upstream.OCModel, modal string) bool {
	if m == nil || modal == "" {
		return true
	}
	for _, in := range m.Capabilities.Input {
		if strings.EqualFold(in, modal) {
			return true
		}
	}
	return false
}

// FilterByModel 丢弃模型不支持的附件（例如纯文本模型收到图片）。
// 返回 (保留, 丢弃)。正是"模型支持就接收，不支持就不硬塞"。
func FilterByModel(files []upstream.OCFileAttach, m *upstream.OCModel) (kept, dropped []upstream.OCFileAttach) {
	for _, f := range files {
		if modelSupportsModal(m, modal(f)) {
			kept = append(kept, f)
		} else {
			dropped = append(dropped, f)
		}
	}
	return kept, dropped
}
