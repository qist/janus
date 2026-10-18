package db

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// store.go —— 新数据模型的 CRUD（providers / credentials / models / routes /
// keys / agents）。全部是「数据」，没有隐藏的全局单例状态。

// ErrDisabled 表示持久化层未启用（内存模式）。
var ErrDisabled = errors.New("persistence disabled")

// ErrNotFound 表示资源不存在。
var ErrNotFound = errors.New("not found")

// ErrConflict 表示唯一约束冲突（如同 provider 下重复 label）。
var ErrConflict = errors.New("conflict")

func now() int64 { return time.Now().Unix() }

// ---------- providers ----------

// ListProviders 列出 provider（enabledOnly 时只返回启用的）。
func (d *Store) ListProviders(enabledOnly bool) []Provider {
	if d == nil {
		return nil
	}
	q := d.db.Model(&Provider{})
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var rows []Provider
	q.Order("id asc").Find(&rows)
	return rows
}

// GetProvider 按 id 取 provider。
func (d *Store) GetProvider(id string) (Provider, bool) {
	if d == nil || id == "" {
		return Provider{}, false
	}
	var row Provider
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return Provider{}, false
	}
	return row, true
}

// SaveProvider 新建或更新 provider。
func (d *Store) SaveProvider(p Provider) (Provider, error) {
	if d == nil {
		return p, ErrDisabled
	}
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return p, errors.New("provider id is required")
	}
	p.UpdatedAt = now()
	if p.CreatedAt == 0 {
		p.CreatedAt = p.UpdatedAt
	}
	if err := d.db.Save(&p).Error; err != nil {
		return p, err
	}
	return p, nil
}

// DeleteProvider 删除 provider 及其凭据、模型、路由。
func (d *Store) DeleteProvider(id string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&Provider{}, "id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Delete(&Credential{}, "provider_id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Delete(&Model{}, "provider_id = ?", id).Error; err != nil {
			return err
		}
		return tx.Delete(&ModelRoute{}, "scope = ? AND scope_id = ?", RouteScopeProvider, id).Error
	})
}

// ---------- credentials ----------

// CredentialInput 是创建/更新凭据的入参。Secret 为明文 JSON（入库前加密）。
type CredentialInput struct {
	ID       string
	Label    string
	Secret   map[string]any
	Priority int
	Enabled  *bool
}

// ListCredentials 列出凭据；secret 不解密。
func (d *Store) ListCredentials(providerID string) []Credential {
	if d == nil {
		return nil
	}
	q := d.db.Model(&Credential{})
	if providerID != "" {
		q = q.Where("provider_id = ?", providerID)
	}
	var rows []Credential
	q.Order("provider_id asc, priority asc, created_at asc").Find(&rows)
	return rows
}

// GetCredential 按 id 取凭据（含密文，未解密）。
func (d *Store) GetCredential(id string) (Credential, bool) {
	if d == nil || id == "" {
		return Credential{}, false
	}
	var row Credential
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return Credential{}, false
	}
	return row, true
}

// CredentialSecret 解密凭据明文（JSON object）。
func (d *Store) CredentialSecret(id string) (map[string]any, bool) {
	row, ok := d.GetCredential(id)
	if !ok {
		return nil, false
	}
	plain, err := DecryptBlob(row.Secret)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, false
	}
	return m, true
}

// FindCredentialByLabel 按 provider 内的 label 查凭据（不区分大小写）。
// providerID 为空表示在所有 provider 中查找。
func (d *Store) FindCredentialByLabel(providerID, label string) (Credential, bool) {
	if d == nil || label == "" {
		return Credential{}, false
	}
	var rows []Credential
	q := d.db.Model(&Credential{})
	if providerID != "" {
		q = q.Where("provider_id = ?", providerID)
	}
	q.Find(&rows)
	for _, r := range rows {
		if strings.EqualFold(r.Label, label) {
			return r, true
		}
	}
	return Credential{}, false
}

// CreateCredential 新建凭据（secret 加密入库）。返回带 ID 的行。
func (d *Store) CreateCredential(providerID string, in CredentialInput) (Credential, error) {
	if d == nil {
		return Credential{}, ErrDisabled
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return Credential{}, errors.New("provider_id is required")
	}
	if _, ok := d.GetProvider(providerID); !ok {
		return Credential{}, ErrNotFound
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return Credential{}, errors.New("label is required")
	}
	if _, ok := d.FindCredentialByLabel(providerID, label); ok {
		return Credential{}, ErrConflict
	}
	raw, err := json.Marshal(in.Secret)
	if err != nil {
		return Credential{}, err
	}
	blob, err := EncryptBlob(raw)
	if err != nil {
		return Credential{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	row := Credential{
		ID:         "cred_" + RandomToken(9),
		ProviderID: providerID,
		Label:      label,
		Secret:     blob,
		Priority:   in.Priority,
		Enabled:    enabled,
		CreatedAt:  now(),
		UpdatedAt:  now(),
	}
	if err := d.db.Create(&row).Error; err != nil {
		return row, err
	}
	return row, nil
}

// UpdateCredential 更新凭据。secret 为 nil 表示不改。
func (d *Store) UpdateCredential(id string, label *string, secret map[string]any, priority *int, enabled *bool) (Credential, error) {
	if d == nil {
		return Credential{}, ErrDisabled
	}
	row, ok := d.GetCredential(id)
	if !ok {
		return Credential{}, ErrNotFound
	}
	updates := map[string]any{"updated_at": now()}
	if label != nil {
		l := strings.TrimSpace(*label)
		if l == "" {
			return row, errors.New("label must not be empty")
		}
		if other, found := d.FindCredentialByLabel(row.ProviderID, l); found && other.ID != row.ID {
			return row, ErrConflict
		}
		updates["label"] = l
	}
	if secret != nil {
		raw, err := json.Marshal(secret)
		if err != nil {
			return row, err
		}
		blob, err := EncryptBlob(raw)
		if err != nil {
			return row, err
		}
		updates["secret"] = blob
	}
	if priority != nil {
		updates["priority"] = *priority
	}
	if enabled != nil {
		updates["enabled"] = *enabled
	}
	if err := d.db.Model(&Credential{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return row, err
	}
	row, _ = d.GetCredential(id)
	return row, nil
}

// DeleteCredential 删除凭据及其路由。
func (d *Store) DeleteCredential(id string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&Credential{}, "id = ?", id).Error; err != nil {
			return err
		}
		return tx.Delete(&ModelRoute{}, "scope = ? AND scope_id = ?", RouteScopeCredential, id).Error
	})
}

// MarkCredentialError 记录失败并设置冷却截止时间。
func (d *Store) MarkCredentialError(id, msg string, failUntil int64) {
	if d == nil || id == "" {
		return
	}
	d.db.Model(&Credential{}).Where("id = ?", id).Updates(map[string]any{
		"last_error": msg, "fail_until": failUntil, "updated_at": now(),
	})
}

// MarkCredentialUsed 记录成功使用时间并清除错误。
func (d *Store) MarkCredentialUsed(id string) {
	if d == nil || id == "" {
		return
	}
	d.db.Model(&Credential{}).Where("id = ?", id).Updates(map[string]any{
		"last_used_at": now(), "last_error": "", "fail_until": 0,
	})
}

// ---------- models ----------

// ListModels 列出模型目录（providerID 为空 = 全部）。
func (d *Store) ListModels(providerID string, enabledOnly bool) []Model {
	if d == nil {
		return nil
	}
	q := d.db.Model(&Model{})
	if providerID != "" {
		q = q.Where("provider_id = ?", providerID)
	}
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var rows []Model
	q.Order("provider_id asc, upstream_id asc").Find(&rows)
	return rows
}

// GetModel 按 id（provider/upstream）取模型。
func (d *Store) GetModel(id string) (Model, bool) {
	if d == nil || id == "" {
		return Model{}, false
	}
	var row Model
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return Model{}, false
	}
	return row, true
}

// FindModelByUpstream 按 provider + upstream id 精确查找。
// providerID 为空表示在所有 provider 中查找（返回第一条启用的）。
func (d *Store) FindModelByUpstream(providerID, upstreamID string) (Model, bool) {
	if d == nil || upstreamID == "" {
		return Model{}, false
	}
	var row Model
	q := d.db.Model(&Model{}).Where("upstream_id = ?", upstreamID)
	if providerID != "" {
		q = q.Where("provider_id = ?", providerID)
	}
	if err := q.Order("provider_id asc").First(&row).Error; err != nil {
		return Model{}, false
	}
	return row, true
}

// SaveModel 新建或更新模型目录条目。
func (d *Store) SaveModel(m Model) (Model, error) {
	if d == nil {
		return m, ErrDisabled
	}
	m.ProviderID = strings.TrimSpace(m.ProviderID)
	m.UpstreamID = strings.TrimSpace(m.UpstreamID)
	if m.ProviderID == "" || m.UpstreamID == "" {
		return m, errors.New("provider_id and upstream_id are required")
	}
	if m.ID == "" {
		m.ID = m.ProviderID + "/" + m.UpstreamID
	}
	m.UpdatedAt = now()
	if m.CreatedAt == 0 {
		m.CreatedAt = m.UpdatedAt
	}
	if err := d.db.Save(&m).Error; err != nil {
		return m, err
	}
	return m, nil
}

// DeleteModel 删除目录条目。
func (d *Store) DeleteModel(id string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Delete(&Model{}, "id = ?", id).Error
}

// ReplaceProviderModels 用发现结果覆盖某 provider 的目录（保留手工条目）。
func (d *Store) ReplaceProviderModels(providerID string, models []Model) {
	if d == nil || providerID == "" {
		return
	}
	d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&Model{}, "provider_id = ? AND source = ?", providerID, ModelSourceDiscovered).Error; err != nil {
			return err
		}
		for _, m := range models {
			m.ProviderID = providerID
			m.Source = ModelSourceDiscovered
			if m.ID == "" {
				m.ID = providerID + "/" + m.UpstreamID
			}
			if m.CreatedAt == 0 {
				m.CreatedAt = now()
			}
			m.UpdatedAt = now()
			if err := tx.Save(&m).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------- routes ----------

// Route 作用域常量。
const (
	RouteScopeGlobal     = "global"
	RouteScopeProvider   = "provider"
	RouteScopeCredential = "credential"
	RouteScopeRole       = "role"
)

// ModelSource 常量。
const (
	ModelSourceDiscovered = "discovered"
	ModelSourceManual     = "manual"
)

// ListRoutes 列出路由规则（scope 为空 = 全部）。
func (d *Store) ListRoutes(scope string) []ModelRoute {
	if d == nil {
		return nil
	}
	q := d.db.Model(&ModelRoute{})
	if scope != "" {
		q = q.Where("scope = ?", scope)
	}
	var rows []ModelRoute
	q.Order("priority asc, created_at asc").Find(&rows)
	return rows
}

// RoutesFor 返回参与解析的规则：credential（指定时）+ provider + global + role。
// 顺序即优先级：credential > provider > global；role 由调用方单独处理。
func (d *Store) RoutesFor(providerID, credentialID string) []ModelRoute {
	if d == nil {
		return nil
	}
	var rows []ModelRoute
	q := d.db.Model(&ModelRoute{}).Where("enabled = ?", true)
	switch {
	case credentialID != "":
		q = q.Where("(scope = ? AND scope_id = ?) OR (scope = ? AND scope_id = ?) OR scope = ?",
			RouteScopeCredential, credentialID, RouteScopeProvider, providerID, RouteScopeGlobal)
	case providerID != "":
		q = q.Where("(scope = ? AND scope_id = ?) OR scope = ?", RouteScopeProvider, providerID, RouteScopeGlobal)
	default:
		q = q.Where("scope = ?", RouteScopeGlobal)
	}
	q.Order("priority asc, created_at asc").Find(&rows)
	// 作用域越具体越靠前（同优先级下 credential 先于 provider 先于 global）
	rank := map[string]int{RouteScopeCredential: 0, RouteScopeProvider: 1, RouteScopeGlobal: 2}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		return rank[rows[i].Scope] < rank[rows[j].Scope]
	})
	return rows
}

// RoleRoutes 返回 role 作用域的全部规则。
func (d *Store) RoleRoutes() []ModelRoute {
	if d == nil {
		return nil
	}
	var rows []ModelRoute
	d.db.Where("scope = ? AND enabled = ?", RouteScopeRole, true).Order("priority asc").Find(&rows)
	return rows
}

// GetRoute 按 id 取规则。
func (d *Store) GetRoute(id string) (ModelRoute, bool) {
	if d == nil || id == "" {
		return ModelRoute{}, false
	}
	var row ModelRoute
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return ModelRoute{}, false
	}
	return row, true
}

// SaveRoute 新建或更新规则。alias 统一小写。
func (d *Store) SaveRoute(r ModelRoute) (ModelRoute, error) {
	if d == nil {
		return r, ErrDisabled
	}
	r.Scope = strings.ToLower(strings.TrimSpace(r.Scope))
	switch r.Scope {
	case RouteScopeGlobal, RouteScopeProvider, RouteScopeCredential, RouteScopeRole:
	default:
		return r, errors.New(`scope must be global|provider|credential|role`)
	}
	if r.Scope == RouteScopeGlobal {
		r.ScopeID = ""
	}
	if r.Scope != RouteScopeGlobal && strings.TrimSpace(r.ScopeID) == "" {
		return r, errors.New("scope_id is required for non-global scope")
	}
	r.Alias = strings.ToLower(strings.TrimSpace(r.Alias))
	r.Target = strings.TrimSpace(r.Target)
	if r.Alias == "" || r.Target == "" {
		return r, errors.New("alias and target are required")
	}
	if r.ID == "" {
		r.ID = "rt_" + RandomToken(9)
	}
	r.UpdatedAt = now()
	if r.CreatedAt == 0 {
		r.CreatedAt = r.UpdatedAt
	}
	if err := d.db.Save(&r).Error; err != nil {
		return r, err
	}
	return r, nil
}

// DeleteRoute 删除规则。
func (d *Store) DeleteRoute(id string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Delete(&ModelRoute{}, "id = ?", id).Error
}

// ---------- api keys ----------

// Key 种类常量。
const (
	KeyKindAdmin  = "admin"
	KeyKindClient = "client"
	KeyKindAgent  = "agent"
	KeyKindTool   = "tool"
)

// KeyPrefix 返回各 kind 的明文前缀。
func KeyPrefix(kind string) string {
	switch kind {
	case KeyKindAdmin:
		return "sk-janus-a-"
	case KeyKindClient:
		return "sk-janus-c-"
	case KeyKindAgent:
		return "sk-janus-g-"
	case KeyKindTool:
		return "sk-janus-t-"
	default:
		return "sk-janus-x-"
	}
}

// ListKeys 列出 key（不含明文）。
func (d *Store) ListKeys(kind string) []APIKey {
	if d == nil {
		return nil
	}
	q := d.db.Model(&APIKey{})
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var rows []APIKey
	q.Order("created_at desc").Find(&rows)
	return rows
}

// GetKey 按 id 取 key。
func (d *Store) GetKey(id string) (APIKey, bool) {
	if d == nil || id == "" {
		return APIKey{}, false
	}
	var row APIKey
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return APIKey{}, false
	}
	return row, true
}

// LookupKey 用明文 key 查记录（sha256 摘要匹配），并校验吊销/过期。
func (d *Store) LookupKey(plain string) (APIKey, bool) {
	if d == nil || plain == "" {
		return APIKey{}, false
	}
	var row APIKey
	if err := d.db.First(&row, "hash = ?", HashKey(plain)).Error; err != nil {
		return APIKey{}, false
	}
	if row.Revoked {
		return APIKey{}, false
	}
	if row.ExpiresAt > 0 && row.ExpiresAt < now() {
		return APIKey{}, false
	}
	return row, true
}

// CreateKey 生成新 key，返回记录与一次性明文。
func (d *Store) CreateKey(kind, name string, scopes, providerIDs, credentialIDs []string, expiresAt int64) (APIKey, string, error) {
	if d == nil {
		return APIKey{}, "", ErrDisabled
	}
	switch kind {
	case KeyKindAdmin, KeyKindClient, KeyKindAgent, KeyKindTool:
	default:
		return APIKey{}, "", errors.New("kind must be admin|client|agent|tool")
	}
	prefix := KeyPrefix(kind)
	plain := prefix + RandomToken(24)
	row := APIKey{
		ID:         "key_" + RandomToken(9),
		Kind:       kind,
		Name:       strings.TrimSpace(name),
		Prefix:     prefix,
		Hash:       HashKey(plain),
		Scopes:     encodeStrings(scopes),
		ProviderIDs: encodeStrings(providerIDs),
		CredentialIDs: encodeStrings(credentialIDs),
		ExpiresAt:  expiresAt,
		CreatedAt:  now(),
	}
	if err := d.db.Create(&row).Error; err != nil {
		return row, "", err
	}
	return row, plain, nil
}

// RevokeKey 吊销 key。
func (d *Store) RevokeKey(id string) error {
	if d == nil {
		return ErrDisabled
	}
	res := d.db.Model(&APIKey{}).Where("id = ?", id).Update("revoked", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateKey 轮换 key：吊销旧记录并返回新明文。
func (d *Store) RotateKey(id string) (APIKey, string, error) {
	old, ok := d.GetKey(id)
	if !ok {
		return APIKey{}, "", ErrNotFound
	}
	if err := d.RevokeKey(id); err != nil {
		return APIKey{}, "", err
	}
	var scopes, providers, creds []string
	_ = json.Unmarshal(old.Scopes, &scopes)
	_ = json.Unmarshal(old.ProviderIDs, &providers)
	_ = json.Unmarshal(old.CredentialIDs, &creds)
	return d.CreateKey(old.Kind, old.Name, scopes, providers, creds, old.ExpiresAt)
}

// TouchKey 更新最后使用时间（失败忽略）。
func (d *Store) TouchKey(id string) {
	if d == nil || id == "" {
		return
	}
	d.db.Model(&APIKey{}).Where("id = ?", id).Update("last_used_at", now())
}

// HasKind 判断是否存在某类未吊销的 key。
func (d *Store) HasKind(kind string) bool {
	if d == nil {
		return false
	}
	var n int64
	d.db.Model(&APIKey{}).Where("kind = ? AND revoked = ?", kind, false).Count(&n)
	return n > 0
}

func encodeStrings(v []string) []byte {
	if len(v) == 0 {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// DecodeStrings 把 JSON 数组列解成切片（空/坏值返回 nil）。
func DecodeStrings(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ---------- agents ----------

// ListAgents 列出 harness 注册记录。
func (d *Store) ListAgents() []Agent {
	if d == nil {
		return nil
	}
	var rows []Agent
	d.db.Order("id asc").Find(&rows)
	return rows
}

// GetAgent 按 id 取 harness。
func (d *Store) GetAgent(id string) (Agent, bool) {
	if d == nil || id == "" {
		return Agent{}, false
	}
	var row Agent
	if err := d.db.First(&row, "id = ?", id).Error; err != nil {
		return Agent{}, false
	}
	return row, true
}

// SaveAgent 新建或更新 harness。
func (d *Store) SaveAgent(a Agent) (Agent, error) {
	if d == nil {
		return a, ErrDisabled
	}
	a.ID = strings.TrimSpace(a.ID)
	if a.ID == "" {
		return a, errors.New("agent id is required")
	}
	a.UpdatedAt = now()
	if a.CreatedAt == 0 {
		a.CreatedAt = a.UpdatedAt
	}
	if err := d.db.Save(&a).Error; err != nil {
		return a, err
	}
	return a, nil
}

// DeleteAgent 删除 harness。
func (d *Store) DeleteAgent(id string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Delete(&Agent{}, "id = ?", id).Error
}

// ---------- admin user ----------

// AdminCount 返回管理员账号数量。
func (d *Store) AdminCount() int64 {
	if d == nil {
		return 0
	}
	var n int64
	d.db.Model(&AdminUser{}).Count(&n)
	return n
}

// FindAdmin 按用户名取管理员。
func (d *Store) FindAdmin(username string) (AdminUser, bool) {
	if d == nil || username == "" {
		return AdminUser{}, false
	}
	var row AdminUser
	if err := d.db.First(&row, "username = ?", username).Error; err != nil {
		return AdminUser{}, false
	}
	return row, true
}

// CreateAdmin 新建管理员。
func (d *Store) CreateAdmin(username, password string) (AdminUser, error) {
	if d == nil {
		return AdminUser{}, ErrDisabled
	}
	row := AdminUser{
		ID:           "adm_" + RandomToken(9),
		Username:     strings.TrimSpace(username),
		PasswordHash: HashPassword(password),
		CreatedAt:    now(),
		UpdatedAt:    now(),
	}
	if row.Username == "" {
		return row, errors.New("username is required")
	}
	if err := d.db.Create(&row).Error; err != nil {
		return row, err
	}
	return row, nil
}

// SetAdminPassword 修改管理员密码。
func (d *Store) SetAdminPassword(id, password string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Model(&AdminUser{}).Where("id = ?", id).Updates(map[string]any{
		"password_hash": HashPassword(password), "updated_at": now(),
	}).Error
}
