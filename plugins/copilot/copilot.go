// Package copilot contributes the GitHub Copilot device-code login flow.
package copilot

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "copilot",
		Name:      "GitHub Copilot",
		Sub:       "GitHub device sign-in, exchanged for a short-lived Copilot token",
		Essential: true,
		Defaults: map[string]any{
			"client_id":  "Iv1.b507a08c87ecfe98",
			"device_url": "https://github.com/login/device/code",
			"token_url":  "https://github.com/login/oauth/access_token",
			"scopes":     []string{"read:user"},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			return ctx.DeviceLogin("github-copilot", oauth.DeviceConfig{
				ClientID:    ctx.String("client_id", ""),
				DeviceURL:   ctx.String("device_url", ""),
				TokenURL:    ctx.String("token_url", ""),
				Scopes:      ctx.Strings("scopes", nil),
				ExchangeURL: "https://api.github.com/copilot_internal/v2/token",
				ExchangeHeaders: map[string]string{
					"Editor-Version":         "vscode/1.95.0",
					"Copilot-Integration-Id": "vscode-chat",
					"User-Agent":             "GitHubCopilotChat/0.23.2",
				},
				UserURL: "https://api.github.com/user",
			})
		},
	})
}
