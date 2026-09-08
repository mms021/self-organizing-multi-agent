package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateUsesOwnerOnlyAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := State{AgentID: "agent", Token: "test-secret", BaseURL: "http://localhost"}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadState(path); err == nil {
		t.Fatal("loaded world-readable key")
	}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	got, ok, err := LoadState(path)
	if err != nil || !ok || got != state {
		t.Fatalf("load: %+v %v %v", got, ok, err)
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadState(link); err == nil {
		t.Fatal("loaded symlink")
	}
	if err := SaveState(link, state); err == nil {
		t.Fatal("saved through symlink")
	}
}

func TestStateRejectsMalformedAndOversizedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantError     bool
	}{
		{"broken-json", `{"token":`, true},
		{"trailing-json", `{"agent_id":"a","token":"k"} {}`, true},
		{"oversized", strings.Repeat(" ", 65537), true},
		{"missing-token", `{"agent_id":"a"}`, false},
		{"missing-agent", `{"token":"k"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			state, ok, err := LoadState(path)
			if (err != nil) != tc.wantError || ok || state != (State{}) {
				t.Fatalf("state=%+v ok=%v err=%v", state, ok, err)
			}
		})
	}
	if _, ok, err := LoadState(filepath.Join(t.TempDir(), "missing.json")); err != nil || ok {
		t.Fatalf("missing file: %v %v", ok, err)
	}
	if _, _, err := LoadState(t.TempDir()); err == nil {
		t.Fatal("directory accepted as credential file")
	}
}

func TestStateSaveReplacesFileWithoutTruncatingOldReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := State{AgentID: "a", Token: "old-key"}
	if err := SaveState(path, old); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldInfo, err := reader.Stat()
	if err != nil {
		t.Fatal(err)
	}
	next := State{AgentID: "a", Token: "new-key"}
	if err := SaveState(path, next); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Fatal("save overwrote the existing inode")
	}
	data, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(data), "old-key") || strings.Contains(string(data), "new-key") {
		t.Fatal("old reader observed truncated/replaced content")
	}
	got, ok, err := LoadState(path)
	if err != nil || !ok || got != next {
		t.Fatalf("new state: %+v %v", got, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, ".agent-state-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary credential files leaked: %v %v", files, err)
	}
	// Invalid parent must not damage a previously written state file.
	if err := SaveState(filepath.Join(path, "child.json"), old); err == nil {
		t.Fatal("regular file accepted as parent directory")
	}
	got, ok, err = LoadState(path)
	if err != nil || !ok || got != next {
		t.Fatal("failed save damaged existing credentials")
	}
}
