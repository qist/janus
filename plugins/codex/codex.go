// Package codex contributes the OpenAI Codex (ChatGPT plan) login flow.
package codex

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "codex",
		Name:      "OpenAI Codex",
		Sub:       "ChatGPT plan sign-in (authorization code + PKCE)",
		Essential: true,
		Defaults: map[string]any{
			"client_id":     "app_EMoamEEZ73f0CkXaXp7hrann",
			"authorize_url": "https://auth.openai.com/oauth/authorize",
			"token_url":     "https://auth.openai.com/oauth/token",
			"redirect_uri":  "http://localhost:1455/auth/callback",
			"scopes":        []string{"openid", "profile", "email", "offline_access"},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			return ctx.OAuthLogin("openai-codex", oauth.Config{
				ClientID:         ctx.String("client_id", ""),
				AuthorizeURL:     ctx.String("authorize_url", ""),
				TokenURL:         ctx.String("token_url", ""),
				RedirectURI:      ctx.String("redirect_uri", ""),
				Scopes:           ctx.Strings("scopes", nil),
				VerifierBytes:    64,
				VerifierEncoding: "hex",
				TokenStyle:       "form",
			})
		},
	})
}
