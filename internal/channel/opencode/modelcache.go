package opencode

import (
	"context"
	"fmt"
	"sync"
	"time"

	"janus/internal/upstream"
)

// modelcache.go —— OpenCode 模型目录缓存。
//
// 上游 /config/providers 偶尔会 200 + 空列表（实测会让所有模型"消失"），
// 缓存层必须把空列表当成失败、并保留旧数据，否则一次抖动就会让客户端
// 报 unknown provider。

// Cache 缓存上游模型目录与默认模型。
type Cache struct {
	mu      sync.RWMutex
	list    []upstream.OCModel
	def     *upstream.OCModel
	fetched time.Time
	ttl     time.Duration
}

// NewCache 构造模型目录缓存。
func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl}
}

// Get 返回模型列表；缓存新鲜时直接复用。
func (c *Cache) Get(ctx context.Context, up *upstream.Upstream, directory string, force bool) ([]upstream.OCModel, error) {
	c.mu.RLock()
	ok := !force && len(c.list) > 0 && time.Since(c.fetched) < c.ttl
	list := c.list
	c.mu.RUnlock()
	if ok {
		return list, nil
	}

	fresh, err := up.ListModels(ctx, directory)

	c.mu.RLock()
	old := c.list
	c.mu.RUnlock()

	if err == nil && len(fresh) == 0 {
		// 上游偶尔会 200 + 空 body/空数组（实测会导致所有模型"消失"，
		// 客户端随即报 unknown provider）。空列表一律不采纳：
		// 有旧数据就继续用，没有就当失败，别把可用状态覆盖成不可用。
		if len(old) > 0 {
			return old, nil
		}
		err = fmt.Errorf("upstream returned an empty model list")
	}

	if err != nil {
		// 拿不到新数据时退回旧缓存，别让模型列表变成 5xx
		if len(old) > 0 {
			return old, nil
		}
		return nil, err
	}

	c.mu.Lock()
	c.list = fresh
	c.fetched = time.Now()
	c.mu.Unlock()

	// 顺带刷新默认模型；失败不影响列表本身
	if def, derr := up.DefaultModel(ctx, directory); derr == nil {
		c.mu.Lock()
		c.def = def
		c.mu.Unlock()
	}
	return fresh, nil
}

// Default 返回上游默认模型（带缓存）。
//
// 注意 Get() 在缓存新鲜时会直接返回、不再请求上游，所以若上次 def 抓取失败，
// 光靠 Get() 永远补不上；这里在缓存后仍为空时强制刷新一次。
func (c *Cache) Default(ctx context.Context, up *upstream.Upstream, directory string) (*upstream.OCModel, error) {
	if def := c.CachedDefault(); def != nil {
		return def, nil
	}
	if _, err := c.Get(ctx, up, directory, false); err != nil {
		return nil, err
	}
	if def := c.CachedDefault(); def != nil {
		return def, nil
	}
	if _, err := c.Get(ctx, up, directory, true); err != nil {
		return nil, err
	}
	if def := c.CachedDefault(); def != nil {
		return def, nil
	}
	return nil, fmt.Errorf("cannot determine upstream default model")
}

// CachedDefault 返回缓存里的默认模型（可能为 nil）。
func (c *Cache) CachedDefault() *upstream.OCModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.def
}
