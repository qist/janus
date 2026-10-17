package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"gorm.io/gorm"
)

// AccountKind 是账号类型。
const (
	AccountKindOpenCode  = "opencode"
	AccountKindWorkBuddy = "workbuddy"
)

// Account 是 janus 自己持久化的上游账号（OpenCode / WorkBuddy）。
// 凭据以加密形式存 credential 字段，密钥由 SetAccountKey / 环境派生。
type Account struct {
	ID          string `gorm:"primaryKey;size:80"`
	Kind        string `gorm:"index;size:40"`
	ProviderID  string `gorm:"size:80"`  // opencode / opencode-go / workbuddy / ...
	Label       string `gorm:"size:80"`
	ExternalID  string `gorm:"size:120"` // 上游侧账号标识（OpenCode 的 label/credential id），用于 auth switch
	Credential  []byte `gorm:"type:blob"` // AES-GCM 密文
	Active      bool   `gorm:"index"`
	CreatedAt   int64
	UpdatedAt   int64
}

// AccountModel 记录某个账号"拥有/可见"的模型集合（自动发现后写入）。
type AccountModel struct {
	AccountID string `gorm:"primaryKey;size:80"`
	ModelID   string `gorm:"primaryKey;size:160"`
	UpdatedAt int64
}

// accountKey 是凭据加密密钥（AES-256）。由 SetAccountKey 设置；未设置时派生自 BRIDGE_API_KEY。
var (
	accountKeyMu sync.RWMutex
	accountKey   []byte
)

// SetAccountKey 设置凭据加密密钥。key 为空时派生自 BRIDGE_API_KEY（或 dev 默认）。
func (d *Store) SetAccountKey(key string) {
	accountKeyMu.Lock()
	defer accountKeyMu.Unlock()
	src := key
	if src == "" {
		src = os.Getenv("BRIDGE_API_KEY")
	}
	if src == "" {
		src = "janus-dev-account-key"
	}
	h := sha256.Sum256([]byte(src))
	accountKey = h[:]
}

func accountKeyBytes() []byte {
	accountKeyMu.RLock()
	defer accountKeyMu.RUnlock()
	if len(accountKey) == 32 {
		return accountKey
	}
	h := sha256.Sum256([]byte("janus-dev-account-key"))
	return h[:]
}

// encryptBlob 用 AES-GCM 加密任意 JSON。
func encryptBlob(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(accountKeyBytes())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// decryptBlob 反之。
func decryptBlob(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(accountKeyBytes())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("credential blob too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// ---------- accounts CRUD ----------

// CreateAccount 新增账号。credential 为任意 JSON（明文，入库时加密）。
func (d *Store) CreateAccount(a Account, credential any) (Account, error) {
	if d == nil {
		return a, fmt.Errorf("db disabled")
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return a, err
	}
	blob, err := encryptBlob(raw)
	if err != nil {
		return a, err
	}
	a.Credential = blob
	if a.ID == "" {
		a.ID = "acc_" + newID()
	}
	now := time.Now().Unix()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if err := d.db.Create(&a).Error; err != nil {
		return a, err
	}
	return a, nil
}

// GetAccount 按 id 取账号（含解密后的 credential）。
func (d *Store) GetAccount(id string) (Account, map[string]any, bool) {
	if d == nil {
		return Account{}, nil, false
	}
	var row Account
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return Account{}, nil, false
	}
	cred, err := decryptBlob(row.Credential)
	if err != nil {
		return row, nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(cred, &m); err != nil {
		return row, nil, false
	}
	return row, m, true
}

// ListAccounts 列出账号；credential 解密失败的账号仍返回（Credential=nil）。
func (d *Store) ListAccounts(kind string) []Account {
	if d == nil {
		return nil
	}
	q := d.db.Model(&Account{})
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var rows []Account
	q.Order("active desc, created_at asc").Find(&rows)
	return rows
}

// SetActiveAccount 把某账号设为该 kind 的唯一 active。
func (d *Store) SetActiveAccount(kind, id string) error {
	if d == nil {
		return fmt.Errorf("db disabled")
	}
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Account{}).Where("kind = ?", kind).Update("active", false).Error; err != nil {
			return err
		}
		res := tx.Model(&Account{}).Where("id = ? AND kind = ?", id, kind).Update("active", true)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("account %s not found for kind %s", id, kind)
		}
		return nil
	})
}

// DeleteAccount 删除账号及其模型映射。
func (d *Store) DeleteAccount(id string) bool {
	if d == nil {
		return false
	}
	d.db.Delete(&AccountModel{}, "account_id = ?", id)
	res := d.db.Delete(&Account{}, "id = ?", id)
	return res.Error == nil && res.RowsAffected > 0
}

// UpdateAccountCredential 更新凭据（加密）。
func (d *Store) UpdateAccountCredential(id string, credential any) error {
	if d == nil {
		return fmt.Errorf("db disabled")
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	blob, err := encryptBlob(raw)
	if err != nil {
		return err
	}
	res := d.db.Model(&Account{}).Where("id = ?", id).Updates(map[string]any{
		"credential": blob, "updated_at": time.Now().Unix(),
	})
	return res.Error
}

// ---------- account_models ----------

// ReplaceAccountModels 覆盖某账号的模型集合（自动发现后调用）。
func (d *Store) ReplaceAccountModels(accountID string, modelIDs []string) {
	if d == nil {
		return
	}
	d.db.Delete(&AccountModel{}, "account_id = ?", accountID)
	now := time.Now().Unix()
	for _, m := range modelIDs {
		d.db.Create(&AccountModel{AccountID: accountID, ModelID: m, UpdatedAt: now})
	}
}

// AccountModels 返回某账号的模型列表。
func (d *Store) AccountModels(accountID string) []string {
	if d == nil {
		return nil
	}
	var rows []AccountModel
	d.db.Where("account_id = ?", accountID).Find(&rows)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ModelID)
	}
	return out
}

// ModelsForAccount 返回 model_id -> account_id 的反查（路由用）。
func (d *Store) ModelsForAccount(modelID string) string {
	if d == nil {
		return ""
	}
	var row AccountModel
	if err := d.db.First(&row, "model_id = ?", modelID).Error; err != nil {
		return ""
	}
	return row.AccountID
}

// UpdateAccountExternalID 更新账号的上游侧标识（OpenCode label）。
func (d *Store) UpdateAccountExternalID(id, ext string) {
	if d == nil || ext == "" {
		return
	}
	d.db.Model(&Account{}).Where("id = ?", id).Update("external_id", ext)
}
func (d *Store) ActiveAccount(kind string) (Account, map[string]any, bool) {
	if d == nil {
		return Account{}, nil, false
	}
	var row Account
	if err := d.db.Where("kind = ? AND active = ?", kind, true).First(&row).Error; err != nil {
		return Account{}, nil, false
	}
	cred, err := decryptBlob(row.Credential)
	if err != nil {
		return row, nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(cred, &m); err != nil {
		return row, nil, false
	}
	return row, m, true
}

// newID 生成短 id（复用 db 包已有的？这里自给）。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}