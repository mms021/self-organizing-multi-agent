package model

import "time"

// Project is the container knowledge and tasks can be scoped to (RFC-1250 §2).
//
// Only `open` visibility is implemented. Under RFC-1250 §11 an open project's
// records are visible to everyone and project_id is purely grouping — which is
// exactly what this does. `closed` additionally requires membership
// enforcement (RFC-1250 §5-§8), so it is rejected rather than accepted as a
// scope that looks isolating but is not.
type Project struct {
	ProjectID     string         `json:"project_id"`
	SchemaVersion string         `json:"schema_version"`
	Name          string         `json:"name"`
	Objective     string         `json:"objective"`
	Visibility    string         `json:"visibility"`
	Listed        bool           `json:"listed"`
	Owner         string         `json:"owner"`
	Status        string         `json:"status"`
	CreatedAt     time.Time      `json:"created_at"`
	Metadata      map[string]any `json:"metadata"`
}

const (
	ProjectOpen     = "open"
	ProjectClosed   = "closed"
	ProjectActive   = "active"
	ProjectArchived = "archived"
)

type CreateProjectRequest struct {
	Name       string `json:"name"`
	Objective  string `json:"objective,omitempty"`
	Visibility string `json:"visibility,omitempty"` // defaults to open
}

func (r *CreateProjectRequest) Validate() *APIError {
	if r.Name == "" {
		return ValidationError("name is required")
	}
	switch r.Visibility {
	case "", ProjectOpen:
	case ProjectClosed:
		return ValidationError("closed projects are not implemented yet: membership enforcement (RFC-1250 §5-§8) is out of scope this milestone")
	default:
		return ValidationError("visibility must be open")
	}
	return nil
}
