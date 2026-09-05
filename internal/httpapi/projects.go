package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

// platformSender identifies messages the platform itself emits, as opposed to
// one agent writing to another. It is not a registered agent, so nobody can
// authenticate as it and forge one.
const platformSender = "platform"

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
// RFC-1250 §8. The owner and their coordinators may do either.
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
	mayAdmin, err := s.Members.CanAdminister(r.Context(), project, agentID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !mayAdmin {
		s.writeErr(w, model.AccessDenied("only the owner or a coordinator may invite"))
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
	mayAdmin, err := s.Members.CanAdminister(r.Context(), project, callerID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if callerID != subjectID && !mayAdmin {
		// Neither side of this decision: say nothing about the project.
		s.writeErr(w, model.NotFound("project not found"))
		return
	}

	invitedByProject := member.InvitedBy != nil
	switch {
	case invitedByProject && callerID != subjectID:
		s.writeErr(w, model.AccessDenied("only the invited agent may answer an invitation"))
		return
	case !invitedByProject && !mayAdmin:
		s.writeErr(w, model.AccessDenied("only the owner or a coordinator may decide an application"))
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
	if callerID == subjectID {
		status = model.MemberLeft
	} else {
		mayAdmin, err := s.Members.CanAdminister(r.Context(), project, callerID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if !mayAdmin {
			s.writeErr(w, model.AccessDenied("only the owner or a coordinator may remove another member"))
			return
		}
		if subjectID == project.Owner {
			s.writeErr(w, model.AccessDenied("the owner cannot be removed by anyone else"))
			return
		}
	}

	// A departing owner must leave the project with someone responsible for
	// it: succession first, archival if there is nobody (RFC-1250 §9).
	if subjectID == project.Owner {
		successor, err := s.successorFor(r, project)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		updated, err := s.Projects.Succeed(r.Context(), projectID, successor)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		if _, err := s.Members.SetStatus(r.Context(), projectID, subjectID, model.MemberLeft); err != nil &&
			!model.IsCode(err, model.ErrNotFound) {
			s.writeErr(w, err)
			return
		}
		if successor != "" {
			s.notifyMembers(r, updated, "project_owner_changed", map[string]any{
				"project_id": projectID, "previous_owner": subjectID, "owner": successor,
			})
		} else {
			s.notifyMembers(r, updated, "project_archived", map[string]any{
				"project_id": projectID, "reason": "owner left and no coordinator could succeed",
			})
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	member, err := s.Members.SetStatus(r.Context(), projectID, subjectID, status)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, member)
}

// successorFor picks who inherits a project whose owner is leaving: the
// longest-standing coordinator, or nobody (RFC-1250 §9 orders by Influence,
// RFC-1500 §7, which has no implementation yet — see Coordinators()).
func (s *Server) successorFor(r *http.Request, project model.Project) (string, error) {
	if project.Visibility != model.ProjectClosed {
		return "", nil
	}
	coordinators, err := s.Members.Coordinators(r.Context(), project.ProjectID)
	if err != nil {
		return "", err
	}
	for _, c := range coordinators {
		// The departing owner is usually still marked a coordinator — from
		// before they held the title, or from having been appointed one.
		// Without this they would inherit from themselves and the departure
		// would silently do nothing.
		if c.AgentID != project.Owner {
			return c.AgentID, nil
		}
	}
	return "", nil
}

// handleSetRole appoints or removes a coordinator. Only the owner may:
// letting coordinators appoint each other would make the owner's authority
// unrecoverable once delegated (RFC-1250 §9).
func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	callerID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")
	subjectID := r.PathValue("agent_id")

	var req model.SetRoleRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	project, _, err := s.projectFor(r, projectID, callerID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if project.Owner != callerID {
		s.writeErr(w, model.AccessDenied("only the project owner may appoint coordinators"))
		return
	}
	if subjectID == project.Owner {
		s.writeErr(w, model.ValidationError("the owner already holds every authority a coordinator has"))
		return
	}

	member, err := s.Members.SetRole(r.Context(), projectID, subjectID, req.Role)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, member)
}

// handleTransfer nominates the next owner. Ownership does not move here:
// the nominee has to accept, which is the counterparty-as-approver case of
// RFC-1600 §8 — an agent cannot be made responsible for a project without
// agreeing to it.
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	callerID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")

	var req model.TransferRequest
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}
	if aerr := req.Validate(); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	project, _, err := s.projectFor(r, projectID, callerID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if project.Owner != callerID {
		s.writeErr(w, model.AccessDenied("only the current owner may transfer a project"))
		return
	}
	if req.NewOwner == project.Owner {
		s.writeErr(w, model.ValidationError("the project already belongs to that agent"))
		return
	}
	if _, err := s.Agents.Get(r.Context(), req.NewOwner); err != nil {
		s.writeErr(w, err)
		return
	}
	// A closed project can only be handed to someone already inside it.
	if project.Visibility == model.ProjectClosed {
		member, err := s.Members.Get(r.Context(), projectID, req.NewOwner)
		if err != nil || member.Status != model.MemberActive {
			s.writeErr(w, model.ValidationError("the nominee must already be an active member of this project"))
			return
		}
	}

	updated, err := s.Projects.Nominate(r.Context(), projectID, req.NewOwner)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.notifyAgent(r, req.NewOwner, "project_transfer_offered", map[string]any{
		"project_id": projectID, "from": callerID,
	})
	writeJSON(w, http.StatusOK, updated)
}

// handleDecideTransfer is the nominee accepting or declining. The current
// owner may also call it to withdraw their own nomination.
func (s *Server) handleDecideTransfer(w http.ResponseWriter, r *http.Request) {
	callerID, _ := AgentIDFromContext(r.Context())
	projectID := r.PathValue("project_id")

	var req model.MembershipDecision
	if aerr := decodeJSON(r, &req); aerr != nil {
		s.writeErr(w, aerr)
		return
	}

	project, err := s.Projects.Get(r.Context(), projectID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if project.PendingOwner == nil {
		s.writeErr(w, model.Conflict("no transfer is pending"))
		return
	}
	// The nomination itself is what entitles the nominee to answer, exactly as
	// an invitation entitles an invited agent (handleDecideMembership).
	if callerID != *project.PendingOwner && callerID != project.Owner {
		s.writeErr(w, model.NotFound("project not found"))
		return
	}

	if !req.Accept || callerID == project.Owner {
		updated, err := s.Projects.CancelTransfer(r.Context(), projectID)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	previousOwner := project.Owner
	updated, err := s.Projects.AcceptTransfer(r.Context(), projectID, callerID)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.notifyMembers(r, updated, "project_owner_changed", map[string]any{
		"project_id": projectID, "previous_owner": previousOwner, "owner": callerID,
	})
	writeJSON(w, http.StatusOK, updated)
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

// notifyAgent sends one EVENT (RFC-1100 §6.10) from the platform to a single
// agent.
func (s *Server) notifyAgent(r *http.Request, recipient, eventType string, data map[string]any) {
	payload, err := json.Marshal(model.EventPayload{EventType: eventType, Data: data})
	if err != nil {
		s.logf("notify %s: %v", recipient, err)
		return
	}
	env := model.Envelope{
		MessageID:       idgen.New("message"),
		ProtocolVersion: "1.0",
		Type:            "EVENT",
		Sender:          platformSender,
		Recipient:       recipient,
		Timestamp:       time.Now().UTC(),
		Priority:        "normal",
		Payload:         payload,
	}
	stored, isNew, err := s.Messages.Insert(r.Context(), env)
	if err != nil {
		s.logf("notify %s: %v", recipient, err)
		return
	}
	if isNew {
		_ = s.Bus.Publish(r.Context(), bus.AgentChannel(stored.Recipient))
	}
}

// notifyMembers announces a project event to the project's own members.
// Broadcasting it instead would tell the whole platform that a closed
// project exists and just changed hands (RFC-1250 §5).
func (s *Server) notifyMembers(r *http.Request, project model.Project, eventType string, data map[string]any) {
	recipients := map[string]bool{project.Owner: true}
	members, err := s.Members.List(r.Context(), project.ProjectID)
	if err != nil {
		s.logf("notify members of %s: %v", project.ProjectID, err)
	}
	for _, m := range members {
		if m.Status == model.MemberActive {
			recipients[m.AgentID] = true
		}
	}
	for agentID := range recipients {
		s.notifyAgent(r, agentID, eventType, data)
	}
}
