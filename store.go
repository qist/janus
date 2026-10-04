package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ConversationKey → OpenCode session 映射 + 历史快照。

type Conversation struct {
	Key string

	// mu 覆盖整个请求生命周期：同一会话的请求天然串行化，
	// 避免 prompt 在上游 busy 时静默排队导致串上下文。
	// 只保护 agent/model/directory 这些"请求期间不变"的字段。
	mu sync.Mutex

	// stateMu 保护 lastMessages / sessionID / lastResponse。
	// 必须独立于 mu：store.Acquire 要在拿到 mu 之前读 lastMessages 做前缀匹配，
	// store.GC 也要在不抢 mu 的前提下读 sessionID。用 mu 会导致持 s.mu 时
	// 阻塞在慢请求上，把整个 store 卡死。
	stateMu      sync.RWMutex
	lastMessages []ChatMessage // 上次成功发给上游的完整 messages
	sessionID    string
	lastResponse *ChatResponse // DiffNone 时直接复用

	agent     string
	model     OCModelRef
	directory string

	// 工具调用状态（只在请求路径上、持 conv.mu 时访问）
	mcpName      string         // 已注册的 MCP server 名；空=未注册
	toolsFP      string         // 已注册工具集的指纹，用于判断是否需要重注册
	toolSess     *toolSession   // 对应的工具上下文
	pendingTools []*pendingCall // 本轮已回给客户端、等待结果回填的调用
	toolsRegAt   time.Time      // 上次成功注册的时间（上游重启会丢注册，靠它续注册）

	lastActive atomic.Int64 // 毫秒，eviction 不需要抢锁读
	createdAt  time.Time
}

func (c *Conversation) touch() { c.lastActive.Store(time.Now().UnixMilli()) }
func (c *Conversation) idle() time.Duration {
	return time.Since(time.UnixMilli(c.lastActive.Load()))
}

func (c *Conversation) snapshotLast() []ChatMessage {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.lastMessages
}

func (c *Conversation) setLast(msgs []ChatMessage) {
	c.stateMu.Lock()
	c.lastMessages = msgs
	c.stateMu.Unlock()
}

func (c *Conversation) clearLast() {
	c.stateMu.Lock()
	c.lastMessages = nil
	c.stateMu.Unlock()
}

func (c *Conversation) snapshotSessionID() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.sessionID
}

func (c *Conversation) setSessionID(id string) {
	c.stateMu.Lock()
	c.sessionID = id
	c.stateMu.Unlock()
}

func (c *Conversation) snapshotResponse() *ChatResponse {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.lastResponse
}

func (c *Conversation) setResponse(r *ChatResponse) {
	c.stateMu.Lock()
	c.lastResponse = r
	c.stateMu.Unlock()
}

// lastMatches 判断 stored 是否是 incoming 的严格前缀（或为空）。
func (c *Conversation) lastMatches(incoming []ChatMessage) bool {
	stored := c.snapshotLast()
	if len(stored) == 0 {
		// 空会话（首 prompt 失败过）可被任意 incoming 复用，等价于新建
		return true
	}
	if len(incoming) < len(stored) {
		return false
	}
	return lcp(stored, incoming) == len(stored)
}

type Store struct {
	mu sync.Mutex
	// dir 是 key → 候选会话列表，不是 key → 单会话。
	// 因为指纹只覆盖 (system, directory)：真实客户端常不带 system 消息，
	// 两个不相关的话题会拿到同一个 key。若严格单映射，它们会互相挤掉对方的
	// 会话（实测 compat_test 每个用例都在互踢）。分桶后按历史前缀挑会话，
	// 不相关的话题各占一格，互不干扰。
	dir       map[string][]*Conversation
	log       *Logger
	ttl       time.Duration
	max       int // 全局会话数上限
	maxPerKey int // 单 key 候选数上限，防止滑动窗口客户端无限新建

	// orphans 收集被挤出索引的上游 sessionID，由 janitor 异步删除。
	// 带缓冲 + 满则丢，保证 evict 路径永不阻塞（它持着 s.mu）。
	orphans chan string
}

func NewStore(log *Logger, ttl time.Duration, max int) *Store {
	return &Store{
		dir:       map[string][]*Conversation{},
		log:       log,
		ttl:       ttl,
		max:       max,
		maxPerKey: 8,
		orphans:   make(chan string, 512),
	}
}

// ConversationKey 优先级：显式头 > 指纹。
// 指纹 = hash(首条 system + directory)，只是分桶依据，不保证唯一会话。
// ConversationKey 决定"哪些请求算同一条对话线"（分桶依据）。
//
// 优先级：
//  1. 显式会话头（X-Session-ID / X-OpenCode-Session）—— 最可靠
//  2. 指纹 sha256(system + user + directory)
//
// 纳入 OpenAI 标准的 `user` 字段：它按约定标识终端用户。不带上它的话，
// 同一目录、同一系统提示词的不同用户会共用同一个桶，桶会无谓地膨胀，
// 也可能在前缀恰好相同时串到别人的会话上。
//
// 注意 key 只决定"分桶"，桶内还要按历史前缀严格匹配（见 Acquire），
// 所以即使 key 相同，不相关话题也会各自独立。
func ConversationKey(explicit, system, user, directory string) string {
	if explicit != "" {
		return "x:" + explicit
	}
	h := sha256.Sum256([]byte(system + "\x00" + user + "\x00" + directory))
	return "f:" + hex.EncodeToString(h[:8])
}

// AcquireKey 按"精确的键"取会话，不做历史前缀匹配。
//
// 用于 Responses API 的 previous_response_id 链：一条链恰好一个会话，
// 每轮只把新增 input 作为 prompt 发出，因此不能走 Chat 那套 Diff 前缀匹配。
// 键统一带 "resp:" 前缀，与 Chat 的 x:/f: 键天然隔离。
func (s *Store) AcquireKey(key string) *Conversation {
	s.mu.Lock()
	bucket := s.dir[key]
	var found *Conversation
	if len(bucket) > 0 {
		found = bucket[0]
	} else {
		if len(s.dir) >= s.max {
			s.evictOldestLocked()
		}
		found = &Conversation{Key: key, createdAt: time.Now()}
		s.dir[key] = []*Conversation{found}
	}
	s.mu.Unlock()

	found.mu.Lock()
	found.touch()
	return found
}

// Acquire 在桶里挑一个「历史是 incoming 前缀」的会话，挑不到就新建。
// 返回后会话已加锁，调用方必须 defer Release。
//
// 只接受严格前缀匹配：stored 必须完整出现在 incoming 开头。这样 delta 永远是
// 纯增量，上游 session 里不会混入与客户端不一致的历史。客户端若改写/删减了
// 旧消息（lcp < len(stored)），一律视为新会话，宁可丢上下文也不污染。
//
// 注意：先放掉 s.mu 再拿 c.mu，否则一个慢请求会把整个 store 卡住。
func (s *Store) Acquire(key string, incoming []ChatMessage) *Conversation {
	// 空历史的会话「匹配一切」，并发首请求会同时选中同一个，然后在 c.mu 上排队，
	// 后来者才发现前缀对不上（此时只能靠 DiffReset 兜底，白多一轮）。
	// 所以拿到 c.mu 之后必须复核一次，对不上就回到桶里重挑。
	for attempt := 0; ; attempt++ {
		c := s.claim(key, incoming, attempt >= 8)
		if attempt < 8 && !c.lastMatches(incoming) {
			c.mu.Unlock()
			continue
		}
		c.touch()
		return c
	}
}

// claim 选一个候选（或新建）并返回已加锁的会话。forceNew 时无条件新建。
func (s *Store) claim(key string, incoming []ChatMessage, forceNew bool) *Conversation {
	s.mu.Lock()

	bucket := s.dir[key]
	var found *Conversation
	best := -1
	if !forceNew {
		for _, c := range bucket {
			if !c.lastMatches(incoming) {
				continue
			}
			n := lcp(c.snapshotLast(), incoming)
			if n > best {
				best, found = n, c
			}
		}
	}

	if found == nil {
		if len(s.dir) >= s.max {
			s.evictOldestLocked()
		}
		if len(bucket) >= s.maxPerKey {
			bucket = evictOldestInBucket(s, bucket)
		}
		found = &Conversation{Key: key, createdAt: time.Now()}
		bucket = append([]*Conversation{found}, bucket...)
		s.dir[key] = bucket
	}
	s.mu.Unlock()

	found.mu.Lock()
	return found
}

// markOrphan 把被挤出索引的会话登记为待删除，交 janitor 统一回收上游 session。
// 只读 stateMu（不碰 c.mu），因此在持 s.mu 时调用也不会卡住整个 store。
func (s *Store) markOrphan(c *Conversation) {
	sid := c.snapshotSessionID()
	if sid == "" {
		return
	}
	select {
	case s.orphans <- sid:
	default:
		// 缓冲满说明 janitor 跟不上，丢掉这一个，靠 TTL 兜底
	}
}

func evictOldestInBucket(s *Store, bucket []*Conversation) []*Conversation {
	// bucket[0] 最近活跃，淘汰末尾
	victim := bucket[len(bucket)-1]
	s.markOrphan(victim)
	return bucket[:len(bucket)-1]
}

func (s *Store) Release(c *Conversation) {
	c.touch()
	c.mu.Unlock()
}

// evictOldestLocked 只从索引移除，不删上游 session（由 janitor 按 TTL 统一回收）。
// lastActive 是 atomic，读它不需要抢 c.mu。
func (s *Store) evictOldestLocked() {
	var oldestKey string
	var bucket []*Conversation
	var victim *Conversation
	var oldest int64
	for k, b := range s.dir {
		for _, c := range b {
			la := c.lastActive.Load()
			if victim == nil || la < oldest {
				oldestKey, bucket, victim, oldest = k, b, c, la
			}
		}
	}
	if victim == nil {
		return
	}
	s.markOrphan(victim)
	rest := make([]*Conversation, 0, len(bucket))
	for _, c := range bucket {
		if c != victim {
			rest = append(rest, c)
		}
	}
	if len(rest) == 0 {
		delete(s.dir, oldestKey)
	} else {
		s.dir[oldestKey] = rest
	}
	s.log.Infof("store evicted conversation %s", oldestKey)
}

// Count 返回当前内存里的会话数（指标用）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.dir {
		n += len(b)
	}
	return n
}

// GC 清掉长期不活动的会话，返回其上游 sessionID 供删除。
// TryLock 保证不会卡在正在服务的请求上。
func (s *Store) GC() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var dead []string
	for k, bucket := range s.dir {
		rest := bucket[:0]
		for _, c := range bucket {
			if !c.mu.TryLock() {
				rest = append(rest, c)
				continue // 正在服务
			}
			if c.idle() > s.ttl {
				if id := c.snapshotSessionID(); id != "" {
					dead = append(dead, id)
				}
				c.mu.Unlock()
				continue
			}
			c.mu.Unlock()
			rest = append(rest, c)
		}
		if len(rest) == 0 {
			delete(s.dir, k)
		} else {
			s.dir[k] = rest
		}
	}
	return dead
}

// ---------- 历史差分 ----------

type DiffMode int

const (
	// DiffReset 客户端重置了上下文（或首条），必须重开会话
	DiffReset DiffMode = iota
	// DiffAppend 正常增量，只需把新增部分发给上游
	DiffAppend
	// DiffNone 内容完全一致，不需要发新 prompt
	DiffNone
)

// lcp 返回两条消息列表的最长公共前缀长度。
func lcp(a, b []ChatMessage) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && msgEqual(a[i], b[i]) {
		i++
	}
	return i
}

func msgEqual(a, b ChatMessage) bool {
	if a.Role != b.Role {
		return false
	}
	if a.Content.Text != b.Content.Text {
		return false
	}
	if a.Content.IsArray != b.Content.IsArray {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		af, bf := a.ToolCalls[i].Function, b.ToolCalls[i].Function
		if (af == nil) != (bf == nil) {
			return false
		}
		if af != nil && (af.Name != bf.Name || af.Arguments != bf.Arguments) {
			return false
		}
	}
	return true
}

// Diff 计算 incoming 相对 stored 的增量（DESIGN.md §5.3）。
//
//	stored 是 incoming 的前缀  → DiffAppend，发 incoming[len(stored):]
//	完全一致                   → DiffNone
//	其余（删改历史/首条不同）    → DiffReset
func Diff(stored, incoming []ChatMessage) (DiffMode, []ChatMessage) {
	if len(incoming) == 0 {
		return DiffNone, nil
	}
	if len(stored) == 0 {
		return DiffReset, incoming
	}

	common := lcp(stored, incoming)

	if common == len(stored) {
		if len(incoming) == len(stored) {
			return DiffNone, nil
		}
		return DiffAppend, incoming[common:]
	}
	// stored 比 incoming 长（客户端缩短了历史），或历史被改写。
	// 无法只发增量，重开会话最安全。
	return DiffReset, incoming
}

// firstSystem 取首条 system 内容（用于 key 指纹）。
func firstSystem(msgs []ChatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content.Text
		}
	}
	return ""
}

// firstUserTitle 取首条 user 文本作为会话标题。
func firstUserTitle(msgs []ChatMessage, max int) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.Join(strings.Fields(m.Content.Text), " ")
		if t == "" {
			continue
		}
		r := []rune(t)
		if len(r) > max {
			return string(r[:max]) + "…"
		}
		return t
	}
	return ""
}

// lastAssistant 取最后一条 assistant 的回复（DiffNone 时回填）。
func lastAssistant(msgs []ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Content.Text
		}
	}
	return ""
}

// cloneMessages 深拷贝，避免调用方后续改动污染快照。
func cloneMessages(in []ChatMessage) []ChatMessage {
	if len(in) == 0 {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		out := make([]ChatMessage, len(in))
		copy(out, in)
		return out
	}
	var out []ChatMessage
	if err := json.Unmarshal(b, &out); err != nil {
		out = make([]ChatMessage, len(in))
		copy(out, in)
	}
	return out
}
