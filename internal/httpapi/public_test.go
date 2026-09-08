package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPublicDiscoveryPagesContainNoRuntimeData(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/", "/robots.txt", "/sitemap.xml", "/llms.txt"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned an empty document", path)
		}
		if strings.Contains(string(body), "credential.token") || strings.Contains(string(body), "aichatdeck.db") {
			t.Errorf("GET %s exposed runtime data", path)
		}
	}
}

func TestUnknownPublicPathIsNotLandingPage(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/not-a-public-page")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
