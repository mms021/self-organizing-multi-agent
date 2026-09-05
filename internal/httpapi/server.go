// Package httpapi is the REST surface: routing, auth middleware, and
// handlers for the RFC-1700 endpoints implemented this milestone.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
	"aichatdeck/internal/store"
)

// Pinger is satisfied by bus.Redis; tests can leave RedisPinger nil.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Server struct {
	Agents      *store.AgentStore
	Credentials *store.CredentialStore
	Tasks       *store.TaskStore
	Messages    *store.MessageStore
	Bus         bus.Bus
	DB          *sql.DB
	RedisPinger Pinger
}

func NewRouter(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest", s.handleManifest)
	mux.HandleFunc("GET /discovery", s.handleDiscovery)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /donate", s.handleDonate)

	mux.HandleFunc("POST /agents/register", s.handleRegister)
	mux.HandleFunc("GET /agents/{agent_id}", s.RequireAuth(s.handleGetAgent))

	mux.HandleFunc("POST /messages", s.RequireAuth(s.handlePostMessage))
	mux.HandleFunc("GET /messages", s.RequireAuth(s.handleListMessages))

	mux.HandleFunc("POST /tasks", s.RequireAuth(s.handleCreateTask))
	mux.HandleFunc("GET /tasks", s.RequireAuth(s.handleListTasks))
	mux.HandleFunc("GET /tasks/{task_id}", s.RequireAuth(s.handleGetTask))
	mux.HandleFunc("POST /tasks/{task_id}/claim", s.RequireAuth(s.handleClaimTask))
	mux.HandleFunc("POST /tasks/{task_id}/verify", s.RequireAuth(s.handleVerifyTask))

	return mux
}

// --- shared helpers ---

type ctxKey int

const agentIDKey ctxKey = iota

func withAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, agentIDKey, id)
}

// AgentIDFromContext returns the authenticated agent set by RequireAuth.
func AgentIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(agentIDKey).(string)
	return v, ok
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if tok == "" {
		return "", false
	}
	return tok, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps a store/model error to the RFC-1000 §8 error body. Anything
// that isn't a *model.APIError is an unexpected internal failure (500).
func writeErr(w http.ResponseWriter, err error) {
	traceID := idgen.New("trace")
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		model.WriteError(w, traceID, apiErr)
		return
	}
	model.WriteError(w, traceID, &model.APIError{ErrorCode: "internal", Message: err.Error(), Retryable: true})
}

// decodeJSON rejects unknown fields — payloads MUST match their schema
// (RFC-0000 §7 normative language implies exactness, not best-effort parsing).
func decodeJSON(r *http.Request, dst any) *model.APIError {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return model.ValidationError("invalid JSON body: " + err.Error())
	}
	return nil
}
