package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestIndexServesConsole(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", resp.Header.Get("Cache-Control"))
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if !strings.Contains(body, "Janus") || !strings.Contains(body, "/ui/assets/") {
		t.Fatalf("body = %s", body)
	}
}

// The hashed asset referenced by index.html must be served and cacheable.
func TestHashedAsset(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	index, err := srv.Client().Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/: %v", err)
	}
	raw, _ := io.ReadAll(index.Body)
	index.Body.Close()

	match := regexp.MustCompile(`src="(/ui/assets/[^"]+\.js)"`).FindStringSubmatch(string(raw))
	if len(match) != 2 {
		t.Fatalf("no hashed js asset in index: %s", raw)
	}

	resp, err := srv.Client().Get(srv.URL + match[1])
	if err != nil {
		t.Fatalf("GET %s: %v", match[1], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d", resp.StatusCode)
	}
	if cache := resp.Header.Get("Cache-Control"); !strings.Contains(cache, "immutable") {
		t.Fatalf("asset cache-control = %q", cache)
	}
}

func TestUIRedirect(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatalf("GET /ui: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("location = %q", loc)
	}
}

func TestUnknownAssetIsNotFound(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/ui/assets/missing.js")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
