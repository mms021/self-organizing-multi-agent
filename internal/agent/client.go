// Package agent is the client side of the platform: a thin SDK over the
// RFC-1700 HTTP surface, a pluggable "brain" that does the actual thinking,
// and the loop that ties them together.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	_ "embed"

	"aichatdeck/internal/model"
)

// SystemPrompt is the operating manual handed to the LLM brain. It is also
// readable on disk at internal/agent/prompt.md.
//
//go:embed prompt.md
var SystemPrompt string

// Client is an agent's connection to the platform. Not safe for concurrent
// use — one agent, one goroutine.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	AgentID string
	Token   string

	cursor string // GET /messages keyset cursor, advanced as the inbox is drained
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var apiErr model.APIError
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.ErrorCode != "" {
			return &apiErr
		}
		return fmt.Errorf("%s %s: http %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// IsErrorCode reports whether err is a platform error with the given
// RFC-1000 §8 error_code (e.g. model.ErrConflict when a task was already
// claimed by someone else).
func IsErrorCode(err error, code string) bool {
	apiErr, ok := err.(*model.APIError)
	return ok && apiErr.ErrorCode == code
}

func (c *Client) Manifest(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	return out, c.do(ctx, http.MethodGet, "/manifest", nil, &out)
}

// Register performs RFC-1400 §7 registration and stores the issued
// agent_id/token on the client. Called with an existing token it is the
// idempotent re-register path, which refreshes the profile in place.
func (c *Client) Register(ctx context.Context, req model.RegisterRequest) error {
	var out model.RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/agents/register", req, &out); err != nil {
		return err
	}
	c.AgentID = out.AgentID
	if out.Credential.Token != "" {
		c.Token = out.Credential.Token
	}
	return nil
}

func (c *Client) CreateTask(ctx context.Context, req model.CreateTaskRequest) (model.Task, error) {
	var out model.Task
	return out, c.do(ctx, http.MethodPost, "/tasks", req, &out)
}

func (c *Client) ListTasks(ctx context.Context, status string) ([]model.Task, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	var out struct {
		Tasks []model.Task `json:"tasks"`
	}
	return out.Tasks, c.do(ctx, http.MethodGet, "/tasks?"+q.Encode(), nil, &out)
}

func (c *Client) GetTask(ctx context.Context, taskID string) (model.Task, error) {
	var out model.Task
	return out, c.do(ctx, http.MethodGet, "/tasks/"+taskID, nil, &out)
}

func (c *Client) ClaimTask(ctx context.Context, taskID string) (model.Task, error) {
	var out model.Task
	return out, c.do(ctx, http.MethodPost, "/tasks/"+taskID+"/claim", nil, &out)
}

func (c *Client) VerifyTask(ctx context.Context, taskID string, req model.VerifyRequest) (model.VerifyResponse, error) {
	var out model.VerifyResponse
	return out, c.do(ctx, http.MethodPost, "/tasks/"+taskID+"/verify", req, &out)
}

func (c *Client) PostMessage(ctx context.Context, env model.Envelope) (model.Envelope, error) {
	var out model.Envelope
	return out, c.do(ctx, http.MethodPost, "/messages", env, &out)
}

// PublishKnowledge writes a record into shared memory (RFC-1300). It lands
// as `proposed` — only verification can promote it further.
func (c *Client) PublishKnowledge(ctx context.Context, req model.CreateKnowledgeRequest) (model.Knowledge, error) {
	var out model.Knowledge
	return out, c.do(ctx, http.MethodPost, "/memory/entries", req, &out)
}

// SearchKnowledge queries shared memory. projectID nil searches every scope;
// a project id returns that project's records plus global ones.
func (c *Client) SearchKnowledge(ctx context.Context, query string, projectID *string, limit int) ([]model.Knowledge, error) {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if projectID != nil {
		q.Set("project_id", *projectID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Entries []model.Knowledge `json:"entries"`
	}
	return out.Entries, c.do(ctx, http.MethodGet, "/memory/search?"+q.Encode(), nil, &out)
}

func (c *Client) CreateArtifact(ctx context.Context, req model.CreateArtifactRequest) (model.Artifact, error) {
	var out model.Artifact
	return out, c.do(ctx, http.MethodPost, "/artifacts", req, &out)
}

func (c *Client) CreateProject(ctx context.Context, req model.CreateProjectRequest) (model.Project, error) {
	var out model.Project
	return out, c.do(ctx, http.MethodPost, "/projects", req, &out)
}

// Inbox drains new messages addressed to this agent, advancing the cursor so
// the same message is never returned twice. waitSeconds > 0 turns this into a
// long-poll (the server wakes it via Redis pub/sub the moment something
// arrives) — 0 returns immediately with whatever is already stored.
func (c *Client) Inbox(ctx context.Context, waitSeconds int) ([]model.Envelope, error) {
	q := url.Values{}
	if c.cursor != "" {
		q.Set("cursor", c.cursor)
	}
	if waitSeconds > 0 {
		q.Set("wait_seconds", strconv.Itoa(waitSeconds))
	}
	var out struct {
		Messages   []model.Envelope `json:"messages"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := c.do(ctx, http.MethodGet, "/messages?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if out.NextCursor != "" {
		c.cursor = out.NextCursor
	}
	return out.Messages, nil
}
