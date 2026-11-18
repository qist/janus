package pluginrt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"janus/internal/oauth"
	"janus/internal/provider"
)

func testChannel(t *testing.T, id string) provider.Channel {
	t.Helper()
	channel := provider.Channel{
		ID: id, Name: id, Category: provider.ChannelSubscription,
		Kind: provider.KindOpenAI, Protocols: []string{"chat"},
		Auth: []string{"oauth"}, BaseURL: "https://example.com/v1",
	}
	if err := provider.RegisterChannel(channel); err != nil {
		t.Fatalf("RegisterChannel: %v", err)
	}
	return channel
}

func oauthManifest(id, channelID, defaultClient string) Manifest {
	return Manifest{
		ID:        id,
		Name:      id,
		Essential: true,
		Defaults:  map[string]any{"client_id": defaultClient},
		Apply: func(ctx *Ctx) error {
			return ctx.OAuthLogin(channelID, oauth.Config{
				ClientID:     ctx.String("client_id", ""),
				AuthorizeURL: "https://example.com/authorize",
				TokenURL:     "https://example.com/token",
				RedirectURI:  "https://example.com/callback",
			})
		},
	}
}

func TestApplyRegistersLoginAndHonoursConfig(t *testing.T) {
	channelID := "test-plugin-login"
	testChannel(t, channelID)
	registry := oauth.NewRegistry()
	devices := oauth.NewDeviceRegistry()

	runtime := ApplyManifests([]Manifest{oauthManifest("login", channelID, "default-client")},
		File{Plugins: []Row{{ID: "login", Config: map[string]any{"client_id": "own-client"}}}},
		Targets{OAuth: registry, Devices: devices})

	statuses := runtime.Status()
	if len(statuses) != 1 || statuses[0].State != StateActive {
		t.Fatalf("statuses = %+v", statuses)
	}
	config, ok := registry.Get(channelID)
	if !ok || config.ClientID != "own-client" {
		t.Fatalf("flow = %+v (ok=%v)", config, ok)
	}
	channel, ok := provider.ChannelByID(channelID)
	if !ok || channel.Login != "oauth" {
		t.Fatalf("channel = %+v", channel)
	}
	runtime.Close()
}

func TestDisabledPluginStaysOff(t *testing.T) {
	channelID := "test-plugin-off"
	testChannel(t, channelID)
	registry := oauth.NewRegistry()
	off := false

	runtime := ApplyManifests([]Manifest{oauthManifest("off", channelID, "cid")},
		File{Plugins: []Row{{ID: "off", Enabled: &off}}},
		Targets{OAuth: registry})

	if statuses := runtime.Status(); statuses[0].State != StateOff {
		t.Fatalf("state = %s", statuses[0].State)
	}
	if _, ok := registry.Get(channelID); ok {
		t.Fatal("disabled plugin registered a flow")
	}
	// The catalogue entry keeps its metadata but has no one-click login.
	channel, _ := provider.ChannelByID(channelID)
	if channel.Login != "" {
		t.Fatalf("login = %q", channel.Login)
	}
}

func TestEssentialsPresetTurnsOffOptional(t *testing.T) {
	channelID := "test-plugin-optional"
	testChannel(t, channelID)
	optional := oauthManifest("optional", channelID, "cid")
	optional.Essential = false

	registry := oauth.NewRegistry()
	runtime := ApplyManifests([]Manifest{optional},
		File{Preset: "essentials"}, Targets{OAuth: registry})
	if statuses := runtime.Status(); statuses[0].State != StateOff || statuses[0].Source != "preset" {
		t.Fatalf("status = %+v", statuses[0])
	}
}

func TestFailingPluginDoesNotStopOthers(t *testing.T) {
	channelID := "test-plugin-survivor"
	testChannel(t, channelID)
	broken := Manifest{
		ID:    "broken",
		Name:  "broken",
		Apply: func(*Ctx) error { return errors.New("boom") },
	}
	panicking := Manifest{
		ID:    "panicking",
		Name:  "panicking",
		Apply: func(*Ctx) error { panic("kaboom") },
	}
	registry := oauth.NewRegistry()
	runtime := ApplyManifests([]Manifest{
		broken, panicking, oauthManifest("good", channelID, "cid"),
	}, File{}, Targets{OAuth: registry})

	states := map[string]State{}
	for _, status := range runtime.Status() {
		states[status.ID] = status.State
	}
	if states["broken"] != StateFailed || states["panicking"] != StateFailed {
		t.Fatalf("states = %v", states)
	}
	if states["good"] != StateActive {
		t.Fatalf("good plugin did not run: %v", states)
	}
	if _, ok := registry.Get(channelID); !ok {
		t.Fatal("good plugin registered nothing")
	}
}

func TestDeviceLoginPlugin(t *testing.T) {
	channelID := "test-plugin-device"
	testChannel(t, channelID)
	registry := oauth.NewRegistry()
	devices := oauth.NewDeviceRegistry()

	runtime := ApplyManifests([]Manifest{{
		ID: "device", Name: "device",
		Defaults: map[string]any{"client_id": "cid"},
		Apply: func(ctx *Ctx) error {
			return ctx.DeviceLogin(channelID, oauth.DeviceConfig{
				ClientID:  ctx.String("client_id", ""),
				DeviceURL: "https://example.com/device",
				TokenURL:  "https://example.com/token",
			})
		},
	}}, File{}, Targets{OAuth: registry, Devices: devices})

	if statuses := runtime.Status(); statuses[0].State != StateActive {
		t.Fatalf("status = %+v", statuses[0])
	}
	if config, ok := devices.Get(channelID); !ok || config.ClientID != "cid" {
		t.Fatalf("device flow = %+v", config)
	}
	channel, _ := provider.ChannelByID(channelID)
	if channel.Login != "device_code" {
		t.Fatalf("login = %q", channel.Login)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	if err := os.WriteFile(path, []byte(`{"preset":"essentials","plugins":[{"id":"codex","enabled":false}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.Preset != "essentials" || len(file.Plugins) != 1 {
		t.Fatalf("file = %+v", file)
	}

	// Missing file is the defaults, not an error.
	missing, err := Load(filepath.Join(dir, "absent.json"))
	if err != nil || len(missing.Plugins) != 0 {
		t.Fatalf("missing = %+v (%v)", missing, err)
	}

	if err := os.WriteFile(path, []byte(`{"preset":"some"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted an unknown preset")
	}
	if err := os.WriteFile(path, []byte(`{"plugins":[{"enabled":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted a row without an id")
	}
}

func TestUnknownChannelIsAnError(t *testing.T) {
	manifest := Manifest{
		ID: "missing", Name: "missing",
		Apply: func(ctx *Ctx) error {
			return ctx.OAuthLogin("no-such-channel", oauth.Config{ClientID: "x"})
		},
	}
	runtime := ApplyManifests([]Manifest{manifest}, File{}, Targets{OAuth: oauth.NewRegistry()})
	if runtime.Status()[0].State != StateFailed {
		t.Fatalf("status = %+v", runtime.Status()[0])
	}
}

var _ = context.Background
