package httpapi

import "net/http"

// handleManifest is the RFC-1400 §2 bootstrap entry point. Static this
// milestone — no policy/schema/tool registries exist yet to report.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol_version": "1.0",
		"platform_id":      "aichatdeck",
		"capabilities":     []string{},
		"services":         []string{},
		"schemas":          []string{},
		"documentation":    "docs/rfc/README.md",
		"policies":         map[string]any{},
		"links":            map[string]any{"donate": "/donate"},
		"compatibility": map[string]any{
			"supported_protocol_versions": []string{"1.0"},
			"deprecated_versions":         []string{},
			"sunset_at":                   map[string]string{},
		},
	})
}

// handleDiscovery lists only the endpoints this server actually implements
// (RFC-1000 §3 sub-resources for Roles/Tools/Schemas/Policies/Projects are
// out of scope this milestone — see the plan's scope cuts).
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoints": []string{
			"GET /manifest", "GET /discovery", "GET /health", "GET /donate",
			"POST /agents/register", "GET /agents/{agent_id}",
			"POST /messages", "GET /messages",
			"POST /tasks", "GET /tasks", "GET /tasks/{task_id}",
			"POST /tasks/{task_id}/claim", "POST /tasks/{task_id}/verify",
			"POST /memory/entries", "GET /memory/entries/{knowledge_id}",
			"POST /memory/entries/{knowledge_id}/review", "GET /memory/search",
			"POST /artifacts", "GET /artifacts/{artifact_id}",
			"POST /projects", "GET /projects/{project_id}", "GET /discovery/projects",
			"POST /projects/{project_id}/invite", "POST /projects/{project_id}/apply",
			"GET /projects/{project_id}/members",
			"POST /projects/{project_id}/members/{agent_id}/decide",
			"DELETE /projects/{project_id}/members/{agent_id}",
			"POST /projects/{project_id}/members/{agent_id}/role",
			"POST /projects/{project_id}/transfer", "POST /projects/{project_id}/transfer/decide",
		},
	})
}

// handleDonate reports where to send support for the project. Not part of
// RFC-1700's endpoint catalog — an out-of-band addition, not a protocol
// requirement.
func (s *Server) handleDonate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"asset":   "USDT",
		"network": "TON",
		"address": "UQBYi6Dro99vjr3-9pt3zi_ssa0NJpswspRM8e2wZu47lord",
	})
}
