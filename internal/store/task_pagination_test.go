package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"aichatdeck/internal/model"
)

func TestTaskMatchingPagination(t *testing.T) {
	for _, mode := range []string{"capabilities", "tools", "both"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := openTestDB(t)
			agent := mustCreateAgent(t, s.Agents)
			filter := model.TaskFilter{Status: model.TaskOpen, Limit: 2}
			if mode != "tools" {
				filter.RequiredCapabilities = []string{"testing", "quote'"}
			}
			if mode != "capabilities" {
				filter.RequiredTools = []string{"shell", "quote'"}
			}
			var expected []string
			for i := 0; i < 55; i++ {
				req := model.CreateTaskRequest{Objective: fmt.Sprintf("task %d", i)}
				match := i == 15 || i == 30 || i == 45 || i == 54
				if !match {
					req.RequiredCapabilities = []string{"unavailable"}
					req.RequiredTools = []string{"unavailable"}
					// With both filters, passing only one must not suffice.
					if mode == "both" && i%2 == 0 {
						req.RequiredCapabilities = []string{"testing"}
					}
					if mode == "both" && i%2 != 0 {
						req.RequiredTools = []string{"shell"}
					}
				} else if i != 54 {
					// Any overlap remains the policy; complete coverage is not required.
					req.RequiredCapabilities = []string{"testing", "other"}
					req.RequiredTools = []string{"shell", "other"}
				}
				task, err := s.Tasks.Create(ctx, agent.AgentID, req, "")
				if err != nil {
					t.Fatal(err)
				}
				// Equal timestamps exercise the task_id tie-breaker deterministically.
				id := fmt.Sprintf("ordered-%03d", i)
				if _, err := s.Tasks.db.ExecContext(ctx, `UPDATE tasks SET task_id=?, created_at=? WHERE task_id=?`, id, "2026-01-01T00:00:00Z", task.TaskID); err != nil {
					t.Fatal(err)
				}
				if match {
					expected = append(expected, id)
				}
			}
			var got []string
			for page := 0; page < 3; page++ {
				tasks, next, err := s.Tasks.List(ctx, agent.AgentID, filter)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 2 {
					t.Fatalf("page %d: expected 2 matching tasks, got %d", page, len(tasks))
				}
				for _, task := range tasks {
					got = append(got, task.TaskID)
				}
				if next == "" {
					break
				}
				if next == filter.Cursor {
					t.Fatal("cursor did not advance")
				}
				filter.Cursor = next
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("got %v, want %v", got, expected)
			}
			filter.Cursor = ""
			filter.RequiredCapabilities = []string{"no-match"}
			filter.RequiredTools = []string{"no-match"}
			tasks, next, err := s.Tasks.List(ctx, agent.AgentID, filter)
			if err != nil || len(tasks) != 1 || tasks[0].TaskID != "ordered-054" || next != "" {
				t.Fatalf("unqualified task: %+v cursor=%q err=%v", tasks, next, err)
			}
		})
	}
}
