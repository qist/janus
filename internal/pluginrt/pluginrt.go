// Package pluginrt runs janus's built-in provider plugins: the login flows a
// deployment may or may not want, each compiled in as a package under plugins/
// that registers a Manifest. The runtime reads a small config file (enable,
// disable, override parameters) and applies the active plugins in a stable
// order. One that fails does not stop the others.
//
// This mirrors magpie's plugin model: plugins are compiled in, registration
// happens from init(), and the runtime owns the state. It is deliberately not a
// code-loading mechanism — nothing here executes an operator-supplied binary.
package pluginrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"janus/internal/oauth"
	"janus/internal/provider"
)

// API is the host API version plugins are written against.
const API = "1.0"

// Manifest is one compiled plugin: what it provides, its defaults, and Apply,
// which registers the channel capability and flows through the Ctx.
type Manifest struct {
	ID   string
	Name string
	Sub  string
	// Essential plugins stay on under the "essentials" preset. Everything is on
	// with the default "all" preset, including a fresh install.
	Essential bool
	// Defaults are the configuration values before plugins.json's rows.
	Defaults map[string]any
	Apply    func(*Ctx) error
}

// File is plugins.json as written.
type File struct {
	// Preset is "all" (default) or "essentials".
	Preset  string `json:"preset,omitempty"`
	Plugins []Row  `json:"plugins,omitempty"`
}

// Row is the operator's say about one plugin: only what they set.
type Row struct {
	ID      string         `json:"id"`
	Enabled *bool          `json:"enabled,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// State is where a plugin ended up.
type State string

const (
	StateOff    State = "off"
	StateActive State = "active"
	StateFailed State = "failed"
)

// Status is one plugin as this process applied it. Config values are not
// returned: they may contain a client id the operator would rather not print.
type Status struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Sub    string `json:"sub,omitempty"`
	State  State  `json:"state"`
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Targets are the registries a plugin writes into.
type Targets struct {
	OAuth   *oauth.Registry
	Devices *oauth.DeviceRegistry
	Polls   *oauth.PollRegistry
}

// Ctx is what Apply is handed. Everything registered through it is undone when
// the runtime closes.
type Ctx struct {
	ID      string
	Config  map[string]any
	targets Targets
	onClose []func()
}

// String reads a string setting.
func (c *Ctx) String(key, fallback string) string {
	if value, ok := c.Config[key].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

// Strings reads a list setting; JSON gives []any, code gives []string.
func (c *Ctx) Strings(key string, fallback []string) []string {
	switch typed := c.Config[key].(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return fallback
}

// OnClose runs f when the runtime closes.
func (c *Ctx) OnClose(f func()) { c.onClose = append(c.onClose, f) }

// Channel adds a channel or overrides a builtin entry.
func (c *Ctx) Channel(channel provider.Channel) error {
	if channel.ID == "" {
		return errors.New("plugin channel requires an id")
	}
	channel.Source = "plugin"
	return provider.RegisterChannel(channel)
}

// OAuthLogin marks an existing channel as having a working authorization-code
// flow and registers it. The channel keeps its catalogue metadata; the plugin
// only adds the capability, so a curated entry is not duplicated in code.
func (c *Ctx) OAuthLogin(channelID string, config oauth.Config) error {
	channel, ok := provider.ChannelByID(channelID)
	if !ok {
		return fmt.Errorf("oauth login: unknown channel %q", channelID)
	}
	channel.Login = "oauth"
	channel.Source = "plugin"
	if err := provider.RegisterChannel(channel); err != nil {
		return err
	}
	if c.targets.OAuth == nil {
		return errors.New("oauth registry is not configured")
	}
	config.Kind = channel.ID
	c.targets.OAuth.Set(config)
	return nil
}

// DeviceLogin is the device-code counterpart of OAuthLogin.
func (c *Ctx) DeviceLogin(channelID string, config oauth.DeviceConfig) error {
	channel, ok := provider.ChannelByID(channelID)
	if !ok {
		return fmt.Errorf("device login: unknown channel %q", channelID)
	}
	channel.Login = "device_code"
	channel.Source = "plugin"
	if err := provider.RegisterChannel(channel); err != nil {
		return err
	}
	if c.targets.Devices == nil {
		return errors.New("device registry is not configured")
	}
	config.Kind = channel.ID
	c.targets.Devices.Set(config)
	return nil
}

var (
	regMu    sync.Mutex
	registry = map[string]Manifest{}
)

// PollLogin registers a state+poll sign-in. It reuses the device endpoints in
// the console, so the UI needs no third mode.
func (c *Ctx) PollLogin(channelID string, config oauth.PollConfig) error {
	channel, ok := provider.ChannelByID(channelID)
	if !ok {
		return fmt.Errorf("poll login: unknown channel %q", channelID)
	}
	channel.Login = "poll"
	channel.Source = "plugin"
	if err := provider.RegisterChannel(channel); err != nil {
		return err
	}
	if c.targets.Polls == nil {
		return errors.New("poll registry is not configured")
	}
	config.Kind = channel.ID
	c.targets.Polls.Set(config)
	return nil
}

// Register adds a compiled plugin; packages call it from init. A duplicate id is
// a programming error, so it panics at startup rather than at request time.
func Register(manifest Manifest) {
	if strings.TrimSpace(manifest.ID) == "" {
		panic("pluginrt: a plugin with no id")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[manifest.ID]; dup {
		panic("pluginrt: two plugins " + manifest.ID)
	}
	registry[manifest.ID] = manifest
}

// Registered returns the compiled plugins, sorted by id.
func Registered() []Manifest {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Manifest, 0, len(registry))
	for _, manifest := range registry {
		out = append(out, manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Composed is one plugin after the three layers: default, preset, operator row.
type Composed struct {
	Manifest Manifest
	Enabled  bool
	Config   map[string]any
	Source   string
}

// Compose resolves the file against the manifests.
func Compose(manifests []Manifest, file File) []Composed {
	preset := strings.ToLower(strings.TrimSpace(file.Preset))
	if preset == "" {
		preset = "all"
	}
	rows := make(map[string]Row, len(file.Plugins))
	for _, row := range file.Plugins {
		rows[strings.ToLower(strings.TrimSpace(row.ID))] = row
	}

	out := make([]Composed, 0, len(manifests))
	for _, manifest := range manifests {
		enabled := true
		source := "default"
		if preset == "essentials" && !manifest.Essential {
			enabled = false
			source = "preset"
		}
		config := make(map[string]any, len(manifest.Defaults))
		for key, value := range manifest.Defaults {
			config[key] = value
		}
		if row, ok := rows[strings.ToLower(manifest.ID)]; ok {
			if row.Enabled != nil {
				enabled = *row.Enabled
			}
			source = "user"
			for key, value := range row.Config {
				config[key] = value
			}
		}
		out = append(out, Composed{Manifest: manifest, Enabled: enabled, Config: config, Source: source})
	}
	return out
}

// Load reads a plugins.json. A missing file is the zero File: every default.
func Load(path string) (File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return File{}, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var file File
	if err := json.Unmarshal(raw, &file); err != nil {
		return File{}, fmt.Errorf("%s does not parse: %w", path, err)
	}
	switch strings.ToLower(strings.TrimSpace(file.Preset)) {
	case "", "all", "essentials":
	default:
		return File{}, fmt.Errorf("%s: preset must be all or essentials", path)
	}
	for _, row := range file.Plugins {
		if strings.TrimSpace(row.ID) == "" {
			return File{}, fmt.Errorf("%s: a plugin row has no id", path)
		}
	}
	return file, nil
}

// Runtime is the outcome of one Apply.
type Runtime struct {
	statuses []Status
	closers  []func()
}

// Apply runs the compiled plugins with the file's settings.
func Apply(file File) *Runtime {
	return ApplyManifests(Registered(), file, Targets{
		OAuth:   oauth.NewRegistry(),
		Devices: oauth.NewDeviceRegistry(),
		Polls:   oauth.NewPollRegistry(),
	})
}

// ApplyManifests is Apply with explicit manifests and targets; tests and callers
// that already own the registries use it.
func ApplyManifests(manifests []Manifest, file File, targets Targets) *Runtime {
	runtime := &Runtime{}
	for _, composed := range Compose(manifests, file) {
		status := Status{
			ID:     composed.Manifest.ID,
			Name:   composed.Manifest.Name,
			Sub:    composed.Manifest.Sub,
			Source: composed.Source,
		}
		if !composed.Enabled {
			status.State = StateOff
			runtime.statuses = append(runtime.statuses, status)
			continue
		}
		ctx := &Ctx{ID: composed.Manifest.ID, Config: composed.Config, targets: targets}
		if err := safeApply(composed.Manifest, ctx); err != nil {
			status.State = StateFailed
			status.Error = err.Error()
			log.Printf("plugin %s failed: %v", composed.Manifest.ID, err)
		} else {
			status.State = StateActive
			runtime.closers = append(runtime.closers, ctx.onClose...)
		}
		runtime.statuses = append(runtime.statuses, status)
	}
	setLast(runtime)
	return runtime
}

func safeApply(manifest Manifest, ctx *Ctx) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	if manifest.Apply == nil {
		return errors.New("plugin has no Apply")
	}
	return manifest.Apply(ctx)
}

// Status returns one row per compiled plugin, in id order.
func (r *Runtime) Status() []Status { return r.statuses }

// Close runs the plugins' cleanup, in reverse order.
func (r *Runtime) Close() {
	for index := len(r.closers) - 1; index >= 0; index-- {
		r.closers[index]()
	}
	r.closers = nil
}

var (
	lastMu sync.Mutex
	last   *Runtime
)

func setLast(runtime *Runtime) {
	lastMu.Lock()
	last = runtime
	lastMu.Unlock()
}

// LastStatus returns the statuses from the most recent Apply, for the console.
func LastStatus() []Status {
	lastMu.Lock()
	defer lastMu.Unlock()
	if last == nil {
		return []Status{}
	}
	return append([]Status(nil), last.statuses...)
}
