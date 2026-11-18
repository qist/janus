// Package antigravity contributes the Google Antigravity sign-in.
package antigravity

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "antigravity",
		Name:      "Google Antigravity",
		Sub:       "Google OAuth for the Antigravity client (same quota as Code Assist)",
		Essential: false,
		Defaults: map[string]any{
			"client_id":     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
			"client_secret": "",
			"authorize_url": "https://accounts.google.com/o/oauth2/v2/auth",
			"token_url":     "https://oauth2.googleapis.com/token",
			"redirect_uri":  "http://localhost:8085/callback",
			"scopes": []string{
				"https://www.googleapis.com/auth/cloud-platform",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
				"https://www.googleapis.com/auth/cclog",
				"https://www.googleapis.com/auth/experimentsandconfigs",
			},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			try := func(channelID string) error {
				secret := ctx.String("client_secret", "")
				if secret == "" {
					return errMissingSecret()
				}
				return ctx.OAuthLogin(channelID, oauth.Config{
					ClientID:         ctx.String("client_id", ""),
					ClientSecret:     secret,
					AuthorizeURL:     ctx.String("authorize_url", ""),
					TokenURL:         ctx.String("token_url", ""),
					RedirectURI:      ctx.String("redirect_uri", ""),
					Scopes:           ctx.Strings("scopes", nil),
					VerifierBytes:    32,
					VerifierEncoding: "base64url",
					TokenStyle:       "form",
					ExtraAuthParams: map[string]string{
						"access_type": "offline",
						"prompt":      "consent",
					},
				})
			}
			return try("antigravity")
		},
	})
}
