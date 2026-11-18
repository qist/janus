// Package gemini contributes the Google Code Assist sign-in (Gemini CLI plan).
package gemini

import (
	"janus/internal/oauth"
	"janus/internal/pluginrt"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID:        "gemini",
		Name:      "Google Code Assist",
		Sub:       "Google OAuth for the Gemini CLI plan; requires a client secret",
		Essential: false,
		Defaults: map[string]any{
			"client_id":     "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com",
			"client_secret": "",
			"authorize_url": "https://accounts.google.com/o/oauth2/v2/auth",
			"token_url":     "https://oauth2.googleapis.com/token",
			"redirect_uri":  "https://codeassist.google.com/authcode",
			"scopes": []string{
				"https://www.googleapis.com/auth/cloud-platform",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
			},
		},
		Apply: func(ctx *pluginrt.Ctx) error {
			secret := ctx.String("client_secret", "")
			if secret == "" {
				// Google requires a secret for this client type; refusing early is
				// clearer than a 400 at the token endpoint.
				return errMissingSecret()
			}
			cfg := oauth.Config{
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
			}
			return ctx.OAuthLogin("google-code-assist", cfg)
		},
	})
}
