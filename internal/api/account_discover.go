package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"janus/internal/channel/opencode"
	"janus/internal/channel/workbuddy"
	"janus/internal/db"
)

// discoverAccountModels 根据账号类型自动发现其可见模型列表。
// 返回 (models, externalID)；externalID 是上游侧账号标识（OpenCode label）。
func (s *Server) discoverAccountModels(ctx context.Context, acc db.Account, cred map[string]any) ([]string, string, error) {
	switch acc.Kind {
	case db.AccountKindOpenCode:
		return s.discoverOpenCodeModels(ctx, acc, cred)
	case db.AccountKindWorkBuddy:
		m, _, err := s.discoverWorkBuddyModels(ctx, acc, cred)
		return m, "", err
	default:
		return nil, "", fmt.Errorf("unsupported kind: %s", acc.Kind)
	}
}

// discoverOpenCodeModels 为一个 OpenCode 账号拉起临时实例，导入其凭据，
// 列出模型后销毁实例，不干扰正在跑的 managed upstream。
func (s *Server) discoverOpenCodeModels(ctx context.Context, acc db.Account, cred map[string]any) ([]string, string, error) {
	bin, err := opencode.FindBinary(s.cfg.OpencodeBin)
	if err != nil {
		return nil, "", err
	}
	// 临时数据目录，避免污染主机的 OpenCode DB。
	dataDir, err := os.MkdirTemp("", "janus-discover-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dataDir)

	// 把凭据写成 legacy auth.json（OpenCode 启动时导入到 SQLite）。
	authPath := filepath.Join(dataDir, "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		return nil, "", err
	}
	// OpenCode 的 auth.json 是 providerID -> credential 的映射。
	providerID := acc.ProviderID
	if providerID == "" {
		providerID = "opencode"
	}
	authDoc := map[string]any{providerID: cred}
	raw, _ := json.Marshal(authDoc)
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		return nil, "", err
	}

	// 找空闲端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	pass := randomPasswordLocal()
	env := os.Environ()
	// XDG_DATA_HOME 让 OpenCode 用临时数据目录
	env = append(env, "XDG_DATA_HOME="+dataDir)
	env = append(env, "OPENCODE_SERVER_PASSWORD="+pass)

	cmd := exec.Command(bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port))
	cmd.Env = env
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// 等待就绪
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", base+"/api/info", nil)
		req.SetBasicAuth("opencode", pass)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 列模型
	req, _ := http.NewRequest("GET", base+"/api/model", nil)
	req.SetBasicAuth("opencode", pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var wrap struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, "", err
	}
	out := make([]string, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		id := m.ID
		if m.ProviderID != "" && !stringsHasPrefix(id, m.ProviderID+"/") {
			id = m.ProviderID + "/" + id
		}
		out = append(out, id)
	}

	// 尽力捕获 OpenCode 侧账号 label（用于后续 auth switch），失败则留空。
	externalID := ""
	cmd2 := exec.Command(bin, "auth", "list")
	cmd2.Env = append(os.Environ(), "XDG_DATA_HOME="+dataDir)
	if raw2, err2 := cmd2.Output(); err2 == nil {
		for _, line := range strings.Split(string(raw2), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// 形如 "OpenCode Console  Personal  stored"
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				// 最后一个字段是状态（stored/...），倒数第二个是 label
				externalID = fields[len(fields)-2]
				break
			}
		}
	}
	return out, externalID, nil
}

func randomPasswordLocal() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func stringsHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// discoverWorkBuddyModels 从 WorkBuddy 账号凭据里拿模型目录。
// 现阶段复用现有 workbuddy runtime 的模型列表（单账号场景）；
// 多账号 WorkBuddy 需要 per-account runtime，留作后续。
func (s *Server) discoverWorkBuddyModels(ctx context.Context, acc db.Account, cred map[string]any) ([]string, string, error) {
	pc, ok := workbuddy.ProviderConfig()
	if !ok {
		return nil, "", fmt.Errorf("workbuddy catalog not ready")
	}
	out := make([]string, 0, len(pc.Models))
	for id := range pc.Models {
		out = append(out, "workbuddy/"+id)
	}
	return out, "", nil
}