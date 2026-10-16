package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// forensics.go —— 楔死取证：把每轮以 tool_calls 收尾的响应原始字节落盘。
//
// 背景：Trae 类客户端偶发收到 tool_call 后不执行也不回传（2026-10-15/16 五次
// 同签名楔死：janus 交付完成、accesslog 显示客户端零请求），纯服务端日志无法
// 定位客户端解析卡死的触发点。开启取证（BRIDGE_WEDGE_FORENSICS）后，每轮
// tool_calls 响应的原始 SSE 字节落盘；楔死发生时（工具结果超时）按 callID 找回
// 对应的落盘文件，与成功轮逐字节对比即可定位触发字段/分块形态。

// Forensics 取证落盘器。nil = 未启用（所有方法 nil 安全）。
type Forensics struct {
	dir  string // 根目录（每会话一个子目录）
	keep int    // 每会话保留份数，超出按文件名时间戳淘汰最旧
	log  *Logger
}

func newForensics(root string, keep int, log *Logger) *Forensics {
	if root == "" || keep <= 0 {
		return nil
	}
	return &Forensics{dir: root, keep: keep, log: log}
}

// save 落盘一轮 tool_calls 响应；返回文件路径（失败返回空串，只记 Debug 不影响主链路）。
// 文件名 <unix-ms>-<首个 callID>.sse，内容为原始 SSE 字节（含 finish/usage/[DONE]）。
func (f *Forensics) save(convKey, sid string, callIDs []string, data []byte) string {
	if f == nil || len(data) == 0 || len(callIDs) == 0 {
		return ""
	}
	dir := filepath.Join(f.dir, sanitizeFilePart(convKey))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.log.Debugf("forensics mkdir: %v", err)
		return ""
	}
	name := fmt.Sprintf("%d-%s.sse", time.Now().UnixMilli(), sanitizeFilePart(callIDs[0]))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.log.Debugf("forensics write: %v", err)
		return ""
	}
	f.prune(dir)
	f.log.Infof("forensics: saved tool_calls response dump (%d bytes, calls=%d) file=%s sid=%s",
		len(data), len(callIDs), path, sid)
	return path
}

// prune 每会话目录只保留最近 keep 份。
func (f *Forensics) prune(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []os.DirEntry
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sse") {
			files = append(files, e)
		}
	}
	if len(files) <= f.keep {
		return
	}
	// 文件名自带毫秒时间戳，按名字排序即按时间排序。
	for i := 0; i < len(files)-f.keep; i++ {
		_ = os.Remove(filepath.Join(dir, files[i].Name()))
	}
}

// trace 楔死发生时按 callID 找回对应的响应落盘（内容包含该 callID 的文件）。
func (f *Forensics) trace(callID string) {
	if f == nil || callID == "" {
		return
	}
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(f.dir, e.Name())
		files, _ := os.ReadDir(dir)
		for _, file := range files {
			p := filepath.Join(dir, file.Name())
			data, err := os.ReadFile(p)
			if err == nil && strings.Contains(string(data), callID) {
				f.log.Warnf("forensics: wedged call %s — response dump: %s", callID, p)
				return
			}
		}
	}
	f.log.Warnf("forensics: wedged call %s — no dump matched (dir=%s)", callID, f.dir)
}

// sanitizeFilePart 把会话 key / callID 清洗成安全的文件名片段。
func sanitizeFilePart(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
