package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

// ---------- scope：没有会话 id 时的「上下文归属」 ----------
//
// scope = IDE + 项目。它**不是会话本身**，而是「同一 IDE、同一项目」的上下文归属。
// 没有客户端会话 id 时，Janus 明确退化到这一级（不做隐式猜测）。
// 未来拿到真实 session id，用 session 即可，scope 作为其上级命名空间：
//
//	conversation_id  ??  client_instance+project  ??  scope(IDE+project)
//
// 注意：远程 + mode B 时 janus 没有客户端文件，所以**不用 git remote** 归一项目。

var reWorkspaceFolder = regexp.MustCompile(`(?i)workspace folder:\s*([^\r\n]+)`)

// normalizeIDE 从 User-Agent 归一出 IDE 产品名（**剥掉版本**，避免升级产生新 scope）。
func normalizeIDE(ua string) string {
	u := strings.ToLower(strings.TrimSpace(ua))
	switch {
	case u == "":
		return "unknown"
	case strings.Contains(u, "codebuddy"):
		return "codebuddy"
	case strings.Contains(u, "hertz"), strings.Contains(u, "trae"):
		return "trae"
	}
	if i := strings.IndexAny(u, " \t"); i >= 0 {
		u = u[:i]
	}
	if i := strings.Index(u, "/"); i > 0 {
		u = u[:i]
	}
	return u
}

// normalizeDir 归一目录写法（Windows 反斜杠、结尾斜杠），便于 map 匹配。
func normalizeDir(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.TrimRight(p, "/")
}

func baseName(p string) string {
	p = normalizeDir(p)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// extractWorkspaceFolder 从消息前言里抽 "Workspace Folder: <x>"（CodeBuddy 会带）。
func extractWorkspaceFolder(msgs []ChatMessage) string {
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "system" {
			continue
		}
		s := m.Content.Text
		if s == "" {
			continue
		}
		if mm := reWorkspaceFolder.FindStringSubmatch(s); mm != nil {
			if v := strings.TrimSpace(mm[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// projectIdentity 归一「项目身份」。优先级：
//
//	BRIDGE_PROJECT > BRIDGE_PROJECT_MAP（客户端目录）> Workspace Folder（前言）> 目录 basename > "default"
//
// 远程 + mode B 时 janus 没有客户端文件，故不用 git remote。
func (s *Server) projectIdentity(clientDir string, msgs []ChatMessage) string {
	if p := strings.TrimSpace(s.cfg.Project); p != "" {
		return p
	}
	dir := normalizeDir(clientDir)
	if dir != "" && len(s.cfg.ProjectMap) > 0 {
		if p, ok := s.cfg.ProjectMap[clientDir]; ok {
			return p
		}
		bestLen, best := -1, ""
		for k, v := range s.cfg.ProjectMap {
			nk := normalizeDir(k)
			if nk == "" {
				continue
			}
			if dir == nk || strings.HasPrefix(dir, nk+"/") {
				if len(nk) > bestLen {
					bestLen, best = len(nk), v
				}
			}
		}
		if best != "" {
			return best
		}
	}
	if wf := extractWorkspaceFolder(msgs); wf != "" {
		return wf
	}
	if dir != "" {
		if b := baseName(dir); b != "" {
			return b
		}
	}
	return "default"
}

// scopeKey 由 IDE + 项目算出 scope key（`s:<hex>`，与 x:/f: 键空间隔离）。
func scopeKey(ide, project string) string {
	h := sha256.Sum256([]byte(strings.ToLower(ide) + "\x00" + strings.ToLower(project)))
	return "s:" + hex.EncodeToString(h[:8])
}

// scopeOf 返回 (scopeKey, 可读 scope)。
func (s *Server) scopeOf(r *http.Request, clientDir string, msgs []ChatMessage) (string, string) {
	ide := normalizeIDE(r.UserAgent())
	project := s.projectIdentity(clientDir, msgs)
	return scopeKey(ide, project), ide + "/" + project
}
