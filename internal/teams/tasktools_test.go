package teams

import (
	"context"
	"strings"
	"testing"
)

// newTestTeamManager 把团队目录指向临时目录，避免写进真实项目目录。
func newTestTeamManager(t *testing.T) *TeamManager {
	t.Helper()
	useTempHome(t)
	GetNameRegistry().Clear()
	t.Cleanup(func() { GetNameRegistry().Clear() })
	return NewTeamManager()
}

func TestCreateTeamInitializesEmptyStore(t *testing.T) {
	mgr := newTestTeamManager(t)
	mgr.CreateTeam("myteam", ModeInProcess)
	store := mgr.GetTaskStore("myteam")
	if store == nil || len(store.ListTasks("", "")) != 0 {
		t.Fatalf("new team should have empty shared store")
	}
}

func TestTeamTaskToolsFlow(t *testing.T) {
	mgr := newTestTeamManager(t)
	mgr.CreateTeam("myteam", ModeInProcess)
	ctx := context.Background()

	create := &TaskCreateTool{TeamMgr: mgr, TeamName: "myteam", AgentName: "lead"}
	list := &TaskListTool{TeamMgr: mgr, TeamName: "myteam"}
	update := &TaskUpdateTool{TeamMgr: mgr, TeamName: "myteam"}
	get := &TaskGetTool{TeamMgr: mgr, TeamName: "myteam"}

	created := create.Execute(ctx, map[string]any{"title": "build parser", "assignee": "alice"})
	if created.IsError || !strings.Contains(created.Output, "ID: 1") {
		t.Fatalf("create failed: %+v", created)
	}

	listed := list.Execute(ctx, map[string]any{})
	if !strings.Contains(listed.Output, "[1] build parser") || !strings.Contains(listed.Output, "[alice]") {
		t.Fatalf("list output unexpected: %s", listed.Output)
	}

	updated := update.Execute(ctx, map[string]any{"task_id": "1", "status": "completed"})
	if updated.IsError || !strings.Contains(updated.Output, "status → completed") {
		t.Fatalf("update failed: %+v", updated)
	}

	got := get.Execute(ctx, map[string]any{"task_id": "1"})
	if !strings.Contains(got.Output, "Status:     completed") {
		t.Fatalf("get output unexpected: %s", got.Output)
	}

	if !strings.Contains(list.Execute(ctx, map[string]any{"status": "pending"}).Output, "No tasks found") {
		t.Fatalf("pending filter should be empty")
	}
}

func TestTaskUpdateRejectsInvalidStatus(t *testing.T) {
	mgr := newTestTeamManager(t)
	mgr.CreateTeam("myteam", ModeInProcess)
	ctx := context.Background()

	(&TaskCreateTool{TeamMgr: mgr, TeamName: "myteam"}).Execute(ctx, map[string]any{"title": "t"})
	r := (&TaskUpdateTool{TeamMgr: mgr, TeamName: "myteam"}).Execute(ctx, map[string]any{"task_id": "1", "status": "done"})
	if !r.IsError || !strings.Contains(r.Output, "Invalid status") {
		t.Fatalf("expected invalid status error, got %+v", r)
	}
}

func TestTaskGetMissingIsError(t *testing.T) {
	mgr := newTestTeamManager(t)
	mgr.CreateTeam("myteam", ModeInProcess)
	r := (&TaskGetTool{TeamMgr: mgr, TeamName: "myteam"}).Execute(context.Background(), map[string]any{"task_id": "42"})
	if !r.IsError {
		t.Fatalf("expected error for missing task")
	}
}

func TestDeleteTeamUnregistersMembers(t *testing.T) {
	mgr := newTestTeamManager(t)
	team := mgr.CreateTeam("myteam", ModeInProcess)
	team.Members["alice"] = &Member{Name: "alice"}
	GetNameRegistry().Register("alice", "alice")

	mgr.DeleteTeam("myteam")
	if mgr.GetTeam("myteam") != nil {
		t.Fatalf("team not deleted")
	}
	if GetNameRegistry().Resolve("alice") != "" {
		t.Fatalf("member name not unregistered")
	}
}
