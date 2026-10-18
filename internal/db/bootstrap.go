package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bootstrap.go —— 首次启动引导。
//
// 空库第一次打开时自动生成：
//   - 管理员账号（随机用户名 + 随机密码）
//   - 一个 admin API key（面板与 /api/v1 使用）
//
// 两者只在生成时返回一次明文，并写入 <data_dir>/bootstrap.key（0600），
// 便于机器上直接 cat 出来登录；库里只留密码哈希与 key 摘要。

// Bootstrap 是首启生成的一次性凭据。
type Bootstrap struct {
	Created  bool
	Username string
	Password string
	KeyID    string
	APIKey   string
}

// EnsureBootstrap 保证至少存在一个管理员与一个 admin key。
// 已有则返回 Created=false，不产生任何明文。
func (d *Store) EnsureBootstrap(token string) (Bootstrap, error) {
	if d == nil {
		return Bootstrap{}, ErrDisabled
	}
	var out Bootstrap

	if d.AdminCount() == 0 {
		username := "admin-" + strings.ToLower(RandomToken(3))
		password := RandomToken(15)
		if _, err := d.CreateAdmin(username, password); err != nil {
			return out, fmt.Errorf("create admin: %w", err)
		}
		out.Created = true
		out.Username = username
		out.Password = password
	}

	if !d.HasKind(KeyKindAdmin) {
		key, plain, err := d.CreateKey(KeyKindAdmin, "bootstrap", []string{"admin:*"}, nil, nil, 0)
		if err != nil {
			return out, fmt.Errorf("create admin key: %w", err)
		}
		if token = strings.TrimSpace(token); token != "" {
			// 允许运维用 JANUS_BOOTSTRAP_TOKEN 固定首启 key（仍只存摘要）。
			if err := d.replaceKeyPlain(key.ID, token); err != nil {
				return out, err
			}
			key, _ = d.GetKey(key.ID)
			plain = token
		}
		out.Created = true
		out.KeyID = key.ID
		out.APIKey = plain
	}
	return out, nil
}

// replaceKeyPlain 把某 key 的摘要换成指定明文（仅首启固定 token 用）。
func (d *Store) replaceKeyPlain(id, plain string) error {
	if d == nil {
		return ErrDisabled
	}
	prefix := plain
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return d.db.Model(&APIKey{}).Where("id = ?", id).Updates(map[string]any{
		"hash": HashKey(plain), "prefix": prefix,
	}).Error
}

// WriteBootstrapFile 把首启凭据写到 0600 文件；已有文件且无新内容时不覆盖。
func WriteBootstrapFile(path string, b Bootstrap) error {
	if path == "" || !b.Created {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("# janus bootstrap credentials (generated once)\n")
	if b.Username != "" {
		fmt.Fprintf(&sb, "JANUS_ADMIN_USER=%s\n", b.Username)
		fmt.Fprintf(&sb, "JANUS_ADMIN_PASSWORD=%s\n", b.Password)
	}
	if b.APIKey != "" {
		fmt.Fprintf(&sb, "JANUS_ADMIN_KEY=%s\n", b.APIKey)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}
