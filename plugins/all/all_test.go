package all

import (
	"testing"

	"janus/internal/oauth"
	"janus/internal/pluginrt"
	"janus/internal/provider"
)

// TestCompiledPluginsProvideLogins is the contract of this package: importing it
// must make the built-in sign-in flows available, and the catalogue entries must
// advertise them.
func TestCompiledPluginsProvideLogins(t *testing.T) {
	registry := oauth.NewRegistry()
	devices := oauth.NewDeviceRegistry()
	polls := oauth.NewPollRegistry()

	pluginrt.ApplyManifests(pluginrt.Registered(), pluginrt.File{}, pluginrt.Targets{
		OAuth: registry, Devices: devices, Polls: polls,
	})

	states := map[string]pluginrt.State{}
	for _, status := range pluginrt.LastStatus() {
		states[status.ID] = status.State
	}
	for _, id := range []string{"codex", "claude", "copilot", "grok", "workbuddy"} {
		if states[id] != pluginrt.StateActive {
			t.Fatalf("plugin %s state = %q", id, states[id])
		}
	}
	// The Google flows need a client secret; without one they must say so
	// instead of failing the whole startup.
	for _, id := range []string{"gemini", "antigravity"} {
		if states[id] != pluginrt.StateNeedsConfig {
			t.Fatalf("plugin %s state = %q", id, states[id])
		}
	}

	if config, ok := registry.Get("openai-codex"); !ok || config.ClientID == "" {
		t.Fatalf("codex flow = %+v (ok=%v)", config, ok)
	}
	if config, ok := registry.Get("claude-code"); !ok || config.ClientID == "" {
		t.Fatalf("claude flow = %+v (ok=%v)", config, ok)
	}
	if config, ok := devices.Get("github-copilot"); !ok || config.ClientID == "" {
		t.Fatalf("copilot flow = %+v (ok=%v)", config, ok)
	}
	if config, ok := registry.Get("grok"); !ok || config.ClientID == "" || len(config.Scopes) == 0 {
		t.Fatalf("grok flow = %+v (ok=%v)", config, ok)
	}
	if config, ok := polls.Get("workbuddy"); !ok || config.TokenURL == "" {
		t.Fatalf("workbuddy flow = %+v (ok=%v)", config, ok)
	}

	for id, want := range map[string]string{
		"openai-codex":   "oauth",
		"claude-code":    "oauth",
		"github-copilot": "device_code",
		"workbuddy":      "poll",
		"grok":           "oauth",
	} {
		channel, ok := provider.ChannelByID(id)
		if !ok {
			t.Fatalf("channel %s missing from the catalogue", id)
		}
		if channel.Login != want {
			t.Fatalf("channel %s login = %q, want %q", id, channel.Login, want)
		}
	}
}

// TestGooglePluginsActivateWithSecret proves needs_config is not a dead end:
// supplying the secret turns the flow on.
func TestGooglePluginsActivateWithSecret(t *testing.T) {
	registry := oauth.NewRegistry()
	pluginrt.ApplyManifests(pluginrt.Registered(), pluginrt.File{Plugins: []pluginrt.Row{
		{ID: "gemini", Config: map[string]any{"client_secret": "secret"}},
		{ID: "antigravity", Config: map[string]any{"client_secret": "secret"}},
	}}, pluginrt.Targets{OAuth: registry, Devices: oauth.NewDeviceRegistry(), Polls: oauth.NewPollRegistry()})

	for _, channel := range []string{"google-code-assist", "antigravity"} {
		config, ok := registry.Get(channel)
		if !ok {
			t.Fatalf("%s flow missing", channel)
		}
		if config.ClientSecret != "secret" || len(config.Scopes) == 0 {
			t.Fatalf("%s config = %+v", channel, config)
		}
		if config.ExtraAuthParams["access_type"] != "offline" {
			t.Fatalf("%s extra params = %v", channel, config.ExtraAuthParams)
		}
	}
}
