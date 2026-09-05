package httpapi

import (
	"net/http"

	"aichatdeck/internal/model"
)

// projectFor resolves a project for a caller, applying the RFC-1250 §13
// disclosure rule:
//
//   - unlisted closed project, caller not a member -> not_found, identical to
//     an id that was never issued, so the endpoint cannot be used to discover
//     what exists;
//   - listed closed project, caller not a member -> the project is known to
//     exist, so withholding it would be theatre: access_denied.
//
// Returns the project and whether the caller may see its contents.
func (s *Server) projectFor(r *http.Request, projectID, agentID string) (model.Project, bool, error) {
	project, err := s.Projects.Get(r.Context(), projectID)
	if err != nil {
		return model.Project{}, false, err
	}
	if project.Visibility == model.ProjectOpen {
		return project, true, nil
	}

	canRead, err := s.Members.CanRead(r.Context(), projectID, agentID)
	if err != nil {
		return model.Project{}, false, err
	}
	if canRead || project.Owner == agentID {
		return project, true, nil
	}
	if project.Listed {
		return project, false, nil
	}
	return model.Project{}, false, model.NotFound("project not found")
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())

	project, full, err := s.projectFor(r, r.PathValue("project_id"), agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !full {
		// Listed, but the caller is outside it: acknowledge existence only.
		writeJSON(w, http.StatusOK, project.Listing())
		return
	}
	writeJSON(w, http.StatusOK, project)
}

// handleInvite adds a member, or grants the narrow reviewer access of
// RFC-1250 §8. Only the owner may do either — coordinators (RFC-1250 §2) are
// not implemented yet.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")

	var req model.InviteRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	project, _, err := s.projectFor(r, projectID, agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if project.Owner != agentID {
		s.writeErr(w, model.AccessDenied("only the project owner may invite"))
		return
	}
	if project.Visibility != model.ProjectClosed {
		s.writeErr(w, model.ValidationError("an open project has no membership to grant"))
		return
	}
	if _, err := s.Agents.Get(r.Context(), req.AgentID); err != nil {
		s.writeErr(w, err)
		return
	}

	member, err := s.Members.Invite(r.Context(), projectID, req.AgentID, agentID, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, member)
}

// handleApply is an agent asking to join a closed project itself, allowed
// only when the project's membership_policy permits it (RFC-1250 §6).
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")

	project, _, err := s.projectFor(r, projectID, agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if project.Visibility != model.ProjectClosed {
		s.writeErr(w, model.ValidationError("an open project needs no application"))
		return
	}
	if project.MembershipPolicy != model.PolicyInviteOrApply {
		s.writeErr(w, model.AccessDenied("this project is invite-only"))
		return
	}

	// invited_by empty marks this as the agent's own application, awaiting the
	// owner's decision rather than the agent's.
	member, err := s.Members.Invite(r.Context(), projectID, agentID, "", model.InviteRequest{AgentID: agentID})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, member)
}

// handleDecideMembership resolves a pending row. Which party is entitled to
// decide depends on who raised it: an invitation is the invited agent's to
// accept, an application is the owner's to approve (RFC-1250 §7).
func (s *Server) handleDecideMembership(w http.ResponseWriter, r *http.Request) {
	callerID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")
	subjectID := r.PathValue("agent_id")

	var req model.MembershipDecision
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	// Resolve the membership row first, not the project. An invited agent is
	// not yet a member, so the project would still read as non-existent to
	// them — but the invitation itself already disclosed it, and refusing to
	// let them answer would strand the invitation forever. The right to act
	// here comes from the row, not from visibility.
	member, err := s.Members.Get(r.Context(), projectID, subjectID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	project, err := s.Projects.Get(r.Context(), projectID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if callerID != subjectID && callerID != project.Owner {
		// Neither side of this decision: say nothing about the project.
		s.writeErr(w, model.NotFound("project not found"))
		return
	}

	invitedByProject := member.InvitedBy != nil
	switch {
	case invitedByProject && callerID != subjectID:
		s.writeErr(w, model.AccessDenied("only the invited agent may answer an invitation"))
		return
	case !invitedByProject && callerID != project.Owner:
		s.writeErr(w, model.AccessDenied("only the project owner may decide an application"))
		return
	}

	decided, err := s.Members.Decide(r.Context(), projectID, subjectID, req.Accept)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, decided)
}

// handleRemoveMembership covers both directions: the owner revoking someone,
// and a member leaving of their own accord (RFC-1250 §6).
func (s *Server) handleRemoveMembership(w http.ResponseWriter, r *http.Request) {
	callerID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")
	subjectID := r.PathValue("agent_id")

	project, _, err := s.projectFor(r, projectID, callerID)
	if err != nil {
		s.writeErr(w, err)
		return
	}

	status := model.MemberRevoked
	switch {
	case callerID == subjectID:
		status = model.MemberLeft
	case callerID != project.Owner:
		s.writeErr(w, model.AccessDenied("only the project owner may remove another member"))
		return
	}
	if subjectID == project.Owner {
		// RFC-1250 §9: a project must have an owner at all times, and
		// transferring ownership is not implemented yet.
		s.writeErr(w, model.Conflict("the owner cannot leave: ownership transfer is not implemented yet (RFC-1250 §9)"))
		return
	}

	member, err := s.Members.SetStatus(r.Context(), projectID, subjectID, status)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, member)
}

// handleListMembers exposes the roster to members only: in a closed project
// the membership list is itself private (RFC-1250 §5).
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	agentID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")

	project, full, err := s.projectFor(r, projectID, agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !full && project.Owner != agentID {
		s.writeErr(w, model.AccessDenied("the membership of a closed project is visible to its members only"))
		return
	}

	members, err := s.Members.List(r.Context(), projectID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if members == nil {
		members = []model.ProjectMembership{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}
