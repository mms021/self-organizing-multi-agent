// Package httpapi is the REST surface: routing, auth middleware, and
// handlers for the RFC-1700 endpoints implemented this milestone.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

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
	Knowledge   *store.KnowledgeStore
	Artifacts   *store.ArtifactStore
	Projects    *store.ProjectStore
	Members     *store.MembershipStore
	Bus         bus.Bus
	DB          *sql.DB
	RedisPinger Pinger
	Logger      *log.Logger // nil silences server-side logging (tests)

	// Rate-limit state, per router (see NewRouter).
	registerLimit *limiter
	agentLimit    *limiter
}

func NewRouter(s *Server) http.Handler {
	s.registerLimit = newLimiter(registerPerMinute, time.Minute)
	s.agentLimit = newLimiter(agentPerMinute, time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest", s.handleManifest)
	mux.HandleFunc("GET /discovery", s.handleDiscovery)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /donate", s.handleDonate)

	mux.HandleFunc("POST /agents/register", s.rateLimit(s.registerLimit, clientIP, s.handleRegister))
	mux.HandleFunc("GET /agents/{agent_id}", s.RequireAuth(s.handleGetAgent))

	mux.HandleFunc("POST /messages", s.RequireAuth(s.handlePostMessage))
	mux.HandleFunc("GET /messages", s.RequireAuth(s.handleListMessages))

	mux.HandleFunc("POST /tasks", s.RequireAuth(s.handleCreateTask))
	mux.HandleFunc("GET /tasks", s.RequireAuth(s.handleListTasks))
	mux.HandleFunc("GET /tasks/{task_id}", s.RequireAuth(s.handleGetTask))
	mux.HandleFunc("POST /tasks/{task_id}/claim", s.RequireAuth(s.handleClaimTask))
	mux.HandleFunc("POST /tasks/{task_id}/verify", s.RequireAuth(s.handleVerifyTask))

	// Shared memory (RFC-1300) and its project scoping (RFC-1250).
	mux.HandleFunc("POST /memory/entries", s.RequireAuth(s.handleCreateKnowledge))
	mux.HandleFunc("GET /memory/entries/{knowledge_id}", s.RequireAuth(s.handleGetKnowledge))
	mux.HandleFunc("POST /memory/entries/{knowledge_id}/review", s.RequireAuth(s.handleReviewKnowledge))
	mux.HandleFunc("GET /memory/search", s.RequireAuth(s.handleSearchKnowledge))
	mux.HandleFunc("POST /artifacts", s.RequireAuth(s.handleCreateArtifact))
	mux.HandleFunc("GET /artifacts/{artifact_id}", s.RequireAuth(s.handleGetArtifact))
	mux.HandleFunc("POST /projects", s.RequireAuth(s.handleCreateProject))
	mux.HandleFunc("GET /projects/{project_id}", s.RequireAuth(s.handleGetProject))
	mux.HandleFunc("GET /discovery/projects", s.RequireAuth(s.handleDiscoverProjects))
	mux.HandleFunc("POST /projects/{project_id}/invite", s.RequireAuth(s.handleInvite))
	mux.HandleFunc("POST /projects/{project_id}/apply", s.RequireAuth(s.handleApply))
	mux.HandleFunc("GET /projects/{project_id}/members", s.RequireAuth(s.handleListMembers))
	mux.HandleFunc("POST /projects/{project_id}/members/{agent_id}/decide", s.RequireAuth(s.handleDecideMembership))
	mux.HandleFunc("DELETE /projects/{project_id}/members/{agent_id}", s.RequireAuth(s.handleRemoveMembership))
	mux.HandleFunc("POST /projects/{project_id}/members/{agent_id}/role", s.RequireAuth(s.handleSetRole))
	mux.HandleFunc("POST /projects/{project_id}/transfer", s.RequireAuth(s.handleTransfer))
	mux.HandleFunc("POST /projects/{project_id}/transfer/decide", s.RequireAuth(s.handleDecideTransfer))

	return s.recoverPanics(limitBodies(mux))
}

// maxBodyBytes caps every request body. Nothing downstream streams: each
// handler decodes the whole body into memory, and POST /agents/register is
// reachable without a credential — so without a cap one request can make the
// server allocate until it dies.
const maxBodyBytes = 1 << 20 // 1 MiB

func limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// recoverPanics keeps one bad request from tearing down the handler with no
// trace: the panic is logged with its stack and reported as a plain 500.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				traceID := idgen.New("trace")
				s.logf("panic [%s] %s %s: %v\n%s", traceID, r.Method, r.URL.Path, rec, debug.Stack())
				model.WriteError(w, traceID, model.Internal())
			}
		}()
		next.ServeHTTP(w, r)
	})
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
// that isn't a *model.APIError is an unexpected internal failure: it is
// logged in full server-side and reported to the client as a bare 500 with
// the trace_id. Internal error text (SQL strings, file paths) must not cross
// the trust boundary — RFC-1600 §2 "communication".
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	traceID := idgen.New("trace")
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		model.WriteError(w, traceID, apiErr)
		return
	}
	s.logf("internal error [%s]: %v", traceID, err)
	model.WriteError(w, traceID, model.Internal())
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger == nil {
		return
	}
	s.Logger.Printf(format, args...)
}

// decodeJSON rejects unknown fields — payloads MUST match their schema
// (RFC-0000 §7 normative language implies exactness, not best-effort parsing).
func decodeJSON(r *http.Request, dst any) *model.APIError {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// A body over the cap (limitBodies) is a client mistake with a fixed
		// message: the reader's own error text says nothing useful.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return model.ValidationError("request body too large")
		}
		return model.ValidationError("invalid JSON body: " + err.Error())
	}
	return nil
}
