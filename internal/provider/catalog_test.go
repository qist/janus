package provider

import "testing"

func TestChannelCatalogIsConsistent(t *testing.T) {
	seen := make(map[string]bool)
	for _, channel := range Channels() {
		if channel.ID == "" || channel.Name == "" {
			t.Fatalf("channel missing id or name: %+v", channel)
		}
		if seen[channel.ID] {
			t.Fatalf("duplicate channel id %q", channel.ID)
		}
		seen[channel.ID] = true

		switch channel.Category {
		case ChannelVendor, ChannelRelay, ChannelSubscription, ChannelLocal:
		default:
			t.Fatalf("channel %q has unknown category %q", channel.ID, channel.Category)
		}
		if !channel.Kind.Valid() {
			t.Fatalf("channel %q has invalid kind %q", channel.ID, channel.Kind)
		}
		if len(channel.Auth) == 0 {
			t.Fatalf("channel %q has no auth method", channel.ID)
		}
		if len(channel.Protocols) == 0 {
			t.Fatalf("channel %q has no protocol", channel.ID)
		}
		if channel.ID != "custom" && channel.BaseURL == "" {
			t.Fatalf("channel %q has no base url", channel.ID)
		}
	}
	if !seen["openai"] || !seen["deepseek"] || !seen["openrouter"] {
		t.Fatalf("catalogue is missing core entries: %v", seen)
	}
}

func TestChannelLookup(t *testing.T) {
	if _, ok := ChannelByID(" DEEPSEEK "); !ok {
		t.Fatal("lookup should be case-insensitive and trim spaces")
	}
	if _, ok := ChannelByID("nope"); ok {
		t.Fatal("unknown channel should not resolve")
	}
	if !KnownChannel("") {
		t.Fatal("empty channel is allowed for legacy providers")
	}
	if KnownChannel("nope") {
		t.Fatal("unknown channel should be rejected")
	}
}

// TestCopilotChannelCarriesIDHeaders guards the header set the Copilot API
// rejects requests without. They must live on the channel, because a provider
// created from it is not otherwise told which IDE it is imitating.
func TestCopilotChannelCarriesIDHeaders(t *testing.T) {
	channel, ok := ChannelByID("github-copilot")
	if !ok {
		t.Fatal("github-copilot channel missing")
	}
	for _, name := range []string{"Editor-Version", "Copilot-Integration-Id", "User-Agent"} {
		if channel.Headers[name] == "" {
			t.Fatalf("github-copilot is missing %s", name)
		}
	}

	provider := Provider{ID: "gh", Channel: "github-copilot", Headers: map[string]string{
		"Editor-Version": "vscode/9.9.9",
	}}
	merged := requestHeaders(provider)
	if merged["Editor-Version"] != "vscode/9.9.9" {
		t.Fatalf("provider override ignored: %v", merged)
	}
	if merged["Copilot-Integration-Id"] == "" {
		t.Fatalf("channel headers dropped: %v", merged)
	}
	if got := requestHeaders(Provider{Channel: "deepseek"}); got != nil {
		t.Fatalf("unexpected headers for a plain channel: %v", got)
	}
}
