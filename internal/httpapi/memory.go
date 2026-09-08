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
	if req.TaskID != nil {
		task, err := s.Tasks.Get(r.Context(), agentID, *req.TaskID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if req.ProjectID != nil && !sameProjectScope(req.ProjectID, task.ProjectID) {
			s.writeErr(w, model.ValidationError("knowledge project_id must match its task's project_id"))
			return
		}
		// Task-attached knowledge is evidence for that task, so its visibility
		// follows the task. Omitting project_id must not accidentally make a
		// closed-project lesson global.
		req.ProjectID = task.ProjectID
	}
	if req.ProjectID != nil {
		project, full, err := s.projectFor(r, *req.ProjectID, agentID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !full {
			s.writeErr(w, model.AccessDenied("only a project member may publish knowledge in it"))
			return
		}
		if project.Status != model.ProjectActive {
			s.writeErr(w, model.Conflict("cannot publish knowledge in an archived project"))
			return
		}
		if err := s.requireProjectWriter(r, project.ProjectID, agentID); err != nil {
			s.writeErr(w, err)
			return
		}
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
	if req.TaskID != nil {
		task, err := s.Tasks.Get(r.Context(), agentID, *req.TaskID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if req.ProjectID != nil && !sameProjectScope(req.ProjectID, task.ProjectID) {
			s.writeErr(w, model.ValidationError("artifact project_id must match its task's project_id"))
			return
		}
		// A task is the source of truth for an attached artifact's scope: this
		// prevents a client from accidentally publishing project evidence as a
		// global artifact by omitting project_id.
		req.ProjectID = task.ProjectID
		if task.ProjectID != nil {
			if err := s.requireProjectWriter(r, *task.ProjectID, agentID); err != nil {
				s.writeErr(w, err)
				return
			}
		}
	} else if req.ProjectID != nil {
		project, full, err := s.projectFor(r, *req.ProjectID, agentID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !full {
			s.writeErr(w, model.AccessDenied("only a project member may create artifacts in it"))
			return
		}
		if project.Status != model.ProjectActive {
			s.writeErr(w, model.Conflict("cannot create an artifact in an archived project"))
			return
		}
	}

	artifact, err := s.Artifacts.Create(r.Context(), agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, artifact)
}

func sameProjectScope(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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
