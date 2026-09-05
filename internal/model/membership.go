package model

import "time"

// Membership statuses, RFC-1250 §7.
const (
	MemberInvited  = "invited"
	MemberActive   = "active"
	MemberRevoked  = "revoked"
	MemberLeft     = "left"
	MemberReviewer = "reviewer"
)

// Membership policies, RFC-1250 §2.
const (
	PolicyInviteOnly    = "invite_only"
	PolicyInviteOrApply = "invite_or_apply"
)

// ProjectMembership is one agent's standing in one closed project
// (RFC-1250 §7).
type ProjectMembership struct {
	ProjectID string     `json:"project_id"`
	AgentID   string     `json:"agent_id"`
	Status    string     `json:"status"`
	InvitedBy *string    `json:"invited_by"`
	JoinedAt  *time.Time `json:"joined_at"`
	Scope     *string    `json:"scope"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// Pending reports whether this row is awaiting a decision, and from whom.
// invited_by non-nil means the project invited the agent; nil means the agent
// applied and the owner must approve (RFC-1250 §7).
func (m ProjectMembership) Pending() bool { return m.Status == MemberInvited }

// InviteRequest is POST /projects/{project_id}/invite.
//
// A reviewer invitation is the guest access of RFC-1250 §8: read-only, scoped
// to a single task, and time-boxed. It exists so a closed project can still be
// verified by someone outside it — without it, isolation and independent
// verification (RFC-0000 P2) would be mutually exclusive (ADR-0004).
type InviteRequest struct {
	AgentID string `json:"agent_id"`
	// Reviewer requests the §8 guest tier instead of full membership.
	Reviewer bool `json:"reviewer,omitempty"`
	// Scope is the task_id a reviewer may read. Required when Reviewer.
	Scope string `json:"scope,omitempty"`
	// ExpiresInSeconds time-boxes reviewer access. Required when Reviewer.
	ExpiresInSeconds int `json:"expires_in_seconds,omitempty"`
}

func (r *InviteRequest) Validate() *APIError {
	if r.AgentID == "" {
		return ValidationError("agent_id is required")
	}
	if err := checkText("agent_id", r.AgentID, MaxShortField); err != nil {
		return err
	}
	if !r.Reviewer {
		if r.Scope != "" || r.ExpiresInSeconds != 0 {
			return ValidationError("scope and expires_in_seconds apply only to reviewer invitations")
		}
		return nil
	}
	// RFC-1250 §8: a reviewer grant without a scope and an expiry is not guest
	// access, it is unbounded membership under another name.
	if r.Scope == "" {
		return ValidationError("scope (a task_id) is required for a reviewer invitation")
	}
	if err := checkText("scope", r.Scope, MaxShortField); err != nil {
		return err
	}
	if r.ExpiresInSeconds <= 0 {
		return ValidationError("expires_in_seconds must be positive for a reviewer invitation")
	}
	if r.ExpiresInSeconds > 30*24*3600 {
		return ValidationError("expires_in_seconds must be at most 30 days")
	}
	return nil
}

// MembershipDecision answers a pending invitation or application.
type MembershipDecision struct {
	Accept bool `json:"accept"`
}
