package httpapi_test

import (
	"net/http"
	"testing"
)

func joinProject(t *testing.T, owner, guest *client, projectID, guestID string) {
	t.Helper()
	if st, out := owner.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": guestID}); st != http.StatusCreated {
		t.Fatalf("invite: %d %v", st, out)
	}
	if st, out := guest.do(http.MethodPost, "/projects/"+projectID+"/members/"+guestID+"/decide",
		map[string]any{"accept": true}); st != http.StatusOK {
		t.Fatalf("accept: %d %v", st, out)
	}
}

// Ownership moves only when the nominee agrees: an agent cannot be made
// responsible for a project behind its back (RFC-1250 §9, and the
// counterparty-as-approver case of RFC-1600 §8).
func TestOwnershipTransferRequiresAcceptance(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	heir := register(t, srv.URL)
	heirID := mustAgentID(t, heir)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "estate", "visibility": "closed"})
	joinProject(t, owner, heir, projectID, heirID)

	status, body := owner.do(http.MethodPost, "/projects/"+projectID+"/transfer",
		map[string]any{"new_owner": heirID})
	if status != http.StatusOK {
		t.Fatalf("nominate: expected 200, got %d: %v", status, body)
	}
	if body["pending_owner"] != heirID {
		t.Fatalf("expected a pending nomination, got %v", body["pending_owner"])
	}
	// The nomination alone changes nothing.
	if body["owner"] != ownerID {
		t.Fatalf("ownership moved before acceptance: %v", body["owner"])
	}

	// An uninvolved agent cannot accept on the nominee's behalf.
	stranger := register(t, srv.URL)
	status, _ = stranger.do(http.MethodPost, "/projects/"+projectID+"/transfer/decide",
		map[string]any{"accept": true})
	if status != http.StatusNotFound {
		t.Errorf("a stranger accepting a transfer: expected 404, got %d", status)
	}

	status, body = heir.do(http.MethodPost, "/projects/"+projectID+"/transfer/decide",
		map[string]any{"accept": true})
	if status != http.StatusOK {
		t.Fatalf("accept transfer: expected 200, got %d: %v", status, body)
	}
	if body["owner"] != heirID {
		t.Fatalf("expected ownership to move, got %v", body["owner"])
	}
	if body["pending_owner"] != nil {
		t.Errorf("pending nomination should be cleared, got %v", body["pending_owner"])
	}

	// Losing ownership is not being removed: the old owner stays a member.
	status, members := heir.do(http.MethodGet, "/projects/"+projectID+"/members", nil)
	if status != http.StatusOK {
		t.Fatalf("member list: %d", status)
	}
	list, _ := members["members"].([]any)
	stillThere := false
	for _, m := range list {
		row, _ := m.(map[string]any)
		if row["agent_id"] == ownerID && row["status"] == "active" {
			stillThere = true
		}
	}
	if !stillThere {
		t.Error("the previous owner was dropped from the project along with the title")
	}
}

// Declining leaves everything as it was.
func TestOwnershipTransferDeclined(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	heir := register(t, srv.URL)
	heirID := mustAgentID(t, heir)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "estate", "visibility": "closed"})
	joinProject(t, owner, heir, projectID, heirID)

	if st, _ := owner.do(http.MethodPost, "/projects/"+projectID+"/transfer",
		map[string]any{"new_owner": heirID}); st != http.StatusOK {
		t.Fatalf("nominate: %d", st)
	}
	status, body := heir.do(http.MethodPost, "/projects/"+projectID+"/transfer/decide",
		map[string]any{"accept": false})
	if status != http.StatusOK {
		t.Fatalf("decline: expected 200, got %d: %v", status, body)
	}
	if body["owner"] != ownerID || body["pending_owner"] != nil {
		t.Fatalf("a declined transfer changed something: %v", body)
	}
}

// A closed project can only be handed to someone already inside it.
func TestOwnershipTransferToOutsiderRefused(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	outsider := register(t, srv.URL)
	outsiderID := mustAgentID(t, outsider)

	projectID := createProject(t, owner, map[string]any{"name": "estate", "visibility": "closed"})

	status, body := owner.do(http.MethodPost, "/projects/"+projectID+"/transfer",
		map[string]any{"new_owner": outsiderID})
	if status != http.StatusBadRequest {
		t.Fatalf("transfer to a non-member: expected 400, got %d: %v", status, body)
	}
}

// RFC-1250 §9 forbids a project with nobody responsible for it. When the
// owner leaves, the longest-standing coordinator inherits.
func TestOwnerDepartureHandsOverToCoordinator(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	deputy := register(t, srv.URL)
	deputyID := mustAgentID(t, deputy)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "succession", "visibility": "closed"})
	joinProject(t, owner, deputy, projectID, deputyID)

	// Only the owner appoints coordinators.
	status, _ := deputy.do(http.MethodPost, "/projects/"+projectID+"/members/"+deputyID+"/role",
		map[string]any{"role": "coordinator"})
	if status != http.StatusForbidden {
		t.Errorf("self-appointment as coordinator: expected 403, got %d", status)
	}

	status, body := owner.do(http.MethodPost, "/projects/"+projectID+"/members/"+deputyID+"/role",
		map[string]any{"role": "coordinator"})
	if status != http.StatusOK || body["role"] != "coordinator" {
		t.Fatalf("appoint coordinator: expected 200/coordinator, got %d: %v", status, body)
	}

	status, body = owner.do(http.MethodDelete, "/projects/"+projectID+"/members/"+ownerID, nil)
	if status != http.StatusOK {
		t.Fatalf("owner leaving: expected 200, got %d: %v", status, body)
	}
	if body["owner"] != deputyID {
		t.Fatalf("expected the coordinator to inherit, got %v", body["owner"])
	}
	if body["status"] != "active" {
		t.Errorf("a project with a successor must stay active, got %v", body["status"])
	}

	// The heir really holds the title, not just the record: it can administer.
	newcomer := register(t, srv.URL)
	newcomerID := mustAgentID(t, newcomer)
	if st, out := deputy.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": newcomerID}); st != http.StatusCreated {
		t.Fatalf("new owner cannot invite: %d %v", st, out)
	}
}

// With no coordinator to inherit, the project is archived rather than left
// ownerless (RFC-1250 §9).
func TestOwnerDepartureArchivesWhenNobodyCanInherit(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	member := register(t, srv.URL)
	memberID := mustAgentID(t, member)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "orphan", "visibility": "closed"})
	joinProject(t, owner, member, projectID, memberID) // a plain member, not a coordinator

	status, body := owner.do(http.MethodDelete, "/projects/"+projectID+"/members/"+ownerID, nil)
	if status != http.StatusOK {
		t.Fatalf("owner leaving: expected 200, got %d: %v", status, body)
	}
	if body["status"] != "archived" {
		t.Fatalf("expected the project to be archived, got %v", body["status"])
	}
	if body["owner"] != ownerID {
		t.Errorf("an archived project keeps its last owner on record, got %v", body["owner"])
	}

	// An archived project cannot change hands.
	status, _ = owner.do(http.MethodPost, "/projects/"+projectID+"/transfer",
		map[string]any{"new_owner": memberID})
	if status != http.StatusConflict {
		t.Errorf("transferring an archived project: expected 409, got %d", status)
	}
}

// After inheriting, an owner who is still flagged a coordinator must not be
// able to inherit from themselves: their departure has to reach a different
// successor, or archive the project.
func TestDepartingOwnerCannotSucceedThemselves(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	deputy := register(t, srv.URL)
	deputyID := mustAgentID(t, deputy)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "chain", "visibility": "closed"})
	joinProject(t, owner, deputy, projectID, deputyID)
	if st, _ := owner.do(http.MethodPost, "/projects/"+projectID+"/members/"+deputyID+"/role",
		map[string]any{"role": "coordinator"}); st != http.StatusOK {
		t.Fatalf("appoint coordinator: %d", st)
	}

	// The coordinator inherits — and keeps the coordinator flag on its row.
	if st, _ := owner.do(http.MethodDelete, "/projects/"+projectID+"/members/"+ownerID, nil); st != http.StatusOK {
		t.Fatalf("owner leaving: %d", st)
	}

	// Now the new owner leaves. Nobody else is a coordinator, so the project
	// must be archived rather than left with a departed owner in charge.
	status, body := deputy.do(http.MethodDelete, "/projects/"+projectID+"/members/"+deputyID, nil)
	if status != http.StatusOK {
		t.Fatalf("second departure: expected 200, got %d: %v", status, body)
	}
	if body["status"] != "archived" {
		t.Fatalf("the departing owner inherited from themselves: project is %v under %v",
			body["status"], body["owner"])
	}
}

// A coordinator may run the roster, but not rewrite the chain of authority:
// only the owner appoints and dismisses coordinators (RFC-1250 §9).
func TestCoordinatorPowersAreBounded(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	deputy := register(t, srv.URL)
	guest := register(t, srv.URL)
	deputyID := mustAgentID(t, deputy)
	guestID := mustAgentID(t, guest)
	ownerID := mustAgentID(t, owner)

	projectID := createProject(t, owner, map[string]any{"name": "delegated", "visibility": "closed"})
	joinProject(t, owner, deputy, projectID, deputyID)
	if st, _ := owner.do(http.MethodPost, "/projects/"+projectID+"/members/"+deputyID+"/role",
		map[string]any{"role": "coordinator"}); st != http.StatusOK {
		t.Fatalf("appoint coordinator: %d", st)
	}

	// A coordinator can bring people in...
	if st, out := deputy.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": guestID}); st != http.StatusCreated {
		t.Fatalf("coordinator inviting: expected 201, got %d: %v", st, out)
	}
	// ...and cannot appoint further coordinators,
	if st, _ := deputy.do(http.MethodPost, "/projects/"+projectID+"/members/"+guestID+"/role",
		map[string]any{"role": "coordinator"}); st != http.StatusForbidden {
		t.Errorf("coordinator appointing a coordinator: expected 403, got %d", st)
	}
	// ...nor remove the owner,
	if st, _ := deputy.do(http.MethodDelete, "/projects/"+projectID+"/members/"+ownerID, nil); st != http.StatusForbidden {
		t.Errorf("coordinator removing the owner: expected 403, got %d", st)
	}
	// ...nor hand the project to themselves.
	if st, _ := deputy.do(http.MethodPost, "/projects/"+projectID+"/transfer",
		map[string]any{"new_owner": deputyID}); st != http.StatusForbidden {
		t.Errorf("coordinator transferring ownership: expected 403, got %d", st)
	}
}

// Only an active member can hold authority: a pending invitation is not yet
// a seat at the table.
func TestCoordinatorMustBeActiveMember(t *testing.T) {
	srv := newTestServer(t)
	owner := register(t, srv.URL)
	pending := register(t, srv.URL)
	pendingID := mustAgentID(t, pending)

	projectID := createProject(t, owner, map[string]any{"name": "waiting", "visibility": "closed"})
	if st, _ := owner.do(http.MethodPost, "/projects/"+projectID+"/invite",
		map[string]any{"agent_id": pendingID}); st != http.StatusCreated {
		t.Fatalf("invite: %d", st)
	}

	status, _ := owner.do(http.MethodPost, "/projects/"+projectID+"/members/"+pendingID+"/role",
		map[string]any{"role": "coordinator"})
	if status != http.StatusConflict {
		t.Errorf("promoting a pending invitation: expected 409, got %d", status)
	}
}
