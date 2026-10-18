// Package db 实现基于 SQLite（github.com/qist/sqlite + gorm）的持久化层。
//
// 所有配置都是数据：providers / credentials / models / routes / keys /
// agents / settings，以及会话映射与 Responses 载荷。
//
// 数据库是唯一的运行期配置来源；env 只用于 bootstrap（见 internal/config）。
package db

import (
	"errors"
	"fmt"
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
	AgentID    string `gorm:"size:64"`
	Credential string `gorm:"size:64"`
	Directory  string `gorm:"size:512"`
	// History 是 Chat 的规范化历史快照（[]ChatMessage 的 JSON），
	// 用于跨进程重启后仍能做历史前缀匹配、只发增量。
	History   []byte
	UpdatedAt int64 `gorm:"index"`
}

// Store 是持久化层句柄。
type Store struct {
	db *gorm.DB
}

// ErrLegacySchema 表示库中存在旧版本表，按「不兼容」策略拒绝启动。
var ErrLegacySchema = errors.New("database contains legacy schema (accounts/account_models); remove the file or point JANUS_DB elsewhere")

// MemoryDSN 表示"不用文件、纯内存"。仅测试使用；生产必须落盘，
// 否则首启生成的 admin key / 凭据加密主密钥都会随进程消失。
func MemoryDSN(path string) bool {
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "", "memory", "off", "none", ":memory:":
		return true
	}
	return false
}

// Open 打开（必要时创建）SQLite 库并建表。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("empty database path")
	}
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
	if gdb.Migrator().HasTable("accounts") || gdb.Migrator().HasTable("account_models") {
		sqlDB, _ := gdb.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		return nil, ErrLegacySchema
	}
	if err := gdb.AutoMigrate(
		&response{}, &Conversation{},
		&Provider{}, &Credential{}, &Model{}, &ModelRoute{},
		&APIKey{}, &Agent{}, &Setting{}, &AdminUser{},
	); err != nil {
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

// DB 暴露底层 gorm 句柄（供同包外的高级查询使用）。
func (d *Store) DB() *gorm.DB { return d.db }

// ---------- responses ----------

func (d *Store) PutResponse(id, convKey, dir string, expiresAt int64, payload []byte) {
	if d == nil {
		return
	}
	row := response{ID: id, ConvKey: convKey, Directory: dir, ExpiresAt: expiresAt, CreatedAt: time.Now().Unix(), Payload: payload}
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
func (d *Store) ClearSession(sid string) {
	if d == nil || sid == "" {
		return
	}
	d.db.Model(&Conversation{}).Where("session_id = ?", sid).Updates(map[string]any{
		"session_id": "", "provider_id": "", "model_id": "", "variant": "",
	})
}

// ListConvs 列出会话（管理面用）。
func (d *Store) ListConvs(limit int) []Conversation {
	if d == nil {
		return nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows []Conversation
	d.db.Order("updated_at desc").Limit(limit).Find(&rows)
	return rows
}

func (d *Store) GCConvs(before int64) {
	if d == nil {
		return
	}
	d.db.Delete(&Conversation{}, "updated_at > 0 AND updated_at < ?", before)
}
