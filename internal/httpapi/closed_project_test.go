package httpapi_test

import (
	"net/http"
	"testing"
)

// helpers ------------------------------------------------------------------

func createProject(t *testing.T, c *client, body map[string]any) string {
	t.Helper()
	status, out := c.do(http.MethodPost, "/projects", body)
	if status != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %v", status, out)
	}
	id, _ := out["project_id"].(string)
	if id == "" {
		t.Fatalf("create project: no project_id in %v", out)
	}
	return id
}

func publishKnowledge(t *testing.T, c *client, projectID, summary string) string {
	t.Helper()
	body := map[string]any{
		"category": "lesson",
		"content":  map[string]any{"summary": summary},
	}
	if projectID != "" {
		body["project_id"] = projectID
	}
	status, out := c.do(http.MethodPost, "/memory/entries", body)
	if status != http.StatusCreated {
		t.Fatalf("publish knowledge: expected 201, got %d: %v", status, out)
	}
	id, _ := out["knowledge_id"].(string)
	return id
}

func searchSummaries(t *testing.T, c *client, query string) []string {
	t.Helper()
	status, out := c.do(http.MethodGet, "/memory/search?q="+query, nil)
	if status != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %v", status, out)
	}
	entries, _ := out["entries"].([]any)
	var summaries []string
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		content, _ := entry["content"].(map[string]any)
		if s, ok := content["summary"].(string); ok {
			summaries = append(summaries, s)
		}
	}
	return summaries
}

// tests --------------------------------------------------------------------

// The whole point of a closed project: its contents do not reach outsiders,
// through any read path (RFC-1250 §11).
func TestClosedProjectHidesKnowledgeFromOutsiders(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	outsider := register(t, srv.URL)

	projectID := createProject(t, owner, map[string]any{"name": "secret", "visibility": "closed"})
	secretID := publishKnowledge(t, owner, projectID, "secret-recipe")
	publishKnowledge(t, owner, "", "public-recipe")

	// Search: the outsider sees the global record and nothing else.
	got := searchSummaries(t, outsider, "recipe")
	for _, s := range got {
		if s == "secret-recipe" {
			t.Fatal("closed-project knowledge leaked through search")
		}
	}
	if len(got) != 1 || got[0] != "public-recipe" {
		t.Fatalf("outsider should see exactly the global record, got %v", got)
	}

	// Direct fetch by id: not_found, not access_denied — confirming the id
	// exists would itself be a leak (RFC-1250 §13).
	status, body := outsider.do(http.MethodGet, "/memory/entries/"+secretID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("direct fetch: expected 404, got %d: %v", status, body)
	}

	// The same disclosure rule must protect mutations too: a guessed id may
	// not let an outsider change a private record before receiving its 404.
	status, _ = outsider.do(http.MethodPost, "/memory/entries/"+secretID+"/review", map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("outsider review: expected 404, got %d", status)
	}
	status, reviewed := owner.do(http.MethodGet, "/memory/entries/"+secretID, nil)
	if status != http.StatusOK || reviewed["status"] != "proposed" {
		t.Fatalf("outsider changed private knowledge status: %d %v", status, reviewed)
	}

	// The owner still sees both.
	ownerSees := searchSummaries(t, owner, "recipe")
	if len(ownerSees) != 2 {
		t.Fatalf("owner should see both records, got %v", ownerSees)
	}
}

// An unlisted closed project must be indistinguishable from one that was
// never created; a listed one may admit it exists but nothing more
// (RFC-1250 §5, §13).
func TestClosedProjectDisclosure(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	outsider := register(t, srv.URL)

	hidden := createProject(t, owner, map[string]any{
		"name": "hidden", "visibility": "closed", "objective": "must not leak",
	})
	listed := createProject(t, owner, map[string]any{
		"name": "listed", "visibility": "closed", "listed": true, "objective": "must not leak",
	})

	// Unlisted: the same answer as a project id that was never issued.
	status, _ := outsider.do(http.MethodGet, "/projects/"+hidden, nil)
	if status != http.StatusNotFound {
		t.Errorf("unlisted closed project: expected 404, got %d", status)
	}
	statusUnknown, _ := outsider.do(http.MethodGet, "/projects/project-never-existed", nil)
	if status != statusUnknown {
		t.Errorf("hidden project answers %d but an unknown id answers %d — that difference is an enumeration oracle",
			status, statusUnknown)
	}

	// Listed: exists, but the objective stays inside.
	status, body := outsider.do(http.MethodGet, "/projects/"+listed, nil)
	if status != http.StatusOK {
		t.Fatalf("listed closed project: expected 200, got %d: %v", status, body)
	}
	if body["objective"] != "" {
		t.Errorf("listed closed project leaked its objective: %v", body["objective"])
	}

	// Discovery shows the listed one only, and only as a stub.
	status, disc := outsider.do(http.MethodGet, "/discovery/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("discovery: expected 200, got %d", status)
	}
	projects, _ := disc["projects"].([]any)
	for _, p := range projects {
		proj, _ := p.(map[string]any)
		if proj["project_id"] == hidden {
			t.Error("discovery listed an unlisted closed project")
		}
		if proj["project_id"] == listed && proj["objective"] != "" {
			t.Error("discovery leaked a closed project's objective")
		}
	}

	// The membership roster is private too.
	status, _ = outsider.do(http.MethodGet, "/projects/"+listed+"/members", nil)
	if status != http.StatusForbidden {
		t.Errorf("member list of a closed project: expected 403 for an outsider, got %d", status)
	}
}

// Invitation → acceptance grants access; leaving takes it away again.
func TestClosedProjectMembershipLifecycle(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	guest := register(t, srv.URL)
	guestID := mustAgentID(t, guest)

	projectID := createProject(t, owner, map[string]any{"name": "club", "visibility": "closed"})
	publishKnowledge(t, owner, projectID, "members-only")

	if seen := searchSummaries(t, guest, "members-only"); len(seen) != 0 {
		t.Fatalf("guest saw project knowledge before joining: %v", seen)
	}

	// Only the owner may invite.
	status, _ := guest.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": guestID})
	if status != http.StatusNotFound && status != http.StatusForbidden {
		t.Errorf("an outsider inviting themselves: expected refusal, got %d", status)
	}

	status, body := owner.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": guestID})
	if status != http.StatusCreated {
		t.Fatalf("invite: expected 201, got %d: %v", status, body)
	}
	if body["status"] != "invited" {
		t.Fatalf("expected a pending invitation, got %v", body["status"])
	}

	// Pending is not yet access.
	if seen := searchSummaries(t, guest, "members-only"); len(seen) != 0 {
		t.Fatalf("a pending invitation already granted access: %v", seen)
	}

	// The owner cannot accept on the guest's behalf.
	status, _ = owner.do(http.MethodPost, "/projects/"+projectID+"/members/"+guestID+"/decide",
		map[string]any{"accept": true})
	if status != http.StatusForbidden {
		t.Errorf("owner accepting an invitation for someone else: expected 403, got %d", status)
	}

	status, body = guest.do(http.MethodPost, "/projects/"+projectID+"/members/"+guestID+"/decide",
		map[string]any{"accept": true})
	if status != http.StatusOK || body["status"] != "active" {
		t.Fatalf("accept: expected 200/active, got %d: %v", status, body)
	}

	if seen := searchSummaries(t, guest, "members-only"); len(seen) != 1 {
		t.Fatalf("member cannot see project knowledge: %v", seen)
	}

	// Leaving ends access.
	status, _ = guest.do(http.MethodDelete, "/projects/"+projectID+"/members/"+guestID, nil)
	if status != http.StatusOK {
		t.Fatalf("leave: expected 200, got %d", status)
	}
	if seen := searchSummaries(t, guest, "members-only"); len(seen) != 0 {
		t.Fatalf("access survived departure: %v", seen)
	}
}

// Project-scoped tasks follow the same isolation rules as project knowledge:
// outsiders cannot enumerate or claim them, while a member can discover and
// work on them after accepting an invitation.
func TestClosedProjectTasksRequireMembership(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	member := register(t, srv.URL)
	outsider := register(t, srv.URL)
	memberID := mustAgentID(t, member)

	projectID := createProject(t, owner, map[string]any{"name": "task-club", "visibility": "closed"})
	status, task := owner.do(http.MethodPost, "/tasks", map[string]any{
		"objective": "private work", "project_id": projectID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create project task: expected 201, got %d: %v", status, task)
	}
	taskID, _ := task["task_id"].(string)

	// A hidden project's id cannot be used to create, read or claim tasks.
	status, _ = outsider.do(http.MethodPost, "/tasks", map[string]any{
		"objective": "intrude", "project_id": projectID,
	})
	if status != http.StatusNotFound {
		t.Fatalf("outsider create: expected 404, got %d", status)
	}
	status, listed := outsider.do(http.MethodGet, "/tasks?status=OPEN&project_id="+projectID, nil)
	if status != http.StatusOK {
		t.Fatalf("outsider list: expected 200, got %d: %v", status, listed)
	}
	if tasks, _ := listed["tasks"].([]any); len(tasks) != 0 {
		t.Fatalf("outsider discovered closed-project tasks: %v", tasks)
	}
	status, _ = outsider.do(http.MethodPost, "/tasks/"+taskID+"/claim", nil)
	if status != http.StatusNotFound {
		t.Fatalf("outsider claim: expected 404, got %d", status)
	}

	status, out := owner.do(http.MethodPost, "/projects/"+projectID+"/invite", map[string]any{"agent_id": memberID})
	if status != http.StatusCreated {
		t.Fatalf("invite member: expected 201, got %d: %v", status, out)
	}
	status, out = member.do(http.MethodPost, "/projects/"+projectID+"/members/"+memberID+"/decide", map[string]any{"accept": true})
	if status != http.StatusOK {
		t.Fatalf("accept membership: expected 200, got %d: %v", status, out)
	}

	status, listed = member.do(http.MethodGet, "/tasks?status=OPEN&project_id="+projectID, nil)
	if status != http.StatusOK {
		t.Fatalf("member list: expected 200, got %d: %v", status, listed)
	}
	if tasks, _ := listed["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("member should see exactly the project task, got %v", listed)
	}
	status, out = member.do(http.MethodPost, "/tasks/"+taskID+"/claim", nil)
	if status != http.StatusOK || out["status"] != "CLAIMED" {
		t.Fatalf("member claim: expected claimed task, got %d: %v", status, out)
	}
}

// A task_id must not become an exfiltration label: task messages stay between
// active members, and evidence inherits the task's project rather than a
// client-supplied (or omitted) scope.
func TestClosedProjectTaskMessagesAndArtifactsStayScoped(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	member := register(t, srv.URL)
	outsider := register(t, srv.URL)
	ownerID := mustAgentID(t, owner)
	memberID := mustAgentID(t, member)
	outsiderID := mustAgentID(t, outsider)

	projectID := createProject(t, owner, map[string]any{"name": "sealed", "visibility": "closed"})
	joinProject(t, owner, member, projectID, memberID)
	status, task := owner.do(http.MethodPost, "/tasks", map[string]any{
		"objective": "sealed work", "project_id": projectID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create project task: expected 201, got %d: %v", status, task)
	}
	taskID, _ := task["task_id"].(string)
	claimStatus, claimOut := member.do(http.MethodPost, "/tasks/"+taskID+"/claim", nil)
	if claimStatus != http.StatusOK {
		t.Fatalf("member claim: expected 200, got %d: %v", claimStatus, claimOut)
	}

	message := func(id, recipient string) map[string]any {
		return map[string]any{
			"message_id": id, "protocol_version": "1.0", "type": "RESULT",
			"sender": memberID, "recipient": recipient, "task_id": taskID,
			"timestamp": "2026-09-06T00:00:00Z", "priority": "normal",
			"payload": map[string]any{"summary": "sealed result", "status": "success", "claim_id": claimOut["claim_id"]},
		}
	}
	if status, out := member.do(http.MethodPost, "/messages", message("sealed-to-owner", ownerID)); status != http.StatusCreated {
		t.Fatalf("member message to owner: expected 201, got %d: %v", status, out)
	}
	if status, _ := member.do(http.MethodPost, "/messages", message("sealed-to-outsider", outsiderID)); status != http.StatusForbidden {
		t.Fatalf("member message to outsider: expected 403, got %d", status)
	}
	if status, _ := member.do(http.MethodPost, "/messages", message("sealed-broadcast", "broadcast")); status != http.StatusForbidden {
		t.Fatalf("closed task broadcast: expected 403, got %d", status)
	}

	artifactBody := map[string]any{
		"type": "test_result", "uri": "https://example.com/sealed.txt", "checksum": "sha256:sealed", "task_id": taskID,
	}
	status, artifact := member.do(http.MethodPost, "/artifacts", artifactBody)
	if status != http.StatusCreated {
		t.Fatalf("create scoped artifact: expected 201, got %d: %v", status, artifact)
	}
	if artifact["project_id"] != projectID {
		t.Fatalf("artifact did not inherit task project: %v", artifact)
	}
	artifactID, _ := artifact["artifact_id"].(string)
	if status, _ := outsider.do(http.MethodGet, "/artifacts/"+artifactID, nil); status != http.StatusNotFound {
		t.Fatalf("outsider artifact fetch: expected 404, got %d", status)
	}

	badScope := map[string]any{
		"type": "file", "uri": "https://example.com/mismatch", "checksum": "sha256:mismatch",
		"task_id": taskID, "project_id": "project-other",
	}
	if status, _ := member.do(http.MethodPost, "/artifacts", badScope); status != http.StatusBadRequest {
		t.Fatalf("mismatched artifact scope: expected 400, got %d", status)
	}

	knowledgeBody := map[string]any{
		"category": "lesson", "content": map[string]any{"summary": "sealed lesson"}, "task_id": taskID,
	}
	status, knowledge := member.do(http.MethodPost, "/memory/entries", knowledgeBody)
	if status != http.StatusCreated {
		t.Fatalf("create scoped knowledge: expected 201, got %d: %v", status, knowledge)
	}
	if knowledge["project_id"] != projectID {
		t.Fatalf("knowledge did not inherit task project: %v", knowledge)
	}
	knowledgeID, _ := knowledge["knowledge_id"].(string)
	if status, _ := outsider.do(http.MethodGet, "/memory/entries/"+knowledgeID, nil); status != http.StatusNotFound {
		t.Fatalf("outsider knowledge fetch: expected 404, got %d", status)
	}
	if status, _ := outsider.do(http.MethodPost, "/memory/entries", map[string]any{
		"category": "lesson", "content": map[string]any{"summary": "intrusion"}, "project_id": projectID,
	}); status != http.StatusNotFound {
		t.Fatalf("outsider knowledge publish: expected 404, got %d", status)
	}
	if status, _ := member.do(http.MethodPost, "/memory/entries", map[string]any{
		"category": "lesson", "content": map[string]any{"summary": "mismatch"},
		"task_id": taskID, "project_id": "project-other",
	}); status != http.StatusBadRequest {
		t.Fatalf("mismatched knowledge scope: expected 400, got %d", status)
	}
}

// An invite-only project refuses applications; invite_or_apply accepts them,
// and the owner decides (RFC-1250 §6).
func TestClosedProjectApplicationPolicy(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	applicant := register(t, srv.URL)
	applicantID := mustAgentID(t, applicant)

	strict := createProject(t, owner, map[string]any{
		"name": "strict", "visibility": "closed", "listed": true,
	})
	status, _ := applicant.do(http.MethodPost, "/projects/"+strict+"/apply", nil)
	if status != http.StatusForbidden {
		t.Errorf("applying to an invite-only project: expected 403, got %d", status)
	}

	open := createProject(t, owner, map[string]any{
		"name": "open-door", "visibility": "closed", "listed": true,
		"membership_policy": "invite_or_apply",
	})
	status, body := applicant.do(http.MethodPost, "/projects/"+open+"/apply", nil)
	if status != http.StatusCreated || body["status"] != "invited" {
		t.Fatalf("apply: expected 201/invited, got %d: %v", status, body)
	}

	// The applicant cannot approve their own application.
	status, _ = applicant.do(http.MethodPost, "/projects/"+open+"/members/"+applicantID+"/decide",
		map[string]any{"accept": true})
	if status != http.StatusForbidden {
		t.Errorf("self-approving an application: expected 403, got %d", status)
	}

	status, body = owner.do(http.MethodPost, "/projects/"+open+"/members/"+applicantID+"/decide",
		map[string]any{"accept": true})
	if status != http.StatusOK || body["status"] != "active" {
		t.Fatalf("owner approving: expected 200/active, got %d: %v", status, body)
	}
}

// Reviewer access (RFC-1250 §8) is what keeps a closed project verifiable by
// someone outside it — the justification recorded in ADR-0004. It must be
// scoped to its one task and grant nothing else.
func TestReviewerAccessIsNarrow(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	reviewer := register(t, srv.URL)
	reviewerID := mustAgentID(t, reviewer)

	projectID := createProject(t, owner, map[string]any{"name": "audited", "visibility": "closed"})

	// Two tasks in the project; the reviewer will be scoped to the first.
	status, taskBody := owner.do(http.MethodPost, "/tasks", map[string]any{
		"objective": "under review", "project_id": projectID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create task: %d", status)
	}
	reviewedTask, _ := taskBody["task_id"].(string)

	// Knowledge attached to the reviewed task, and other project knowledge.
	inScope := map[string]any{
		"category": "lesson", "content": map[string]any{"summary": "in-scope-note"},
		"project_id": projectID, "task_id": reviewedTask,
	}
	if st, out := owner.do(http.MethodPost, "/memory/entries", inScope); st != http.StatusCreated {
		t.Fatalf("publish in-scope: %d %v", st, out)
	}
	publishKnowledge(t, owner, projectID, "out-of-scope-note")

	status, body := owner.do(http.MethodPost, "/projects/"+projectID+"/invite", map[string]any{
		"agent_id": reviewerID, "reviewer": true,
		"scope": reviewedTask, "expires_in_seconds": 3600,
	})
	if status != http.StatusCreated {
		t.Fatalf("reviewer invite: expected 201, got %d: %v", status, body)
	}
	if body["status"] != "reviewer" {
		t.Fatalf("expected reviewer status, got %v", body["status"])
	}
	status, _ = reviewer.do(http.MethodPost, "/tasks/"+reviewedTask+"/claim", nil)
	if status != http.StatusForbidden {
		t.Fatalf("reviewer claim: expected 403, got %d", status)
	}
	status, _ = reviewer.do(http.MethodPost, "/tasks/"+reviewedTask+"/verify", map[string]any{
		"target_message_id": "message-does-not-matter", "verdict": "verified",
	})
	if status != http.StatusForbidden {
		t.Fatalf("reviewer verify: expected 403, got %d", status)
	}

	seen := searchSummaries(t, reviewer, "note")
	for _, s := range seen {
		if s == "out-of-scope-note" {
			t.Fatal("reviewer saw project knowledge outside its scope")
		}
	}
	found := false
	for _, s := range seen {
		if s == "in-scope-note" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reviewer cannot see the task it was invited to review: %v", seen)
	}
}

// A reviewer grant without a scope or an expiry is unbounded membership under
// another name, so it must be refused (RFC-1250 §8).
func TestReviewerInviteRequiresScopeAndExpiry(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	guest := register(t, srv.URL)
	guestID := mustAgentID(t, guest)

	projectID := createProject(t, owner, map[string]any{"name": "audited", "visibility": "closed"})

	for _, body := range []map[string]any{
		{"agent_id": guestID, "reviewer": true, "expires_in_seconds": 60},
		{"agent_id": guestID, "reviewer": true, "scope": "task-x"},
		{"agent_id": guestID, "scope": "task-x"},
	} {
		status, out := owner.do(http.MethodPost, "/projects/"+projectID+"/invite", body)
		if status != http.StatusBadRequest {
			t.Errorf("invite %v: expected 400, got %d: %v", body, status, out)
		}
	}
}
