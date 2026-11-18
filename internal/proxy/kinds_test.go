package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/auth"
	"janus/internal/gateway"
)

func TestKeyKindGatingOnModelSurface(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"real","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer up.Close()

	client, err := gateway.New(gateway.Config{BaseURL: up.URL, Model: "real", APIKey: "up"})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	keys := auth.NewStaticKeys()
	keys.Add("admin-key", auth.Identity{AgentID: "admin", Kind: auth.KeyKindAdmin, Scopes: []auth.Scope{auth.ScopeAdmin}})
	keys.Add("client-key", auth.Identity{AgentID: "app", Kind: auth.KeyKindClient, Scopes: []auth.Scope{auth.ScopeChat}})
	keys.Add("tool-key", auth.Identity{AgentID: "tool", Kind: auth.KeyKindTool, Scopes: []auth.Scope{auth.ScopeChat}})

	srv := httptest.NewServer(New(map[string]*gateway.Client{"fast": client}, keys, nil).Handler())
	defer srv.Close()

	post := func(key string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"fast","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if status := post("admin-key"); status != http.StatusForbidden {
		t.Fatalf("admin key status = %d, want 403", status)
	}
	if status := post("tool-key"); status != http.StatusForbidden {
		t.Fatalf("tool key status = %d, want 403", status)
	}
	if status := post("client-key"); status != http.StatusOK {
		t.Fatalf("client key status = %d, want 200", status)
	}
}
