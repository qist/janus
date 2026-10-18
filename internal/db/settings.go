package db

import (
	"encoding/json"
	"time"
)

// settings.go —— settings 表的读写。值统一是 JSON，保留类型。
//
// 运行期配置整体存一行（key = "runtime"，见 config.Specs），其余零散键
// （ui_prefs / default_model 等）也走同一张表。

// SettingRuntime 是整份运行期配置的 key。
const SettingRuntime = "runtime"

// GetSetting 读字符串设置。值可能是 JSON 字符串（"abc"）或裸串（abc）。
func (d *Store) GetSetting(key string) (string, bool) {
	if d == nil || key == "" {
		return "", false
	}
	var row Setting
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return "", false
	}
	if len(row.Value) == 0 {
		return "", true
	}
	var s string
	if err := json.Unmarshal(row.Value, &s); err == nil {
		return s, true
	}
	return string(row.Value), true
}

// GetSettingRaw 读设置原始 JSON 字节（用于整份 runtime 配置）。
func (d *Store) GetSettingRaw(key string) ([]byte, bool) {
	if d == nil || key == "" {
		return nil, false
	}
	var row Setting
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return nil, false
	}
	return row.Value, true
}

// SetSetting 写字符串设置（JSON 编码，保留引号语义）。
func (d *Store) SetSetting(key, value string) error {
	return d.SetSettingJSON(key, value)
}

// GetSettingJSON 把设置值解到 v。
func (d *Store) GetSettingJSON(key string, v any) bool {
	if d == nil || key == "" {
		return false
	}
	var row Setting
	if err := d.db.First(&row, "key = ?", key).Error; err != nil {
		return false
	}
	if len(row.Value) == 0 {
		return false
	}
	return json.Unmarshal(row.Value, v) == nil
}

// SetSettingJSON 把 v 编码成 JSON 写入设置。
func (d *Store) SetSettingJSON(key string, v any) error {
	if d == nil {
		return ErrDisabled
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return d.SetSettingRaw(key, raw)
}

// SetSettingRaw 直接写 JSON 字节。
func (d *Store) SetSettingRaw(key string, raw []byte) error {
	if d == nil {
		return ErrDisabled
	}
	row := Setting{Key: key, Value: raw, UpdatedAt: time.Now().Unix()}
	return d.db.Save(&row).Error
}

// DeleteSetting 删除一项设置。
func (d *Store) DeleteSetting(key string) error {
	if d == nil {
		return ErrDisabled
	}
	return d.db.Delete(&Setting{}, "key = ?", key).Error
}

// AllSettings 返回全部设置（key → 原始 JSON）。
func (d *Store) AllSettings() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if d == nil {
		return out
	}
	var rows []Setting
	d.db.Find(&rows)
	for _, r := range rows {
		out[r.Key] = json.RawMessage(r.Value)
	}
	return out
}
