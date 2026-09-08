package model

import "time"

// Task execution and verification have separate states: only CLAIMED has
// a worker lease; SUBMITTED awaits the creator's verdict without requeueing.
const (
	TaskOpen      = "OPEN"
	TaskClaimed   = "CLAIMED"
	TaskSubmitted = "SUBMITTED"
	TaskCompleted = "COMPLETED"
	TaskFailed    = "FAILED"
)

// CreateTaskRequest is the POST /tasks body (RFC-1800 Task example fields).
// TeamID remains deferred with Roles; ProjectID scopes a task to an existing
// project and is authorized by the HTTP layer before it is persisted.
type CreateTaskRequest struct {
	Objective            string         `json:"objective"`
	Context              map[string]any `json:"context,omitempty"`
	Constraints          []string       `json:"constraints,omitempty"`
	RequiredCapabilities []string       `json:"required_capabilities,omitempty"`
	RequiredTools        []string       `json:"required_tools,omitempty"`
	SuccessCriteria      []string       `json:"success_criteria,omitempty"`
	Deadline             *time.Time     `json:"deadline,omitempty"`
	TeamID               *string        `json:"team_id,omitempty"`
	ProjectID            *string        `json:"project_id,omitempty"`
}

func (r *CreateTaskRequest) Validate() *APIError {
	if r.Objective == "" {
		return ValidationError("objective is required")
	}
	if r.TeamID != nil {
		return ValidationError("team_id is not supported yet (RFC-1200 out of scope this milestone)")
	}
	// Everything below crosses into other agents' prompts (internal/agent
	// brain.go), so it is bounded here rather than at the point of use.
	if err := checkText("objective", r.Objective, MaxTextField); err != nil {
		return err
	}
	if err := checkList("constraints", r.Constraints, MaxListItems, MaxTextField); err != nil {
		return err
	}
	if err := checkList("success_criteria", r.SuccessCriteria, MaxListItems, MaxTextField); err != nil {
		return err
	}
	if err := checkList("required_capabilities", r.RequiredCapabilities, MaxListItems, MaxShortField); err != nil {
		return err
	}
	if err := checkList("required_tools", r.RequiredTools, MaxListItems, MaxShortField); err != nil {
		return err
	}
	return checkObjectSize("context", r.Context)
}

// Task is the RFC-1800 Task shape.
type Task struct {
	TaskID               string         `json:"task_id"`
	SchemaVersion        string         `json:"schema_version"`
	Objective            string         `json:"objective"`
	Context              map[string]any `json:"context"`
	Constraints          []string       `json:"constraints"`
	RequiredCapabilities []string       `json:"required_capabilities"`
	RequiredTools        []string       `json:"required_tools"`
	SuccessCriteria      []string       `json:"success_criteria"`
	Status               string         `json:"status"`
	Owner                *string        `json:"owner"`
	TeamID               *string        `json:"team_id"`
	ProjectID            *string        `json:"project_id"`
	CreatedBy            string         `json:"created_by"`
	CreatedAt            time.Time      `json:"created_at"`
	Deadline             *time.Time     `json:"deadline"`
	ClaimedAt            *time.Time     `json:"claimed_at"`
	LeaseExpiresAt       *time.Time     `json:"lease_expires_at"`
	ClaimID              string         `json:"claim_id"`
	SubmittedMessageID   string         `json:"submitted_message_id"`
}

// TaskFilter is the GET /tasks query (RFC-1700 §4).
type TaskFilter struct {
	Status               string
	RequiredCapabilities []string
	RequiredTools        []string
	ProjectID            *string // nil = every visible scope; empty = global only
	Cursor               string
	Limit                int
}
