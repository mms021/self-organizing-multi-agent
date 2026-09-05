package model

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Size limits on agent-supplied text. The transport cap (httpapi's 1 MiB body
// limit) stops the server from being flooded; these stop one agent from
// filling shared state — and from filling other agents' prompts, which is
// billed per token and where a wall of text pushes the real instructions out
// of the model's attention.
const (
	MaxTextField  = 4000  // objective, summary, rationale, free-form prose
	MaxShortField = 200   // names, tags, ids, single skills
	MaxListItems  = 64    // any array of the above
	MaxObjectSize = 16384 // JSON bytes of a free-form object (content, context)
)

// checkText rejects an over-long string field. Length is counted in runes:
// the limit describes text, not encoding.
func checkText(field, s string, max int) *APIError {
	if utf8.RuneCountInString(s) > max {
		return ValidationError(fmt.Sprintf("%s must be at most %d characters", field, max))
	}
	return nil
}

// checkList rejects an over-long array or an over-long element of one.
func checkList(field string, items []string, maxItems, maxItem int) *APIError {
	if len(items) > maxItems {
		return ValidationError(fmt.Sprintf("%s must have at most %d entries", field, maxItems))
	}
	for i, it := range items {
		if err := checkText(fmt.Sprintf("%s[%d]", field, i), it, maxItem); err != nil {
			return err
		}
	}
	return nil
}

// checkObjectSize caps a free-form object by its serialized size — the only
// bound that means anything for a map[string]any of arbitrary shape.
func checkObjectSize(field string, obj map[string]any) *APIError {
	if len(obj) == 0 {
		return nil
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return ValidationError(field + " must be a JSON object")
	}
	if len(raw) > MaxObjectSize {
		return ValidationError(fmt.Sprintf("%s must be at most %d bytes of JSON", field, MaxObjectSize))
	}
	return nil
}
