package agent

import (
	"strings"
	"testing"

	"aichatdeck/internal/model"
)

// formatKnowledge renders shared-memory entries; their status must travel
// with them so a proposed guess is never read as an established fact
// (RFC-1300 §5).
func TestFormatKnowledgeCarriesStatus(t *testing.T) {
	out := formatKnowledge([]model.Knowledge{{
		Category: model.KnowledgeHypothesis,
		Status:   model.KnowledgeProposed,
		Content:  map[string]any{"summary": "the archive is sorted by date"},
	}})
	if !strings.Contains(out, model.KnowledgeProposed) {
		t.Errorf("status missing from rendered knowledge: %q", out)
	}
	if !strings.Contains(out, "the archive is sorted by date") {
		t.Errorf("summary missing from rendered knowledge: %q", out)
	}
}
