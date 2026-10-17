// Package db 实现基于 SQLite（github.com/qist/sqlite + gorm）的持久化层。
//
// 目前持久化三类数据，让 Responses 的 previous_response_id 能跨进程重启续链：
//   - responses：响应本身（id → convKey + payload + 过期时间）
//   - conversations：会话键 → 上游 OpenCode sessionID（+ model/agent/dir）
//   - settings：运行时可改的键值设置（如 default_model）
//
// 未配置 BRIDGE_DB（或设为 memory/off）时整层为 nil，退回纯内存行为。
package db

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qist/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type response struct {
	ID        string `gorm:"primaryKey;size:80"`
	ConvKey   string `gorm:"index;size:160"`
	Directory string `gorm:"size:512"`
	ExpiresAt int64  `gorm:"index"`
	CreatedAt int64
	Payload   []byte
}

// Conversation 是一行「会话键 → 上游 sessionID」映射。
type Conversation struct {
	Key        string `gorm:"primaryKey;size:160"`
	SessionID  string `gorm:"size:160"`
	ProviderID string `gorm:"size:80"`
	ModelID    string `gorm:"size:80"`
	Variant    string `gorm:"size:40"`
	Agent      string `gorm:"size:80"`
	Directory  string `gorm:"size:512"`
	// History 是 Chat 的规范化历史快照（[]ChatMessage 的 JSON），
	// 用于跨进程重启后仍能做历史前缀匹配、只发增量。
	History   []byte
	UpdatedAt int64 `gorm:"index"`
}

// setting 是运行时可改的键值设置（如 default_model），
// 让 web 里选的默认模型不写 env 也能持久化、立即生效。
type setting struct {
	Key       string `gorm:"primaryKey;size:80"`
	Value     string `gorm:"size:512"`
	UpdatedAt int64
}

// MemoryDSN 表示"不用文件、纯内存"。
func MemoryDSN(path string) bool {
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "", "memory", "off", "none", ":memory:":
		return true
	}
	return false
}

// Store 是持久化层句柄。
type Store struct {
	db *gorm.DB
}

// Open 打开（必要时创建）SQLite 库并建表。不负责决定是否启用（调用方判断）。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	gdb, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if err := gdb.AutoMigrate(&response{}, &Conversation{}, &setting{}, &Account{}, &AccountModel{}); err != nil {
		return nil, err
	}
	return &Store{db: gdb}, nil
}

// Close 关闭底层 SQL 连接。
func (d *Store) Close() {
	if d == nil || d.db == nil {
		return
	}
	if sqlDB, err := d.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// ---------- responses ----------

func (d *Store) PutResponse(id, convKey, dir string, expiresAt int64, payload []byte) {
	if d == nil {
		return
	}
	row := response{ID: id, ConvKey: convKey, Directory: dir, ExpiresAt: expiresAt, CreatedAt: time.Now().Unix(), Payload: payload}
	// Upsert：同 id 覆盖
	d.db.Save(&row)
}

func (d *Store) GetResponse(id string) (payload []byte, convKey, dir string, expiresAt int64, ok bool) {
	if d == nil {
		return nil, "", "", 0, false
	}
	var row response
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return nil, "", "", 0, false
	}
	return row.Payload, row.ConvKey, row.Directory, row.ExpiresAt, true
}

func (d *Store) DeleteResponse(id string) bool {
	if d == nil {
		return false
	}
	res := d.db.Delete(&response{}, "id = ?", id)
	return res.Error == nil && res.RowsAffected > 0
}

func (d *Store) GCResponses(now int64) {
	if d == nil {
		return
	}
	d.db.Delete(&response{}, "expires_at > 0 AND expires_at < ?", now)
}

// ---------- conversations ----------

func (d *Store) SaveConv(row Conversation) {
	if d == nil {
		return
	}
	row.UpdatedAt = time.Now().Unix()
	d.db.Save(&row)
}

func (d *Store) LoadConv(key string) (Conversation, bool) {
	if d == nil {
		return Conversation{}, false
	}
	var row Conversation
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return Conversation{}, false
	}
	return row, true
}

func (d *Store) DeleteConv(key string) {
	if d == nil || key == "" {
		return
	}
	d.db.Delete(&Conversation{}, "key = ?", key)
}

// ClearSession 把引用该上游 session 的会话行的 session_id 清空（保留历史快照）。
// janitor 删掉上游会话后必须调用：否则库里残留死 sessionID，下次请求拿它去打
// 会得到 502 "Session not found"。清空后下次请求会重建会话（并重放完整历史）。
func (d *Store) ClearSession(sid string) {
	if d == nil || sid == "" {
		return
	}
	d.db.Model(&Conversation{}).Where("session_id = ?", sid).Updates(map[string]any{
		"session_id": "", "provider_id": "", "model_id": "", "variant": "",
	})
}

func (d *Store) GCConvs(before int64) {
	if d == nil {
		return
	}
	d.db.Delete(&Conversation{}, "updated_at > 0 AND updated_at < ?", before)
}

// ---------- settings ----------

func (d *Store) GetSetting(key string) (string, bool) {
	if d == nil {
		return "", false
	}
	var row setting
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return "", false
	}
	return row.Value, true
}

func (d *Store) SetSetting(key, value string) {
	if d == nil {
		return
	}
	d.db.Save(&setting{Key: key, Value: value, UpdatedAt: time.Now().Unix()})
}
