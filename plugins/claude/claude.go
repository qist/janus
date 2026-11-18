// Package claude contributes the Claude Code (Anthropic plan) login flow.
package claude

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "claude",
		Name:      "Claude Code",
		Sub:       "Anthropic plan sign-in (authorization code + PKCE)",
		Essential: true,
		Defaults: map[string]any{
			"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
			"authorize_url": "https://claude.com/cai/oauth/authorize",
			"token_url":     "https://platform.claude.com/v1/oauth/token",
			"redirect_uri":  "https://platform.claude.com/oauth/code/callback",
			"scopes": []string{
				"org:create_api_key",
				"user:profile",
				"user:inference",
				"user:sessions:claude_code",
				"user:mcp_servers",
				"user:file_upload",
			},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			return ctx.OAuthLogin("claude-code", oauth.Config{
				ClientID:         ctx.String("client_id", ""),
				AuthorizeURL:     ctx.String("authorize_url", ""),
				TokenURL:         ctx.String("token_url", ""),
				RedirectURI:      ctx.String("redirect_uri", ""),
				Scopes:           ctx.Strings("scopes", nil),
				VerifierBytes:    32,
				VerifierEncoding: "base64url",
				TokenStyle:       "json",
			})
		},
	})
}
