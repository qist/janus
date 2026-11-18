// Package workbuddy contributes the Tencent CodeBuddy web sign-in.
package workbuddy

import (
	"strings"

	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "workbuddy",
		Name:      "WorkBuddy / CodeBuddy",
		Sub:       "Web sign-in with polling, then the vendor's own token refresh",
		Essential: false,
		Defaults: map[string]any{
			"base_url":       "https://copilot.tencent.com/v2",
			"state_path":     "/plugin/auth/state",
			"token_path":     "/plugin/auth/token",
			"refresh_path":   "/plugin/auth/token/refresh",
			"state_query":    "state",
			"refresh_header": "X-Refresh-Token",
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			base := strings.TrimRight(ctx.String("base_url", "https://copilot.tencent.com/v2"), "/")
			return ctx.PollLogin("workbuddy", oauth.PollConfig{
				StateURL:      base + ctx.String("state_path", "/plugin/auth/state"),
				TokenURL:      base + ctx.String("token_path", "/plugin/auth/token"),
				RefreshURL:    base + ctx.String("refresh_path", "/plugin/auth/token/refresh"),
				StateQuery:    ctx.String("state_query", "state"),
				RefreshHeader: ctx.String("refresh_header", "X-Refresh-Token"),
			})
		},
	})
}
