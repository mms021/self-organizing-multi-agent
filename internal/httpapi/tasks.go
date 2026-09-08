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
	if req.ProjectID != nil {
		project, full, err := s.projectFor(r, *req.ProjectID, agentID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !full {
			s.writeErr(w, model.AccessDenied("only a project member may create tasks in it"))
			return
		}
		if project.Status != model.ProjectActive {
			s.writeErr(w, model.Conflict("cannot create a task in an archived project"))
			return
		}
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
	if q.Has("project_id") {
		projectID := q.Get("project_id")
		filter.ProjectID = &projectID
	}
	if caps := q.Get("required_capabilities"); caps != "" {
		filter.RequiredCapabilities = strings.Split(caps, ",")
	}
	if tools := q.Get("required_tools"); tools != "" {
		filter.RequiredTools = strings.Split(tools, ",")
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

	// Listing with required_capabilities helps honest agents find suitable
	// work, but callers may omit that query parameter or call claim directly.
	// Check the registered profile again at the state-changing boundary.
	target, err := s.Tasks.Get(r.Context(), agentID, r.PathValue("task_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if target.ProjectID != nil {
		project, err := s.Projects.Get(r.Context(), *target.ProjectID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if project.Status != model.ProjectActive {
			s.writeErr(w, model.Conflict("cannot claim a task in an archived project"))
			return
		}
		mayWork, err := s.Members.CanRead(r.Context(), project.ProjectID, agentID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !mayWork {
			s.writeErr(w, model.AccessDenied("reviewer access is read-only and cannot claim tasks"))
			return
		}
	}
	claimer, err := s.Agents.Get(r.Context(), agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !agentMatchesTask(claimer.Capabilities, target.RequiredCapabilities) {
		s.writeErr(w, model.AccessDenied("agent lacks a required capability for this task"))
		return
	}
	if !agentMatchesTools(claimer.Tools, target.RequiredTools) {
		s.writeErr(w, model.AccessDenied("agent lacks a required tool for this task"))
		return
	}

	task, err := s.Tasks.Claim(r.Context(), target.TaskID, agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func agentMatchesTools(tools []model.Tool, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make([]string, 0, len(tools))
	for _, tool := range tools {
		have = append(have, tool.Name)
	}
	return hasAnyName(have, required)
}

func hasAnyName(have, required []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, name := range have {
		set[name] = struct{}{}
	}
	for _, name := range required {
		if _, ok := set[name]; ok {
			return true
		}
	}
	return false
}

func (s *Server) handleHeartbeatTask(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	var req struct {
		ClaimID string `json:"claim_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if req.ClaimID == "" {
		s.writeErr(w, model.ValidationError("claim_id is required"))
		return
	}
	target, err := s.Tasks.Get(r.Context(), agentID, r.PathValue("task_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if target.ProjectID != nil {
		if err := s.requireProjectWriter(r, *target.ProjectID, agentID); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	task, err := s.Tasks.Heartbeat(r.Context(), target.TaskID, agentID, req.ClaimID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// agentMatchesTask accepts unqualified tasks for every agent. A qualified
// task needs at least one relevant declared capability, matching the list
// endpoint and RFC-1500's capability_fit model.
func agentMatchesTask(capabilities []model.Capability, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		have[capability.Name] = struct{}{}
	}
	for _, name := range required {
		if _, ok := have[name]; ok {
			return true
		}
	}
	return false
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
	// Verification records a state-changing judgement. A reviewer may inspect
	// its scoped task, but cannot alter it through a verdict.
	target, err := s.Tasks.Get(r.Context(), agentID, r.PathValue("task_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if target.ProjectID != nil {
		if err := s.requireProjectWriter(r, *target.ProjectID, agentID); err != nil {
			s.writeErr(w, err)
			return
		}
	}
	for _, artifactID := range req.Evidence {
		artifact, err := s.Artifacts.Get(r.Context(), agentID, artifactID)
		if err != nil || artifact.TaskID == nil || *artifact.TaskID != target.TaskID {
			s.writeErr(w, model.ValidationError("verification evidence must be attached to this task"))
			return
		}
	}

	task, verification, err := s.Tasks.Verify(r.Context(), target.TaskID, agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.VerifyResponse{Task: task, Verification: verification})
}

func (s *Server) handleClaimVerification(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	job, err := s.Tasks.ClaimVerification(r.Context(), agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}
