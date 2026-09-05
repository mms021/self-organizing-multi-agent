package httpapi

import (
	"net/http"

	"aichatdeck/internal/model"
)

// handleRegister implements RFC-1400 §7: no valid credential -> new agent +
// minted credential; valid credential -> idempotent re-register that applies
// the newly-submitted profile and does not mint a new token.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req model.RegisterRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		writeErr(w, aerr)
		return
	}

	if token, ok := bearerToken(r); ok {
		agentID, credentialID, found, err := s.Credentials.LookupAgentByToken(r.Context(), token)
		if err != nil {
			writeErr(w, err)
			return
		}
		if found {
			agent, err := s.Agents.Update(r.Context(), agentID, req)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, model.RegisterResponse{
				Agent:      agent,
				Credential: model.CredentialOut{CredentialID: credentialID}, // token never re-echoed
			})
			return
		}
		// Invalid/expired credential: fall through and register as new.
	}

	agent, err := s.Agents.Create(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	cred, err := s.Credentials.Issue(r.Context(), agent.AgentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, model.RegisterResponse{Agent: agent, Credential: cred})
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.Agents.Get(r.Context(), r.PathValue("agent_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}
