// export.go —— 供网关/控制面复用的导出接口。
//
// 重构后 WorkBuddy 不再是独立的「账号体系」，而是 providers/credentials 里的一种
// 渠道：provider.auth_kind == "workbuddy" 时，网关用这里的客户端发请求，
// 凭据就是 ParseCredential 认的那份明文 JSON。这里只做最小导出，
// 不改变 workbuddyClient 的内部行为。
package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
)

// Chat 发送一次 chat 请求（含 401 自动续期重试，续期成功走 onRefresh 回调）。
func (c *workbuddyClient) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	return c.chat(ctx, body)
}

// Refresh 用 refreshToken 换新的 accessToken（单飞）。
func (c *workbuddyClient) Refresh(ctx context.Context) error { return c.refresh(ctx) }

// SetOnRefresh 注册续期成功回调，用于把新凭据回写数据库。
func (c *workbuddyClient) SetOnRefresh(fn func(*Credential)) { c.onRefresh = fn }

// Credential 返回当前凭据快照。
func (c *workbuddyClient) Credential() Credential { return *c.credential() }

// ChatURL 返回 chat 端点的完整地址。
func (c *workbuddyClient) ChatURL() string { return c.credential().chatBase() + "/v2/chat/completions" }

// FetchCredits 拉取账号剩余积分（个人套餐列表 / CN 企业单周期配额）。
func (c *workbuddyClient) FetchCredits(ctx context.Context) (*Credits, error) {
	return c.fetchCredits(ctx)
}

// PrepareBody 规整请求体（强制 stream、developer→system、拍平 tool_choice 等）。
func PrepareBody(src []byte) []byte { return prepareBody(src) }

// CredentialFromSecret 把 providers/credentials 里的 secret 对象解析成凭据。
func CredentialFromSecret(secret map[string]any) (*Credential, error) {
	raw, err := json.Marshal(secret)
	if err != nil {
		return nil, err
	}
	return ParseCredential(string(raw))
}

// Secret 把凭据序列化成可入库的 secret 对象（与 ParseCredential 互逆）。
func (c Credential) Secret() map[string]any {
	raw, _ := json.Marshal(c)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
