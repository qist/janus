package db

import (
	"sort"
	"time"

	"gorm.io/gorm"
)

// usage.go —— 本地用量记账（usage_events）的写入与聚合。
//
// 设计见 docs/ARCHITECTURE.md §11（P1.5 观测与计量）：每次模型面请求落一行，
// 记录 provider / credential / key / model 以及 tokens、缓存分项、成本、耗时。
// 聚合在 Go 侧完成：SQLite 单机数据量有限，按时间窗 + 索引扫描足够。

// UsageFilter 是查询/聚合的过滤条件。零值表示不限。
type UsageFilter struct {
	From         int64
	To           int64
	ProviderID   string
	CredentialID string
	Model        string
	KeyID        string
	Source       string
}

// UsageTotals 是一组请求的合计值。
type UsageTotals struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	DurationMs       int64   `json:"duration_ms"`
}

func (t *UsageTotals) add(e UsageEvent) {
	t.Requests++
	if e.Status >= 400 || e.ErrorCode != "" {
		t.Errors++
	}
	t.InputTokens += e.InputTokens
	t.OutputTokens += e.OutputTokens
	t.CacheReadTokens += e.CacheReadTokens
	t.CacheWriteTokens += e.CacheWriteTokens
	t.ReasoningTokens += e.ReasoningTokens
	t.CostUSD += e.CostUSD
	t.DurationMs += e.DurationMs
}

// UsageBucket 是聚合后的一个分组。按 provider/credential/model 聚合时填对应字段，
// 按天聚合时填 Date。
type UsageBucket struct {
	ProviderID   string `json:"provider_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Model        string `json:"model,omitempty"`
	Date         string `json:"date,omitempty"`
	UsageTotals
}

// UsageSummary 是本地记账的完整聚合结果。
type UsageSummary struct {
	Totals       UsageTotals   `json:"totals"`
	ByProvider   []UsageBucket `json:"by_provider"`
	ByCredential []UsageBucket `json:"by_credential"`
	ByModel      []UsageBucket `json:"by_model"`
	ByDay        []UsageBucket `json:"by_day"`
}

// RecordUsage 写入一条用量事件（ID / Source / Ts 缺省时自动补）。
func (d *Store) RecordUsage(e UsageEvent) error {
	if d == nil {
		return ErrDisabled
	}
	if e.ID == "" {
		e.ID = "ue_" + RandomToken(9)
	}
	if e.Source == "" {
		e.Source = "local"
	}
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	return d.db.Create(&e).Error
}

// usageQuery 按过滤条件构造查询。
func (d *Store) usageQuery(f UsageFilter) *gorm.DB {
	q := d.db.Model(&UsageEvent{})
	if f.From > 0 {
		q = q.Where("ts >= ?", f.From)
	}
	if f.To > 0 {
		q = q.Where("ts <= ?", f.To)
	}
	if f.ProviderID != "" {
		q = q.Where("provider_id = ?", f.ProviderID)
	}
	if f.CredentialID != "" {
		q = q.Where("credential_id = ?", f.CredentialID)
	}
	if f.Model != "" {
		q = q.Where("model = ?", f.Model)
	}
	if f.KeyID != "" {
		q = q.Where("key_id = ?", f.KeyID)
	}
	if f.Source != "" {
		q = q.Where("source = ?", f.Source)
	}
	return q
}

// ListUsage 返回最近的用量事件（ts 倒序）。
func (d *Store) ListUsage(f UsageFilter, limit int) []UsageEvent {
	if d == nil {
		return nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []UsageEvent
	d.usageQuery(f).Order("ts desc").Limit(limit).Find(&rows)
	return rows
}

// SummarizeUsage 聚合时间窗内的用量，返回总量、按渠道/凭据/模型/天的分组。
func (d *Store) SummarizeUsage(f UsageFilter) UsageSummary {
	var out UsageSummary
	if d == nil {
		return out
	}
	// 上限防止误查全库拖垮内存；单机网关的日请求量远低于此。
	var rows []UsageEvent
	d.usageQuery(f).Order("ts asc").Limit(500000).Find(&rows)
	out.ByDay = []UsageBucket{}
	out.ByProvider = []UsageBucket{}
	out.ByCredential = []UsageBucket{}
	out.ByModel = []UsageBucket{}

	prov := map[string]*UsageBucket{}
	cred := map[string]*UsageBucket{}
	model := map[string]*UsageBucket{}
	day := map[string]*UsageBucket{}

	get := func(m map[string]*UsageBucket, key string, init func(*UsageBucket)) *UsageBucket {
		b, ok := m[key]
		if !ok {
			b = &UsageBucket{}
			init(b)
			m[key] = b
		}
		return b
	}

	for _, e := range rows {
		out.Totals.add(e)
		get(prov, e.ProviderID, func(b *UsageBucket) { b.ProviderID = e.ProviderID }).add(e)
		get(cred, e.ProviderID+"\x00"+e.CredentialID, func(b *UsageBucket) {
			b.ProviderID, b.CredentialID = e.ProviderID, e.CredentialID
		}).add(e)
		get(model, e.Model, func(b *UsageBucket) { b.Model = e.Model }).add(e)
		date := time.Unix(e.Ts, 0).Format("2006-01-02")
		get(day, date, func(b *UsageBucket) { b.Date = date }).add(e)
	}

	out.ByProvider = sortedBuckets(prov)
	out.ByCredential = sortedBuckets(cred)
	out.ByModel = sortedBuckets(model)
	for _, b := range day {
		out.ByDay = append(out.ByDay, *b)
	}
	sort.Slice(out.ByDay, func(i, j int) bool { return out.ByDay[i].Date < out.ByDay[j].Date })
	return out
}

// sortedBuckets 按花费降序、请求数降序、tokens 降序排列。
func sortedBuckets(m map[string]*UsageBucket) []UsageBucket {
	out := make([]UsageBucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].InputTokens+out[i].OutputTokens > out[j].InputTokens+out[j].OutputTokens
	})
	return out
}

// GCUsage 清理 before 之前的用量事件，返回删除行数。
func (d *Store) GCUsage(before int64) int64 {
	if d == nil || before <= 0 {
		return 0
	}
	res := d.db.Delete(&UsageEvent{}, "ts < ?", before)
	if res.Error != nil {
		return 0
	}
	return res.RowsAffected
}
