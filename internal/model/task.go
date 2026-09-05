package model

import "time"

// Task statuses this milestone implements (RFC-1800 Task example uses OPEN;
// CLAIMED/COMPLETED/FAILED are the RFC-1100 §6.7 VERIFY outcomes applied to it).
// HANDOFF is out of scope, so there is no intermediate "handed off" status.
const (
	TaskOpen      = "OPEN"
	TaskClaimed   = "CLAIMED"
	TaskCompleted = "COMPLETED"
	TaskFailed    = "FAILED"
)

// CreateTaskRequest is the POST /tasks body (RFC-1800 Task example fields).
// TeamID/ProjectID are accepted only as null — Teams/Projects are out of this
// milestone's scope, so a non-null value is rejected rather than silently
// ignored (see internal/store/tasks.go).
type CreateTaskRequest struct {
	Objective            string         `json:"objective"`
	Context              map[string]any `json:"context,omitempty"`
	Constraints          []string       `json:"constraints,omitempty"`
	RequiredCapabilities []string       `json:"required_capabilities,omitempty"`
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
	if r.ProjectID != nil {
		return ValidationError("project_id is not supported yet (RFC-1250 out of scope this milestone)")
	}
	return nil
}

// Task is the RFC-1800 Task shape.
type Task struct {
	TaskID               string         `json:"task_id"`
	SchemaVersion        string         `json:"schema_version"`
	Objective            string         `json:"objective"`
	Context              map[string]any `json:"context"`
	Constraints          []string       `json:"constraints"`
	RequiredCapabilities []string       `json:"required_capabilities"`
	SuccessCriteria      []string       `json:"success_criteria"`
	Status               string         `json:"status"`
	Owner                *string        `json:"owner"`
	TeamID               *string        `json:"team_id"`
	ProjectID            *string        `json:"project_id"`
	CreatedBy            string         `json:"created_by"`
	CreatedAt            time.Time      `json:"created_at"`
	Deadline             *time.Time     `json:"deadline"`
}

// TaskFilter is the GET /tasks query (RFC-1700 §4).
type TaskFilter struct {
	Status               string
	RequiredCapabilities []string
	Cursor               string
	Limit                int
}
