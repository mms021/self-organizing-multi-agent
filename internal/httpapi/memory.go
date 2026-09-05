package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"aichatdeck/internal/model"
)

func (s *Server) handleCreateKnowledge(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var req model.CreateKnowledgeRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	entry, err := s.Knowledge.Create(r.Context(), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleGetKnowledge(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	entry, err := s.Knowledge.Get(r.Context(), agentID, r.PathValue("knowledge_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleReviewKnowledge(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	entry, err := s.Knowledge.Review(r.Context(), agentID, r.PathValue("knowledge_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleSearchKnowledge is GET /memory/search (RFC-1300 §8).
//
// project_id semantics: omitted searches every scope; `project_id=` (empty)
// restricts to global knowledge; a project id returns that project's records
// plus global ones.
func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := model.KnowledgeFilter{
		Category: q.Get("category"),
		Status:   q.Get("status"),
		TaskID:   q.Get("task_id"),
		Query:    q.Get("q"),
		Cursor:   q.Get("cursor"),
	}
	if q.Has("project_id") {
		projectID := q.Get("project_id")
		filter.ProjectID = &projectID
	}
	if tags := q.Get("tags"); tags != "" {
		filter.Tags = strings.Split(tags, ",")
	}
	if v := q.Get("include_inactive"); v != "" {
		filter.IncludeInactive, _ = strconv.ParseBool(v)
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}

	agentID, _ := AgentIDFromContext(r.Context())
	entries, next, err := s.Knowledge.Search(r.Context(), agentID, filter)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if entries == nil {
		entries = []model.Knowledge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next_cursor": next})
}

func (s *Server) handleCreateArtifact(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var req model.CreateArtifactRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	artifact, err := s.Artifacts.Create(r.Context(), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, artifact)
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	artifact, err := s.Artifacts.Get(r.Context(), agentID, r.PathValue("artifact_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, artifact)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	var req model.CreateProjectRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	project, err := s.Projects.Create(r.Context(), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleDiscoverProjects(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	projects, err := s.Projects.List(r.Context(), agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if projects == nil {
		projects = []model.Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}
