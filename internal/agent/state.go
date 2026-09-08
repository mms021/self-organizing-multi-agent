package agent

import (
	"encoding/json"
	"fmt"
	"io"
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
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read agent state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return State{}, false, fmt.Errorf("agent state must be a regular owner-only file (chmod 600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return State{}, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return State{}, false, err
	}
	if !os.SameFile(info, opened) || opened.Mode().Perm()&0077 != 0 {
		return State{}, false, fmt.Errorf("agent state changed while opening")
	}
	var s State
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return State{}, false, err
	}
	if len(data) > 65536 {
		return State{}, false, fmt.Errorf("agent state exceeds size limit")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false, fmt.Errorf("parse agent state: %w", err)
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
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular agent state path")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".agent-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
