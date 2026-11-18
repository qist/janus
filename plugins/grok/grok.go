// Package grok contributes the xAI Grok subscription sign-in.
package grok

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "grok",
		Name:      "Grok (X Premium)",
		Sub:       "xAI OAuth; tokens are served by the Grok CLI proxy",
		Essential: false,
		Defaults: map[string]any{
			"client_id":     "b1a00492-073a-47ea-816f-4c329264a828",
			"authorize_url": "https://auth.x.ai/oauth2/authorize",
			"token_url":     "https://auth.x.ai/oauth2/token",
			"redirect_uri":  "http://127.0.0.1:56121/callback",
			"scopes": []string{
				"openid", "profile", "email", "offline_access",
				"grok-cli:access", "api:access",
			},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			return ctx.OAuthLogin("grok", oauth.Config{
				ClientID:         ctx.String("client_id", ""),
				AuthorizeURL:     ctx.String("authorize_url", ""),
				TokenURL:         ctx.String("token_url", ""),
				RedirectURI:      ctx.String("redirect_uri", ""),
				Scopes:           ctx.Strings("scopes", nil),
				VerifierBytes:    32,
				VerifierEncoding: "base64url",
				TokenStyle:       "form",
			})
		},
	})
}
