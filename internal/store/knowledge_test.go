package store

import (
	"context"
	"testing"

	"aichatdeck/internal/model"
)

func strptr(s string) *string { return &s }

func TestKnowledgeGlobalAndProjectScope(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	author := mustCreateAgent(t, stores.Agents)

	projects := NewProjectStore(stores.Agents.db)
	knowledge := NewKnowledgeStore(stores.Agents.db)

	proj, err := projects.Create(ctx, author.AgentID, model.CreateProjectRequest{Name: "alpha"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	other, err := projects.Create(ctx, author.AgentID, model.CreateProjectRequest{Name: "beta"})
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}

	mk := func(summary string, projectID *string) {
		t.Helper()
		if _, err := knowledge.Create(ctx, author.AgentID, model.CreateKnowledgeRequest{
			Category:  model.KnowledgeLesson,
			Content:   map[string]any{"summary": summary},
			ProjectID: projectID,
		}); err != nil {
			t.Fatalf("create knowledge %q: %v", summary, err)
		}
	}
	mk("global thing", nil)
	mk("alpha thing", &proj.ProjectID)
	mk("beta thing", &other.ProjectID)

	// Global-only scope.
	entries, _, err := knowledge.Search(ctx, model.KnowledgeFilter{ProjectID: strptr("")})
	if err != nil {
		t.Fatalf("search global: %v", err)
	}
	if len(entries) != 1 || entries[0].Content["summary"] != "global thing" {
		t.Fatalf("global scope should return only global knowledge, got %d: %+v", len(entries), entries)
	}

	// Project scope sees its own records plus global ones, never another
	// project's.
	entries, _, err = knowledge.Search(ctx, model.KnowledgeFilter{ProjectID: &proj.ProjectID})
	if err != nil {
		t.Fatalf("search project: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("project scope should return project + global, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Content["summary"] == "beta thing" {
			t.Fatal("project scope leaked another project's knowledge")
		}
	}
}

// RFC-1300 §5: a fact is what a verified hypothesis becomes. Letting an agent
// publish one directly would make "fact" mean nothing.
func TestKnowledgeRejectsDirectFact(t *testing.T) {
	req := model.CreateKnowledgeRequest{
		Category: model.KnowledgeFact,
		Content:  map[string]any{"summary": "the sky is green"},
	}
	if err := req.Validate(); err == nil {
		t.Fatal("expected a direct fact to be rejected")
	}
}

func TestKnowledgeSupersedeChain(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	author := mustCreateAgent(t, stores.Agents)
	knowledge := NewKnowledgeStore(stores.Agents.db)

	first, err := knowledge.Create(ctx, author.AgentID, model.CreateKnowledgeRequest{
		Category: model.KnowledgeHypothesis,
		Content:  map[string]any{"summary": "v1"},
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}

	second, err := knowledge.Create(ctx, author.AgentID, model.CreateKnowledgeRequest{
		Category:   model.KnowledgeHypothesis,
		Content:    map[string]any{"summary": "v2"},
		Supersedes: &first.KnowledgeID,
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("expected version 2, got %d", second.Version)
	}

	// The old record is archived but preserved, linked both ways (RFC-1300 §9).
	old, err := knowledge.Get(ctx, first.KnowledgeID)
	if err != nil {
		t.Fatalf("get superseded: %v", err)
	}
	if old.Status != model.KnowledgeArchived {
		t.Errorf("expected superseded record archived, got %s", old.Status)
	}
	if old.SupersededBy == nil || *old.SupersededBy != second.KnowledgeID {
		t.Errorf("superseded_by not linked: %+v", old.SupersededBy)
	}
	if second.Supersedes == nil || *second.Supersedes != first.KnowledgeID {
		t.Errorf("supersedes not linked: %+v", second.Supersedes)
	}

	// Archived records are excluded from search by default.
	entries, _, err := knowledge.Search(ctx, model.KnowledgeFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(entries) != 1 || entries[0].KnowledgeID != second.KnowledgeID {
		t.Fatalf("expected only the current version, got %+v", entries)
	}

	// Superseding the same record twice is a conflict, not a silent fork.
	_, err = knowledge.Create(ctx, author.AgentID, model.CreateKnowledgeRequest{
		Category:   model.KnowledgeHypothesis,
		Content:    map[string]any{"summary": "v2-fork"},
		Supersedes: &first.KnowledgeID,
	})
	if code := apiCode(t, err); code != model.ErrConflict {
		t.Fatalf("expected conflict on double supersede, got %s", code)
	}
}

// RFC-1300 §10: proposed -> reviewed is open to anyone, but verified and
// rejected are reachable only through verification, never by direct edit.
func TestKnowledgeReviewOnlyFromProposed(t *testing.T) {
	ctx := context.Background()
	stores := openTestDB(t)
	author := mustCreateAgent(t, stores.Agents)
	knowledge := NewKnowledgeStore(stores.Agents.db)

	entry, err := knowledge.Create(ctx, author.AgentID, model.CreateKnowledgeRequest{
		Category: model.KnowledgeHypothesis,
		Content:  map[string]any{"summary": "maybe"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if entry.Status != model.KnowledgeProposed {
		t.Fatalf("expected proposed on creation, got %s", entry.Status)
	}

	reviewed, err := knowledge.Review(ctx, entry.KnowledgeID)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if reviewed.Status != model.KnowledgeReviewed {
		t.Fatalf("expected reviewed, got %s", reviewed.Status)
	}

	if _, err := knowledge.Review(ctx, entry.KnowledgeID); err == nil {
		t.Fatal("expected a second review to conflict")
	}
}

func TestArtifactRequiresChecksum(t *testing.T) {
	req := model.CreateArtifactRequest{Type: "file", URI: "file:///tmp/x"}
	if err := req.Validate(); err == nil {
		t.Fatal("expected an artifact without a checksum to be rejected")
	}
}

// Closed projects must be refused while membership enforcement is unbuilt —
// accepting one would promise isolation the platform cannot deliver.
func TestClosedProjectRejected(t *testing.T) {
	req := model.CreateProjectRequest{Name: "secret", Visibility: model.ProjectClosed}
	if err := req.Validate(); err == nil {
		t.Fatal("expected closed visibility to be rejected")
	}
}
