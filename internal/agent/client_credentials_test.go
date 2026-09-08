package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aichatdeck/internal/model"
)

func TestRegisterNeverSilentlyReplacesSavedIdentity(t *testing.T) {
	for _, scenario := range []string{"revoked", "unavailable", "new-identity", "replacement-key", "same-identity"} {
		t.Run(scenario, func(t *testing.T) {
			registrations := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer old-key" {
					t.Error("saved key missing")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/tasks":
					if scenario == "revoked" {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"error_code":"access_denied"}`)
						return
					}
					if scenario == "unavailable" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error_code":"unavailable"}`)
						return
					}
					fmt.Fprint(w, `{"tasks":[]}`)
				case "/agents/register":
					registrations++
					switch scenario {
					case "new-identity":
						fmt.Fprint(w, `{"agent_id":"different","credential":{}}`)
					case "replacement-key":
						fmt.Fprint(w, `{"agent_id":"original","credential":{"token":"new-key"}}`)
					default:
						fmt.Fprint(w, `{"agent_id":"original","credential":{}}`)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := NewClient(srv.URL)
			client.AgentID, client.Token = "original", "old-key"
			err := client.Register(context.Background(), model.RegisterRequest{})
			if (err == nil) != (scenario == "same-identity") {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
			if client.AgentID != "original" || client.Token != "old-key" {
				t.Fatal("client replaced saved identity or key")
			}
			if (scenario == "revoked" || scenario == "unavailable") && registrations != 0 {
				t.Fatal("registered after failed credential validation")
			}
		})
	}
}
