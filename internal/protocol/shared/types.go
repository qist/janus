package shared

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------- 协议无关的线格式类型 ----------
//
// 三套对外 API（Chat Completions / Responses / Anthropic Messages）最终都要落到
// 同一套内部消息形状上。这里定义的就是那套形状：它是协议层的公共词汇，
// 不属于任何一个具体 API。

type ToolSpec struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 是 OpenAI 的 function 定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ChatMessage struct {
	Role             string         `json:"role"`
	Content          MessageContent `json:"content"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function *FunctionCall `json:"function,omitempty"`
	// Index 只在流式 delta 里出现（OpenAI 用它对应并发工具调用的序号）。
	// 非流式响应的 message.tool_calls 不带 index，所以用指针区分"未设置"。
	Index *int `json:"index,omitempty"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// MessageContent 兼容 content 为 string | array | null 三种形态。
type MessageContent struct {
	Text    string
	Parts   []ContentPart
	IsArray bool
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func (m *MessageContent) UnmarshalJSON(b []byte) error {
	*m = MessageContent{}
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.Text = s
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(b, &parts); err == nil {
		m.Parts = parts
		m.IsArray = true
		m.Text = partsText(parts)
		return nil
	}
	// 未知形态：整段当作文本保留，绝不丢数据。
	m.Text = strings.TrimSpace(string(b))
	return nil
}

func (m MessageContent) MarshalJSON() ([]byte, error) {
	if m.IsArray {
		return json.Marshal(m.Parts)
	}
	return json.Marshal(m.Text)
}

func partsText(parts []ContentPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// Images 提取全部图片 data URI。
func (m MessageContent) Images() []string {
	var out []string
	for _, p := range m.Parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			out = append(out, p.ImageURL.URL)
		}
	}
	return out
}

type Usage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *TokenDetails      `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionDetails `json:"completion_tokens_details,omitempty"`

	// DeepSeek 风格的缓存明细（不少国产客户端/网关用它统计缓存命中率）。
	// 语义：prompt_tokens = hit + miss。
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
}

type TokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// CacheCreationTokens 为写入缓存的 token（上游 cache.write）。OpenAI 无此字段，
	// 但网关普遍透出，标准客户端会忽略。
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type CompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------- ID 生成 ----------

// NewID 生成响应 ID 的随机部分：128 位密码学随机，32 个十六进制字符。
//
// 刻意不用时间/进程/自增拼出来的伪随机：这个值会随响应返回给客户端，可能被
// 当作幂等键、日志关联键甚至缓存键。可预测或在高并发/多进程下有碰撞风险都不合适。
// 16 字节 crypto/rand 的碰撞概率可以忽略（生日界 2^64 量级）。
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Linux 上基本不会失败。真失败也不能让请求挂掉，
		// 退化成"时间 + 进程 + 递增"，保证唯一性，牺牲不可预测性。
		return FallbackID()
	}
	return hex.EncodeToString(b[:])
}

var (
	fallbackMu  sync.Mutex
	fallbackSeq uint64
)

// FallbackID 是 crypto/rand 不可用时的退化 ID：时间 + 进程 + 递增序号。
// 牺牲不可预测性，只保证唯一性。
func FallbackID() string {
	fallbackMu.Lock()
	fallbackSeq++
	n := fallbackSeq
	fallbackMu.Unlock()
	return fmt.Sprintf("%016x%016x", uint64(time.Now().UnixNano()),
		uint64(os.Getpid())<<32|(n&0xffffffff))
}
