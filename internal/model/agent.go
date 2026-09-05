package model

import "fmt"

// Agent lifecycle statuses this milestone tracks (RFC-1400 §1, trimmed —
// COMPLETED/FAILED are per-task outcomes recorded on Task, not Agent).
const (
	AgentCreated = "CREATED"
	AgentReady   = "READY"
	AgentWorking = "WORKING"
	AgentWaiting = "WAITING"
	AgentBlocked = "BLOCKED"
	AgentRetired = "RETIRED"
)

// Capability, RFC-1800 §3 example.
type Capability struct {
	CapabilityID string   `json:"capability_id,omitempty"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// RegisterRequest is the POST /agents/register body (RFC-1000 §5, RFC-1400 §7).
type RegisterRequest struct {
	Capabilities   []Capability   `json:"capabilities,omitempty"`
	Skills         []string       `json:"skills,omitempty"`
	Constraints    map[string]any `json:"constraints,omitempty"`
	CurrentLoad    *float64       `json:"current_load,omitempty"`
	PreferredRoles []string       `json:"preferred_roles,omitempty"`
	Evidence       []string       `json:"evidence,omitempty"`
	Operator       string         `json:"operator,omitempty"`
}

func (r *RegisterRequest) Validate() *APIError {
	// RFC-1400 §7 requires capabilities, skills, constraints and
	// preferred_roles. Only capabilities is enforced: it is what tasks are
	// matched against, so an agent without it cannot be given work. The other
	// three are optional here — preferred_roles in particular refers to Roles
	// (RFC-1200), which this milestone does not implement.
	if len(r.Capabilities) == 0 {
		return ValidationError("capabilities is required: an agent with no capabilities cannot be matched to any task")
	}
	for i, c := range r.Capabilities {
		if c.Name == "" {
			return ValidationError(fmt.Sprintf("capabilities[%d].name is required", i))
		}
	}
	if r.CurrentLoad != nil && (*r.CurrentLoad < 0 || *r.CurrentLoad > 1) {
		return ValidationError("current_load must be between 0 and 1")
	}
	// Registration is the one unauthenticated write on the platform, so its
	// body is bounded field by field, not just by the transport cap.
	if len(r.Capabilities) > MaxListItems {
		return ValidationError(fmt.Sprintf("capabilities must have at most %d entries", MaxListItems))
	}
	for i, c := range r.Capabilities {
		if err := checkText(fmt.Sprintf("capabilities[%d].name", i), c.Name, MaxShortField); err != nil {
			return err
		}
		if err := checkText(fmt.Sprintf("capabilities[%d].description", i), c.Description, MaxTextField); err != nil {
			return err
		}
		if err := checkList(fmt.Sprintf("capabilities[%d].tags", i), c.Tags, MaxListItems, MaxShortField); err != nil {
			return err
		}
	}
	if err := checkList("skills", r.Skills, MaxListItems, MaxShortField); err != nil {
		return err
	}
	if err := checkList("preferred_roles", r.PreferredRoles, MaxListItems, MaxShortField); err != nil {
		return err
	}
	if err := checkList("evidence", r.Evidence, MaxListItems, MaxShortField); err != nil {
		return err
	}
	if err := checkText("operator", r.Operator, MaxShortField); err != nil {
		return err
	}
	return checkObjectSize("constraints", r.Constraints)
}

// Agent is the RFC-1800 §3 Agent shape. skills/preferred_roles/evidence/operator
// have no dedicated top-level field in that example, so they're folded into
// Metadata rather than invented as new fields (RFC-1800 §1: extension via
// metadata/extensions).
type Agent struct {
	AgentID       string         `json:"agent_id"`
	SchemaVersion string         `json:"schema_version"`
	Capabilities  []Capability   `json:"capabilities"`
	Roles         []string       `json:"roles"`
	Status        string         `json:"status"`
	Load          float64        `json:"load"`
	Reputation    map[string]any `json:"reputation"`
	Constraints   map[string]any `json:"constraints"`
	Metadata      map[string]any `json:"metadata"`
}

// CredentialOut is returned once, at issuance (register / idempotent
// re-register). The platform never stores or echoes the plaintext token again
// (RFC-1600 §7).
type CredentialOut struct {
	CredentialID string `json:"credential_id"`
	Token        string `json:"token"`
}

// RegisterResponse flattens Agent's fields alongside the issued credential.
type RegisterResponse struct {
	Agent
	Credential CredentialOut `json:"credential"`
}
