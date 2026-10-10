package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

// reWorkingDir 匹配 Trae 环境提醒里的 "Primary working directory: <path>"（权威项目根）。
var reWorkingDir = regexp.MustCompile(`(?i)primary working directory:\s*([^\r\n]+)`)

// reCopilotWorkspace 匹配 Copilot Chat 的 "following folders: - /path"。
var reCopilotWorkspace = regexp.MustCompile(`(?i)following folders:\s*-\s*([^\s]+)`)

// reAbsPath 匹配消息里的绝对路径（Unix 或 Windows）。
var reAbsPath = regexp.MustCompile(`(?:^|[\s"'=(,\[:])((?:/[A-Za-z0-9_.@+\-]+)+|[A-Za-z]:\\[^\s"']+)`)

// systemPathPrefixes 归一项目时排除的系统 / 工具自身目录，避免抽错。
var systemPathPrefixes = []string{
	"/usr", "/etc", "/var", "/proc", "/sys", "/dev", "/run", "/boot", "/lib", "/sbin", "/bin",
	"/tmp", "/root/.trae", "/root/.cache", "/root/.config", "/root/.local", "/root/.vscode", "/root/.vscode-server",
}

func isSystemPath(p string) bool {
	for _, pre := range systemPathPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// extractProjectRoot 抽「项目根目录」：
//  1. Trae 环境提醒 "Primary working directory: <path>"（最权威）
//  2. CodeBuddy "Workspace Folder: <path>"
//  3. 兜底：消息里出现最多的 3 段路径前缀（排除系统/Trae 自身目录）
//
// 用于 Trae 这类「不给项目 header」的客户端：让 janus 用客户端自己的项目当会话
// 目录，从而隔离不同项目。
func extractProjectRoot(msgs []ChatMessage) string {
	for _, m := range msgs {
		s := m.Content.Text
		if s == "" {
			continue
		}
		if mm := reWorkingDir.FindStringSubmatch(s); mm != nil {
			if v := normalizeDir(strings.TrimSpace(mm[1])); v != "" {
				return v
			}
		}
		if mm := reCopilotWorkspace.FindStringSubmatch(s); mm != nil {
			if v := normalizeDir(strings.TrimSpace(mm[1])); v != "" {
				return v
			}
		}
	}
	if wf := extractWorkspaceFolder(msgs); wf != "" {
		return normalizeDir(wf)
	}
	counts := map[string]int{}
	for _, m := range msgs {
		s := m.Content.Text
		if s == "" {
			continue
		}
		for _, mm := range reAbsPath.FindAllStringSubmatch(s, -1) {
			p := normalizeDir(mm[1])
			if isSystemPath(p) {
				continue
			}
			parts := strings.Split(p, "/")
			n := 3
			if len(parts) < n {
				n = len(parts)
			}
			root := strings.Join(parts[:n], "/")
			if root != "" && root != "/" {
				counts[root]++
			}
		}
	}
	best, bestN := "", 1
	for p, n := range counts {
		if n > bestN {
			best, bestN = p, n
		}
	}
	return best
}

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
	if dir == "" {
		// 客户端没给目录 header（如 Trae/Copilot）：从消息里抽项目根。
		dir = normalizeDir(extractProjectRoot(msgs))
	}
	if dir != "" && len(s.cfg.ProjectMap) > 0 {
		if p, ok := s.cfg.ProjectMap[clientDir]; ok {
			return p
		}
		if p, ok := s.cfg.ProjectMap[dir]; ok {
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
	if dir != "" {
		if b := baseName(dir); b != "" {
			return b
		}
	}
	return "default"
}

// scopeKeyPrefix 是 scope（IDE+项目）会话键的前缀，与 x:/f: 键空间隔离。
const scopeKeyPrefix = "s:"

// scopeKey 由 IDE + 项目算出 scope key（`s:<hex>`）。
func scopeKey(ide, project string) string {
	h := sha256.Sum256([]byte(strings.ToLower(ide) + "\x00" + strings.ToLower(project)))
	return scopeKeyPrefix + hex.EncodeToString(h[:8])
}

// scopeOf 返回 (scopeKey, 可读 scope)。
func (s *Server) scopeOf(r *http.Request, clientDir string, msgs []ChatMessage) (string, string) {
	ide := normalizeIDE(r.UserAgent())
	project := s.projectIdentity(clientDir, msgs)
	return scopeKey(ide, project), ide + "/" + project
}

// sessionDir 决定上游 OpenCode 会话的工作目录。Chat / Anthropic Messages /
// Responses 三个接口共用同一套逻辑，保证远程 + mode B 行为一致：
//
//	客户端 header（仅当目录在 janus 主机上真实存在）
//	  > 从消息抽的项目根（同样要求真实存在）
//	  > per-scope 中性工作目录（BRIDGE_WORKSPACES_DIR/<scope>）
//
// 为什么要存在性校验：Trae/Copilot/Claude Code 的项目路径是「客户端侧」的；远程 + mode B 时
// janus 主机上未必有该目录（Windows 客户端路径在 Linux 上必不存在），上游对不存在的目录
// 注册 MCP 会 500，整轮直接失败。此时用 per-scope 中性目录后，agent 的工作区是该项目
// 专属的空目录，不会误认成别的项目，也不会回落到部署目录；隔离仍由 scope（key）保证。
func (s *Server) sessionDir(headerDir string, msgs []ChatMessage, scopeKey string) string {
	if d := strings.TrimSpace(headerDir); d != "" {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		// header 是客户端侧路径（janus 主机上不存在）：不直接使用，继续往下落。
	}
	if p := extractProjectRoot(msgs); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	name := strings.Trim(strings.TrimPrefix(scopeKey, scopeKeyPrefix), "/")
	if name == "" {
		name = "default"
	}
	d := filepath.Join(s.cfg.WorkspacesDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return s.cfg.Directory
	}
	return d
}

// ---------- 出向路径改写（工作区 → 客户端项目路径） ----------

// pathRewriter 把出向文本里的「janus 主机工作区路径」改写成「客户端项目路径」。
//
// 远程 + mode B 时客户端项目路径在 janus 主机上不存在，会话目录落到
// BRIDGE_WORKSPACES_DIR/<scope>。agent 在回答、工具输出里引用自己 cwd 下的文件时，
// 给出的是 janus 主机路径 —— 客户端 IDE 打不开，也不应去读远端主机的目录。
// 改写后客户端看到的是自己声明的项目路径。
type pathRewriter struct {
	from string // janus 主机上的会话工作区前缀（无尾斜杠）
	to   string // 客户端侧项目路径（保持客户端声明的原样）
	pend string // 可能是 from 真前缀的尾部：先扣住，等下一个 delta 拼全再放行
}

// newPathRewriter 规则不成立（无路径可改写、两侧相同）时返回 nil。
func newPathRewriter(from, to string) *pathRewriter {
	from = strings.TrimRight(strings.TrimSpace(from), "/")
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return nil
	}
	return &pathRewriter{from: from, to: to}
}

// rewrite 改写一段增量文本，返回可立即发出的部分。
//
// 被 delta 切断的路径前缀先扣住（pend ≤ len(from)-1 字节），避免改写漏掉
// 跨块出现的路径；调用方必须在终态时调 flush() 补发扣住的尾部。
func (rw *pathRewriter) rewrite(s string) string {
	if rw == nil || s == "" {
		return s
	}
	if rw.pend != "" {
		s = rw.pend + s
		rw.pend = ""
	}
	// 完整匹配逐个替换（路径不含可转义字符，纯字符串替换即可）
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, rw.from)
		if i < 0 {
			break
		}
		b.WriteString(rest[:i])
		b.WriteString(rw.to)
		rest = rest[i+len(rw.from):]
	}
	// 尾部若是 from 的真前缀（被 delta 切断），扣住不发
	hold := 0
	max := len(rw.from) - 1
	if max > len(rest) {
		max = len(rest)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(rw.from, rest[len(rest)-n:]) {
			hold = n
			break
		}
	}
	rw.pend = rest[len(rest)-hold:]
	if b.Len() == 0 && hold == 0 {
		return s
	}
	return b.String() + rest[:len(rest)-hold]
}

// flush 终态时补发扣住的尾部（幂等）。
func (rw *pathRewriter) flush() string {
	if rw == nil || rw.pend == "" {
		return ""
	}
	out := rw.pend
	rw.pend = ""
	return out
}

// setPathRewrite 计算并记录本会话的出向路径改写规则（写在 conv 上，只在持
// conv.mu 的请求路径上访问）。仅当会话目录落在 BRIDGE_WORKSPACES_DIR 下（远程
// mode B 的中性工作区兜底）且客户端声明过项目路径时才有规则；同机部署零开销。
// scope 共享会话可能被多台设备共用，每轮请求按当前请求者重算。
func (s *Server) setPathRewrite(conv *Conversation, dir, headerDir string, msgs []ChatMessage) {
	root := filepath.Clean(s.cfg.WorkspacesDir)
	if dir == "" || dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		return
	}
	to := strings.TrimSpace(headerDir)
	if to == "" {
		to = extractProjectRoot(msgs)
	}
	if to == "" {
		return
	}
	conv.rewriteFrom, conv.rewriteTo = dir, to
}

// jsonStringValue 把字符串编码成 JSON 字符串值的转义内容（不含两侧引号），
// 用于把任意路径安全地嵌进 JSON 文本（如客户端工具的调用参数）。
func jsonStringValue(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		b = b[1 : len(b)-1]
	}
	return string(b)
}

// pathMappingNote 远程 mode B 下告知 agent「工具在客户端设备执行、该用哪条路径」，
// 从源头避免它把工作区路径传进工具参数。只在会话首轮随 prompt 注入一次
// （随上游会话上下文留存），不逐轮重复，省 token；与出向/参数改写互为兜底。
func (s *Server) pathMappingNote(conv *Conversation, first bool) string {
	if !first || conv.rewriteFrom == "" || conv.rewriteTo == "" {
		return ""
	}
	return "\n\n[环境说明] 本次会话的客户端工具在用户的设备上执行，不在本机。" +
		"你的工作目录 " + conv.rewriteFrom + " 只是桥侧的中性工作区，用户设备上并不存在，" +
		"它与用户的项目目录 " + conv.rewriteTo + " 相互对应。" +
		"调用工具读写文件/执行命令时，路径参数一律使用用户项目目录 " + conv.rewriteTo +
		" 下的路径（或对话里出现的用户侧路径），不要把你的工作目录路径传给工具。"
}
