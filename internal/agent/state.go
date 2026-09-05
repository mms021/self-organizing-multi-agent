package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is what an agent must keep across restarts to stay the same agent.
// The server already supports idempotent re-registration (RFC-1400 §7): with
// a valid credential it returns the original agent_id and refreshes the
// profile. Without persisting the credential, every restart registers a new
// agent instead, orphaning the tasks and knowledge the old one owned.
type State struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
}

// LoadState reads agent state from path. A missing file is not an error: it
// means this agent has never registered.
func LoadState(path string) (State, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read agent state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false, fmt.Errorf("parse agent state %s: %w", path, err)
	}
	if s.Token == "" || s.AgentID == "" {
		return State{}, false, nil
	}
	return s, true, nil
}

// SaveState writes agent state to path with owner-only permissions — the file
// holds a credential, which is a secret (RFC-1600 §7).
func SaveState(path string, s State) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create agent state dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write agent state: %w", err)
	}
	return nil
}
