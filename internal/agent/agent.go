package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"aichatdeck/internal/idgen"
	"aichatdeck/internal/model"
)

// Agent is one participant: a platform connection, a brain, and the loop
// that turns "there are open tasks" into claimed, done, verified work.
type Agent struct {
	Client *Client
	Brain  Brain
	Name   string
	Caps   []string
	Log    *log.Logger

	// PollWait is how long an idle iteration long-polls the inbox before
	// giving up and looking for tasks again.
	PollWait int
}

// Bootstrap walks RFC-1400 §2: read the manifest, register, announce READY.
func (a *Agent) Bootstrap(ctx context.Context) error {
	manifest, err := a.Client.Manifest(ctx)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	a.logf("manifest: protocol_version=%v platform=%v", manifest["protocol_version"], manifest["platform_id"])

	caps := make([]model.Capability, 0, len(a.Caps))
	for _, c := range a.Caps {
		caps = append(caps, model.Capability{Name: c})
	}
	if err := a.Client.Register(ctx, model.RegisterRequest{
		Capabilities: caps,
		Skills:       a.Caps,
		Operator:     a.Name,
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	a.logf("registered as %s", a.Client.AgentID)

	// The ZZ marker (prompt.md) confirms the agent is operating with the
	// instructions loaded — visible to anyone reading the message log.
	if err := a.sendStatus(ctx, "idle", "ZZ agent "+a.Name+" ready"); err != nil {
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

// handleInbox verifies RESULT messages for tasks this agent created. Other
// message types are logged and left alone — nothing in this milestone
// requires answering them.
func (a *Agent) handleInbox(ctx context.Context, waitSeconds int) (bool, error) {
	msgs, err := a.Client.Inbox(ctx, waitSeconds)
	if err != nil {
		return false, fmt.Errorf("inbox: %w", err)
	}

	acted := false
	for _, msg := range msgs {
		if msg.Type != "RESULT" || msg.TaskID == "" {
			a.logf("inbox: %s from %s (ignored)", msg.Type, msg.Sender)
			continue
		}

		task, err := a.Client.GetTask(ctx, msg.TaskID)
		if err != nil {
			a.logf("inbox: cannot load task %s: %v", msg.TaskID, err)
			continue
		}
		if task.CreatedBy != a.Client.AgentID {
			continue // not mine to verify
		}
		if task.Status != model.TaskClaimed {
			continue // already resolved
		}

		var payload model.ResultPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			a.logf("inbox: bad RESULT payload in %s: %v", msg.MessageID, err)
			continue
		}

		verdict, rationale, err := a.Brain.Verify(ctx, task, payload.Summary)
		if err != nil {
			return acted, fmt.Errorf("brain verify: %w", err)
		}

		resp, err := a.Client.VerifyTask(ctx, task.TaskID, model.VerifyRequest{
			TargetMessageID: msg.MessageID,
			Verdict:         verdict,
			Rationale:       rationale,
		})
		if err != nil {
			a.logf("verify %s: %v", task.TaskID, err)
			continue
		}
		a.logf("verified %s: %s -> %s", task.TaskID, verdict, resp.Task.Status)
		acted = true
	}
	return acted, nil
}

// takeOneTask claims a single OPEN task this agent didn't create, does the
// work, and reports it as a RESULT message to the task's creator.
func (a *Agent) takeOneTask(ctx context.Context) (bool, error) {
	tasks, err := a.Client.ListTasks(ctx, model.TaskOpen)
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
		_ = a.sendStatus(ctx, "working", "on "+claimed.TaskID)

		// Look up what the group already knows before doing the work again
		// (RFC-1300 §8). A failure here is not fatal — working without prior
		// knowledge is worse, not impossible.
		prior, kerr := a.Client.SearchKnowledge(ctx, claimed.Objective, claimed.ProjectID, 10)
		if kerr != nil {
			a.logf("knowledge search failed, working without it: %v", kerr)
		} else if len(prior) > 0 {
			a.logf("found %d prior knowledge entries for %s", len(prior), claimed.TaskID)
		}

		summary, status, err := a.Brain.Work(ctx, claimed, prior)
		if err != nil {
			return true, fmt.Errorf("brain work: %w", err)
		}

		if err := a.sendResult(ctx, claimed, summary, status); err != nil {
			return true, fmt.Errorf("send result: %w", err)
		}
		a.logf("reported %s: %s", claimed.TaskID, status)

		a.publishLesson(ctx, claimed, summary, status)
		_ = a.sendStatus(ctx, "idle", "done with "+claimed.TaskID)
		return true, nil
	}
	return false, nil
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

func (a *Agent) sendStatus(ctx context.Context, state, detail string) error {
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
		Recipient:       model.BroadcastRecipient,
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
