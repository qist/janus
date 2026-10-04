package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qist/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// db.go —— 基于 SQLite（github.com/qist/sqlite + gorm）的持久化层。
//
// 目前持久化两类数据，让 Responses 的 previous_response_id 能跨进程重启续链：
//   - responses：响应本身（id → convKey + payload + 过期时间）
//   - conversations：会话键 → 上游 OpenCode sessionID（+ model/agent/dir）
//
// 未配置 BRIDGE_DB（或设为 memory/off）时整层为 nil，退回纯内存行为。

type dbResponse struct {
	ID        string `gorm:"primaryKey;size:80"`
	ConvKey   string `gorm:"index;size:160"`
	ExpiresAt int64  `gorm:"index"`
	CreatedAt int64
	Payload   []byte
}

type dbConversation struct {
	Key        string `gorm:"primaryKey;size:160"`
	SessionID  string `gorm:"size:160"`
	ProviderID string `gorm:"size:80"`
	ModelID    string `gorm:"size:80"`
	Variant    string `gorm:"size:40"`
	Agent      string `gorm:"size:80"`
	Directory  string `gorm:"size:512"`
	UpdatedAt  int64  `gorm:"index"`
}

// dbMemoryDSN 表示"不用文件、纯内存"。
func dbMemoryDSN(path string) bool {
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "", "memory", "off", "none", ":memory:":
		return true
	}
	return false
}

type dbStore struct {
	db *gorm.DB
}

// openDB 打开（必要时创建）SQLite 库并建表。不负责决定是否启用（调用方判断）。
func openDB(path string) (*dbStore, error) {
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
	if err := gdb.AutoMigrate(&dbResponse{}, &dbConversation{}); err != nil {
		return nil, err
	}
	return &dbStore{db: gdb}, nil
}

func (d *dbStore) close() {
	if d == nil || d.db == nil {
		return
	}
	if sqlDB, err := d.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// ---------- responses ----------

func (d *dbStore) putResponse(id, convKey string, expiresAt int64, payload []byte) {
	if d == nil {
		return
	}
	row := dbResponse{ID: id, ConvKey: convKey, ExpiresAt: expiresAt, CreatedAt: time.Now().Unix(), Payload: payload}
	// Upsert：同 id 覆盖
	d.db.Save(&row)
}

func (d *dbStore) getResponse(id string) (payload []byte, convKey string, expiresAt int64, ok bool) {
	if d == nil {
		return nil, "", 0, false
	}
	var row dbResponse
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return nil, "", 0, false
	}
	return row.Payload, row.ConvKey, row.ExpiresAt, true
}

func (d *dbStore) deleteResponse(id string) bool {
	if d == nil {
		return false
	}
	res := d.db.Delete(&dbResponse{}, "id = ?", id)
	return res.Error == nil && res.RowsAffected > 0
}

func (d *dbStore) gcResponses(now int64) {
	if d == nil {
		return
	}
	d.db.Delete(&dbResponse{}, "expires_at > 0 AND expires_at < ?", now)
}

// ---------- conversations ----------

func (d *dbStore) saveConv(row dbConversation) {
	if d == nil {
		return
	}
	row.UpdatedAt = time.Now().Unix()
	d.db.Save(&row)
}

func (d *dbStore) loadConv(key string) (dbConversation, bool) {
	if d == nil {
		return dbConversation{}, false
	}
	var row dbConversation
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return dbConversation{}, false
	}
	return row, true
}

func (d *dbStore) deleteConv(key string) {
	if d == nil || key == "" {
		return
	}
	d.db.Delete(&dbConversation{}, "key = ?", key)
}

func (d *dbStore) gcConvs(before int64) {
	if d == nil {
		return
	}
	d.db.Delete(&dbConversation{}, "updated_at > 0 AND updated_at < ?", before)
}

// defaultDBPath 按 XDG 规则给出默认库路径。
func defaultDBPath() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "janus", "janus.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "janus", "janus.db")
}
