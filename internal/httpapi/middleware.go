package httpapi

import (
	"net/http"

	"aichatdeck/internal/model"
)

// RequireAuth is the entire auth model this milestone: a valid, active
// credential is required; there is no per-permission authorization on top
// of it (RFC-1600's Permission/Approval model is out of scope).
func (s *Server) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			s.writeErr(w, model.AccessDenied("missing bearer token"))
			return
		}
		agentID, _, found, err := s.Credentials.LookupAgentByToken(r.Context(), token)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !found {
			s.writeErr(w, model.AccessDenied("invalid or revoked credential"))
			return
		}
		next(w, r.WithContext(withAgentID(r.Context(), agentID)))
	}
}
