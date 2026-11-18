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

	pluginrt.ApplyManifests(pluginrt.Registered(), pluginrt.File{}, pluginrt.Targets{
		OAuth: registry, Devices: devices,
	})

	states := map[string]pluginrt.State{}
	for _, status := range pluginrt.LastStatus() {
		states[status.ID] = status.State
	}
	for _, id := range []string{"codex", "claude", "copilot"} {
		if states[id] != pluginrt.StateActive {
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

	for id, want := range map[string]string{
		"openai-codex":   "oauth",
		"claude-code":    "oauth",
		"github-copilot": "device_code",
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
