package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCredentialEndpointsPreserveIdentity(t *testing.T) {
	srv := newTestServer(t)
	a := register(t, srv.URL)
	status, task := a.do(http.MethodPost, "/tasks", map[string]any{"objective": "retain my task"})
	if status != 201 {
		t.Fatalf("create %d %v", status, task)
	}
	status, out := a.do(http.MethodPost, "/credentials/rotate", nil)
	if status != 200 {
		t.Fatalf("rotate %d %v", status, out)
	}
	token, ok := out["token"].(string)
	if !ok || token == "" {
		t.Fatal("missing replacement")
	}
	if status, _ := a.do(http.MethodGet, "/tasks", nil); status != http.StatusForbidden {
		t.Fatalf("old key accepted %d", status)
	}
	a.token = token
	if status, out := a.do(http.MethodGet, "/tasks/"+task["task_id"].(string), nil); status != 200 || out["created_by"] != task["created_by"] {
		t.Fatalf("identity/task changed %d %v", status, out)
	}
	if status, out := a.do(http.MethodPost, "/credentials/revoke", nil); status != 200 {
		t.Fatalf("revoke %d %v", status, out)
	}
	if status, _ := a.do(http.MethodGet, "/tasks", nil); status != http.StatusForbidden {
		t.Fatalf("revoked key accepted %d", status)
	}
}

func TestCredentialEndpointsRequireAuthAndDoNotCacheSecrets(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/credentials/rotate", "/credentials/revoke"} {
		for _, token := range []string{"", "invalid-token"} {
			req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s accepted invalid credential: %d", path, resp.StatusCode)
			}
		}
	}
	a := register(t, srv.URL)
	other := register(t, srv.URL)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/credentials/rotate", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("rotation response status=%d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["token"] == a.token || out["token"] == "" || out["token"] == nil {
		t.Fatal("invalid replacement token")
	}
	if status, _ := other.do(http.MethodGet, "/tasks", nil); status != 200 {
		t.Fatalf("rotation affected other agent: %d", status)
	}
	if status, _ := a.do(http.MethodPost, "/credentials/revoke", nil); status != http.StatusForbidden {
		t.Fatalf("old key revoked replacement: %d", status)
	}
}
