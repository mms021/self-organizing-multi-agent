package model

import (
	"strconv"
	"time"

	"aichatdeck/internal/artifactpolicy"
)

// Knowledge categories, RFC-1300 §2.
const (
	KnowledgeFact       = "fact"
	KnowledgeHypothesis = "hypothesis"
	KnowledgeDecision   = "decision"
	KnowledgeEvent      = "event"
	KnowledgeError      = "error"
	KnowledgeLesson     = "lesson"
	KnowledgeProcedure  = "procedure"
)

var knowledgeCategories = map[string]bool{
	KnowledgeFact: true, KnowledgeHypothesis: true, KnowledgeDecision: true,
	KnowledgeEvent: true, KnowledgeError: true, KnowledgeLesson: true,
	KnowledgeProcedure: true,
	// "task" and "artifact" from RFC-1300 §2 are entities in their own right
	// here (tasks table, artifacts table), so they are not knowledge categories.
}

// Knowledge lifecycle, RFC-1300 §4.
const (
	KnowledgeProposed = "proposed"
	KnowledgeReviewed = "reviewed"
	KnowledgeVerified = "verified"
	KnowledgeArchived = "archived"
	KnowledgeRejected = "rejected"
)

// Reference is a typed pointer to another entity (RFC-1300 §3 `references`).
type Reference struct {
	Type string `json:"type"` // knowledge|artifact|task|message
	ID   string `json:"id"`
}

// Knowledge is a shared-memory record (RFC-1300 §3).
type Knowledge struct {
	KnowledgeID   string         `json:"knowledge_id"`
	SchemaVersion string         `json:"schema_version"`
	Category      string         `json:"category"`
	Author        string         `json:"author"`
	Content       map[string]any `json:"content"`
	Source        string         `json:"source,omitempty"`
	Confidence    float64        `json:"confidence"`
	Status        string         `json:"status"`
	Tags          []string       `json:"tags"`
	References    []Reference    `json:"references"`
	ProjectID     *string        `json:"project_id"`
	TaskID        *string        `json:"task_id"`
	Supersedes    *string        `json:"supersedes"`
	SupersededBy  *string        `json:"superseded_by"`
	Version       int            `json:"version"`
	CreatedAt     time.Time      `json:"created_at"`
}

// CreateKnowledgeRequest is the POST /memory/entries body.
type CreateKnowledgeRequest struct {
	Category   string         `json:"category"`
	Content    map[string]any `json:"content"`
	Source     string         `json:"source,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	References []Reference    `json:"references,omitempty"`
	ProjectID  *string        `json:"project_id,omitempty"` // null = global
	TaskID     *string        `json:"task_id,omitempty"`
	// Supersedes replaces an earlier record: the old one is archived and
	// linked both ways (RFC-1300 §9).
	Supersedes *string `json:"supersedes,omitempty"`
}

func (r *CreateKnowledgeRequest) Validate() *APIError {
	if !knowledgeCategories[r.Category] {
		return ValidationError("category must be one of: fact, hypothesis, decision, event, error, lesson, procedure")
	}
	if len(r.Content) == 0 {
		return ValidationError("content is required")
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return ValidationError("confidence must be between 0 and 1")
	}
	// A `fact` is what a hypothesis becomes *after* verification (RFC-1300 §5).
	// Publishing one directly would let an agent mint facts by assertion.
	if r.Category == KnowledgeFact {
		return ValidationError("a fact cannot be published directly: publish a hypothesis and have it verified (RFC-1300 §5)")
	}
	for i, ref := range r.References {
		switch ref.Type {
		case "knowledge", "artifact", "task", "message":
		default:
			return ValidationError("references[" + strconv.Itoa(i) + "].type must be knowledge, artifact, task or message")
		}
		if ref.ID == "" {
			return ValidationError("references[" + strconv.Itoa(i) + "].id is required")
		}
		if err := checkText("references["+strconv.Itoa(i)+"].id", ref.ID, MaxShortField); err != nil {
			return err
		}
	}
	if len(r.References) > MaxListItems {
		return ValidationError("references must have at most " + strconv.Itoa(MaxListItems) + " entries")
	}
	// Shared memory is read straight into other agents' prompts (RFC-1300 §8),
	// so an entry is bounded like any other untrusted prompt input.
	if err := checkList("tags", r.Tags, MaxListItems, MaxShortField); err != nil {
		return err
	}
	if err := checkText("source", r.Source, MaxShortField); err != nil {
		return err
	}
	return checkObjectSize("content", r.Content)
}

// ReviewKnowledgeRequest is the POST /memory/entries/{id}/review body. Only
// the proposed -> reviewed transition is available this way; verified and
// rejected MUST come from a VERIFY message (RFC-1300 §10).
type ReviewKnowledgeRequest struct {
	Note string `json:"note,omitempty"`
}

// KnowledgeFilter is the GET /memory/search query (RFC-1300 §8).
type KnowledgeFilter struct {
	Category  string
	Status    string
	TaskID    string
	Tags      []string
	ProjectID *string // nil = any scope; set with empty string for global-only
	Query     string  // substring match against content/tags
	// IncludeInactive returns archived and rejected records too; by default
	// they are excluded (RFC-1300 §8).
	IncludeInactive bool
	Cursor          string
	Limit           int
}

// Artifact is a verifiable object referenced by results (RFC-1300 §7).
type Artifact struct {
	ArtifactID    string     `json:"artifact_id"`
	SchemaVersion string     `json:"schema_version"`
	Type          string     `json:"type"`
	URI           string     `json:"uri"`
	Checksum      string     `json:"checksum"`
	CreatedBy     string     `json:"created_by"`
	TaskID        *string    `json:"task_id"`
	ProjectID     *string    `json:"project_id"`
	Size          int64      `json:"size"`
	Retention     string     `json:"retention"`
	ExpiresAt     *time.Time `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

type CreateArtifactRequest struct {
	Type      string  `json:"type"`
	URI       string  `json:"uri"`
	Checksum  string  `json:"checksum"`
	TaskID    *string `json:"task_id,omitempty"`
	ProjectID *string `json:"project_id,omitempty"`
	Size      int64   `json:"size,omitempty"`
	Retention string  `json:"retention,omitempty"`
}

func (r *CreateArtifactRequest) Validate() *APIError {
	switch r.Type {
	case "file", "log", "test_result", "computation", "other":
	default:
		return ValidationError("type must be file, log, test_result, computation or other")
	}
	if r.URI == "" {
		return ValidationError("uri is required")
	}
	if err := artifactpolicy.URI(r.URI); err != nil {
		return ValidationError(err.Error())
	}
	// RFC-1300 §7: evidence artifacts MUST carry a checksum, and an artifact
	// is immutable once created — without it a result cannot be re-verified.
	if r.Checksum == "" {
		return ValidationError("checksum is required: an artifact used as evidence must be verifiable")
	}
	if r.Retention != "" && r.Retention != "permanent" && r.Retention != "ttl" {
		return ValidationError("retention must be permanent or ttl")
	}
	return nil
}
