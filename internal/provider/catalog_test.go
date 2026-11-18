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
