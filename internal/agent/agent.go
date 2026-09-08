package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

// Agent is one participant: a platform connection, a brain, and the loop
// that turns "there are open tasks" into claimed, done, verified work.
type Agent struct {
	Client       *Client
	Brain        Brain
	Name         string
	Caps         []string
	Tools        []model.Tool
	artifactHTTP *http.Client // isolated fetch transport; nil uses secure defaults
	// OnOperatorAnswer lets the runtime resume its own pending workflow.
	OnOperatorAnswer       func(context.Context, string, string) error
	pendingOperatorAnswers []model.Envelope
	Log                    *log.Logger

	// PollWait is how long an idle iteration long-polls the inbox before
	// giving up and looking for tasks again.
	PollWait int

	// StatePath, when set, persists the issued credential so a restart
	// resumes the same identity instead of registering a new agent.
	StatePath string
}

// Bootstrap walks RFC-1400 §2: read the manifest, register, announce READY.
//
// When StatePath is set, a previously issued credential is reused so a
// restarted process comes back as the same agent (RFC-1400 §7) instead of
// abandoning the tasks and knowledge its previous incarnation owned.
func (a *Agent) Bootstrap(ctx context.Context) error {
	manifest, err := a.Client.Manifest(ctx)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	a.logf("manifest: protocol_version=%v platform=%v", manifest["protocol_version"], manifest["platform_id"])

	resumed := false
	if a.StatePath != "" {
		state, ok, err := LoadState(a.StatePath)
		if err != nil {
			return err
		}
		if ok && state.BaseURL == a.Client.BaseURL {
			a.Client.AgentID = state.AgentID
			a.Client.Token = state.Token
			resumed = true
		} else if ok {
			a.logf("stored state is for %s, not %s — registering fresh", state.BaseURL, a.Client.BaseURL)
		}
	}

	caps := make([]model.Capability, 0, len(a.Caps))
	for _, c := range a.Caps {
		caps = append(caps, model.Capability{Name: c})
	}
	// With a token in hand this is the idempotent re-register path, which also
	// refreshes the profile; without one it mints a new identity.
	if err := a.Client.Register(ctx, model.RegisterRequest{
		Capabilities: caps,
		Tools:        a.Tools,
		Skills:       a.Caps,
		Operator:     a.Name,
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	if resumed {
		a.logf("resumed as %s", a.Client.AgentID)
	} else {
		a.logf("registered as %s", a.Client.AgentID)
	}
	if a.StatePath != "" {
		if err := SaveState(a.StatePath, State{
			AgentID: a.Client.AgentID,
			Token:   a.Client.Token,
			BaseURL: a.Client.BaseURL,
		}); err != nil {
			return err
		}
	}

	// The ZZ marker (prompt.md) confirms the agent is operating with the
	// instructions loaded — visible to anyone reading the message log.
	if err := a.broadcastStatus(ctx, "idle", "ZZ agent "+a.Name+" ready"); err != nil {
		return fmt.Errorf("initial status: %w", err)
	}
	return nil
}

// RunOnce performs one iteration: verify anything waiting in the inbox, then
// pick up at most one open task. Reports whether it did anything.
func (a *Agent) RunOnce(ctx context.Context, waitSeconds int) (bool, error) {
	did, err := a.handleInbox(ctx, waitSeconds)
	if err != nil {
		return did, err
	}
	worked, err := a.takeOneTask(ctx)
	return did || worked, err
}

// Run loops until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := a.RunOnce(ctx, a.PollWait); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.logf("iteration error: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// handleInbox routes platform ANSWERs to the runtime and verifies RESULTs.
// Unrelated broadcast traffic is excluded by the inbox filter.
func (a *Agent) handleInbox(ctx context.Context, waitSeconds int) (bool, error) {
	msgs, err := a.Client.Inbox(ctx, waitSeconds, "RESULT", "ANSWER")
	if err != nil {
		return false, fmt.Errorf("inbox: %w", err)
	}

	acted := false
	for _, msg := range msgs {
		if msg.Type == "ANSWER" && msg.Sender == "platform" && msg.ReplyTo != nil {
			a.pendingOperatorAnswers = append(a.pendingOperatorAnswers, msg)
		}
	}
	for len(a.pendingOperatorAnswers) > 0 {
		msg := a.pendingOperatorAnswers[0]
		var answer model.AnswerPayload
		if err := json.Unmarshal(msg.Payload, &answer); err != nil {
			return acted, err
		}
		if a.OnOperatorAnswer != nil {
			if err := a.OnOperatorAnswer(ctx, *msg.ReplyTo, answer.Answer); err != nil {
				return acted, err
			}
		} else {
			a.logf("operator answer for %s: %s", *msg.ReplyTo, answer.Answer)
		}
		a.pendingOperatorAnswers = a.pendingOperatorAnswers[1:]
		acted = true
	}
	// RESULT inbox entries only wake the poll. The database queue, not the
	// consumed inbox cursor, is authoritative after crashes and restarts.
	job, err := a.Client.ClaimVerification(ctx)
	if err != nil {
		return acted, fmt.Errorf("verification queue: %w", err)
	}
	if job == nil {
		return acted, nil
	}
	did, err := a.verifyJob(ctx, *job)
	return acted || did, err
}

func (a *Agent) verifyJob(ctx context.Context, job model.VerificationJob) (bool, error) {
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelVerify()
	ctx = verifyCtx
	var err error
	task, msg := job.Task, job.Result
	if task.CreatedBy != a.Client.AgentID {
		return false, fmt.Errorf("verification job belongs to another creator")
	}
	if task.Status != model.TaskSubmitted || task.SubmittedMessageID != msg.MessageID {
		return false, fmt.Errorf("verification job is not the submitted result")
	}

	var payload model.ResultPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return true, fmt.Errorf("verification RESULT payload: %w", err)
	}

	input := VerificationInput{Result: payload}
	var evidence []string
	seen := map[string]bool{}
	unavailable := false
	contentClient := a.artifactHTTP
	if contentClient == nil {
		contentClient = artifactHTTPClient()
	}
	artifactCtx, cancelArtifacts := context.WithTimeout(ctx, 30*time.Second)
	for _, artifactID := range payload.Artifacts {
		if seen[artifactID] {
			continue
		}
		seen[artifactID] = true
		if len(seen) > maxVerificationArtifacts {
			unavailable = true
			break
		}
		artifact, err := a.Client.GetArtifact(artifactCtx, artifactID)
		if err != nil || artifact.ArtifactID != artifactID || artifact.TaskID == nil || *artifact.TaskID != task.TaskID || artifact.CreatedBy != msg.Sender || (artifact.ExpiresAt != nil && !artifact.ExpiresAt.After(time.Now())) {
			unavailable = true
			continue
		}
		input.Artifacts = append(input.Artifacts, artifact)
		content, err := fetchArtifactContent(artifactCtx, contentClient, artifact)
		if err != nil {
			unavailable = true
			a.logf("artifact content unavailable: %v", err)
			continue
		}
		input.Contents = append(input.Contents, content)
		evidence = append(evidence, artifactID)
	}
	cancelArtifacts()
	contentClient.CloseIdleConnections()
	verdict, rationale := "inconclusive", "referenced artifact is unavailable, out of scope, unsupported, exceeds verification limits or failed SHA-256 validation"
	if !unavailable {
		verdict, rationale, err = a.Brain.Verify(ctx, task, input)
		if err != nil {
			return true, fmt.Errorf("brain verify: %w", err)
		}
		if verdict == "verified" && payload.Status != "success" {
			verdict, rationale = "inconclusive", "worker did not report a complete success"
		}
	}

	resp, err := a.Client.VerifyTask(ctx, task.TaskID, model.VerifyRequest{
		AttemptID:       job.AttemptID,
		TargetMessageID: msg.MessageID,
		Verdict:         verdict,
		Rationale:       rationale,
		Evidence:        evidence,
	})
	if err != nil {
		return true, fmt.Errorf("verify %s: %w", task.TaskID, err)
	}
	a.logf("verified %s: %s -> %s", task.TaskID, verdict, resp.Task.Status)
	return true, nil
}

// takeOneTask claims a single OPEN task this agent didn't create, does the
// work, and reports it as a RESULT message to the task's creator.
func (a *Agent) takeOneTask(ctx context.Context) (bool, error) {
	// Ask the server for tasks relevant to this agent. The server repeats the
	// same check when claiming, because a client-side query parameter is only
	// a matching optimisation, never an authorization boundary.
	toolNames := make([]string, 0, len(a.Tools))
	for _, tool := range a.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	tasks, err := a.Client.ListTasks(ctx, model.TaskOpen, a.Caps, toolNames)
	if err != nil {
		return false, fmt.Errorf("list tasks: %w", err)
	}

	for _, task := range tasks {
		if task.CreatedBy == a.Client.AgentID {
			continue // don't grade your own homework: verifying it later would be rejected
		}

		claimed, err := a.Client.ClaimTask(ctx, task.TaskID)
		if err != nil {
			if IsErrorCode(err, model.ErrConflict) {
				continue // someone else got there first — expected, not an error
			}
			return false, fmt.Errorf("claim %s: %w", task.TaskID, err)
		}
		a.logf("claimed %s: %s", claimed.TaskID, claimed.Objective)
		// Progress goes to the one party that cares — the task's creator —
		// rather than to every agent on the platform.
		_ = a.sendStatus(ctx, claimed.CreatedBy, "working", "on "+claimed.TaskID)

		// Look up what the group already knows before doing the work again
		// (RFC-1300 §8). A failure here is not fatal — working without prior
		// knowledge is worse, not impossible.
		prior, kerr := a.Client.SearchKnowledge(ctx, claimed.Objective, claimed.ProjectID, 10)
		if kerr != nil {
			a.logf("knowledge search failed, working without it: %v", kerr)
		} else if len(prior) > 0 {
			a.logf("found %d prior knowledge entries for %s", len(prior), claimed.TaskID)
		}

		workCtx, stopHeartbeat := a.maintainLease(ctx, claimed.TaskID, claimed.ClaimID)
		summary, status, err := a.Brain.Work(workCtx, claimed, prior)
		leaseErr := workCtx.Err()
		stopHeartbeat()
		if err != nil {
			return true, fmt.Errorf("brain work: %w", err)
		}
		if leaseErr != nil {
			return true, fmt.Errorf("claim interrupted: %w", leaseErr)
		}

		if err := a.sendResult(ctx, claimed, summary, status); err != nil {
			return true, fmt.Errorf("send result: %w", err)
		}
		a.logf("reported %s: %s", claimed.TaskID, status)

		a.publishLesson(ctx, claimed, summary, status)
		_ = a.sendStatus(ctx, claimed.CreatedBy, "idle", "done with "+claimed.TaskID)
		return true, nil
	}
	return false, nil
}

// maintainLease renews a claim while a potentially slow brain is working.
func (a *Agent) maintainLease(ctx context.Context, taskID, claimID string) (context.Context, func()) {
	workCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if _, err := a.Client.HeartbeatTask(workCtx, taskID, claimID); err != nil {
					a.logf("heartbeat %s: %v", taskID, err)
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, func() { close(done); cancel() }
}

// publishLesson records what the agent learned into shared memory, scoped to
// the task's project (or globally when the task has none). It lands as a
// `lesson` in `proposed` status — an unverified claim, not an established
// fact (RFC-1300 §5, RFC-1400 §5). Failure is logged, not fatal: the work
// itself is already reported.
func (a *Agent) publishLesson(ctx context.Context, task model.Task, summary, status string) {
	entry, err := a.Client.PublishKnowledge(ctx, model.CreateKnowledgeRequest{
		Category: model.KnowledgeLesson,
		Content: map[string]any{
			"summary":   summary,
			"objective": task.Objective,
			"outcome":   status,
		},
		Source:     task.TaskID,
		Confidence: 0.5, // self-reported and unverified
		Tags:       task.RequiredCapabilities,
		References: []model.Reference{{Type: "task", ID: task.TaskID}},
		ProjectID:  task.ProjectID,
		TaskID:     &task.TaskID,
	})
	if err != nil {
		a.logf("publish lesson for %s failed: %v", task.TaskID, err)
		return
	}
	a.logf("published lesson %s for %s", entry.KnowledgeID, task.TaskID)
}

func (a *Agent) sendResult(ctx context.Context, task model.Task, summary, status string) error {
	payload, err := json.Marshal(model.ResultPayload{
		ClaimID:   task.ClaimID,
		Summary:   summary,
		Status:    status,
		Artifacts: []string{}, // shared memory (RFC-1300) isn't implemented yet
	})
	if err != nil {
		return err
	}
	_, err = a.Client.PostMessage(ctx, model.Envelope{
		MessageID:       idgen.New("message"),
		ProtocolVersion: "1.0",
		Type:            "RESULT",
		Sender:          a.Client.AgentID,
		Recipient:       task.CreatedBy,
		Timestamp:       time.Now().UTC(),
		TaskID:          task.TaskID,
		Priority:        "normal",
		Payload:         payload,
	})
	return err
}

// broadcastStatus announces presence to everyone. Reserved for things the
// whole platform genuinely needs — an agent coming online — not routine
// per-task transitions.
func (a *Agent) broadcastStatus(ctx context.Context, state, detail string) error {
	return a.sendStatus(ctx, model.BroadcastRecipient, state, detail)
}

func (a *Agent) sendStatus(ctx context.Context, recipient, state, detail string) error {
	payload, err := json.Marshal(model.StatusPayload{
		Subject:   "agent",
		SubjectID: a.Client.AgentID,
		State:     state,
		Detail:    detail,
	})
	if err != nil {
		return err
	}
	_, err = a.Client.PostMessage(ctx, model.Envelope{
		MessageID:       idgen.New("message"),
		ProtocolVersion: "1.0",
		Type:            "STATUS",
		Sender:          a.Client.AgentID,
		Recipient:       recipient,
		Timestamp:       time.Now().UTC(),
		Priority:        "low",
		Payload:         payload,
	})
	return err
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	a.Log.Printf("["+a.Name+"] "+format, args...)
}
