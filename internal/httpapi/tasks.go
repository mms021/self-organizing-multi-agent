package httpapi

import (
	"net/http"
	"strings"

	"aichatdeck/internal/model"
)

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var req model.CreateTaskRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	task, err := s.Tasks.Create(r.Context(), agentID, req, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.TaskFilter{
		Status: q.Get("status"),
		Cursor: q.Get("cursor"),
	}
	if caps := q.Get("required_capabilities"); caps != "" {
		filter.RequiredCapabilities = strings.Split(caps, ",")
	}

	agentID, _ := AgentIDFromContext(r.Context())
	tasks, next, err := s.Tasks.List(r.Context(), agentID, filter)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if tasks == nil {
		tasks = []model.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "next_cursor": next})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	task, err := s.Tasks.Get(r.Context(), agentID, r.PathValue("task_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	task, err := s.Tasks.Claim(r.Context(), r.PathValue("task_id"), agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleVerifyTask(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var req model.VerifyRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	task, verification, err := s.Tasks.Verify(r.Context(), r.PathValue("task_id"), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.VerifyResponse{Task: task, Verification: verification})
}
