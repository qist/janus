package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestParseCIDRList(t *testing.T) {
	got := ParseCIDRList(" 127.0.0.1/32 , 10.0.0.0/8 ,::1 , bad ,")
	want := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestClientIPIgnoresXFFByDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")

	if got := ClientIP(r, false); got != "10.0.0.1" {
		t.Fatalf("got %q", got)
	}
	if got := ClientIP(r, true); got != "203.0.113.9" {
		t.Fatalf("got %q", got)
	}
}

func TestBearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer sk-abc")
	if got := Bearer(r); got != "sk-abc" {
		t.Fatalf("got %q", got)
	}
	r.Header.Del("Authorization")
	if got := Bearer(r); got != "" {
		t.Fatalf("got %q", got)
	}
}
