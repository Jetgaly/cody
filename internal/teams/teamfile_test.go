package teams

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTeamFileRoundTrip(t *testing.T) {
	useTempHome(t)

	tm := NewTeamManager()
	team := tm.CreateTeamFull("Refactor Auth", ModeInProcess, "lead", "重构认证模块")
	team.AddMember("alice", nil, nil, "anthropic")
	team.SetMemberMeta("alice", "worker", "claude-sonnet-4-6", "/tmp/wt/alice")

	// 换一个全新的 manager，模拟队员进程或下一次会话
	fresh := NewTeamManager()
	got := fresh.GetTeam("Refactor Auth")
	if got == nil {
		t.Fatal("期望从磁盘重建出团队，实际拿到 nil")
	}
	if got.LeadAgentID != "lead" {
		t.Errorf("LeadAgentID = %q，期望 lead", got.LeadAgentID)
	}
	if got.Description != "重构认证模块" {
		t.Errorf("Description = %q，期望 重构认证模块", got.Description)
	}
	m, ok := got.Members["alice"]
	if !ok {
		t.Fatalf("成员 alice 没恢复出来，现有成员：%v", got.Members)
	}
	if m.AgentType != "worker" || m.Model != "claude-sonnet-4-6" || m.WorktreePath != "/tmp/wt/alice" {
		t.Errorf("成员元信息没对上：%+v", m)
	}
}

func TestTeamFilePathIsSanitized(t *testing.T) {
	useTempHome(t)

	tm := NewTeamManager()
	tm.CreateTeamFull("Refactor Auth!", ModeTmux, "lead", "")

	want := filepath.Join(teamsBaseDir(), "refactor-auth-", "config.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("期望配置落在 %s，stat 失败：%v", want, err)
	}
}

func TestDeleteTeamRemovesDir(t *testing.T) {
	useTempHome(t)

	tm := NewTeamManager()
	tm.CreateTeamFull("gone", ModeInProcess, "lead", "")
	if _, err := os.Stat(teamDir("gone")); err != nil {
		t.Fatalf("建团队后目录应存在：%v", err)
	}

	tm.DeleteTeam("gone")
	if _, err := os.Stat(teamDir("gone")); !os.IsNotExist(err) {
		t.Errorf("拆团队后目录应被清掉，err = %v", err)
	}
	if fresh := NewTeamManager().GetTeam("gone"); fresh != nil {
		t.Errorf("拆掉的团队不该还能从磁盘捞回来")
	}
}

func TestGetTeamMissingReturnsNil(t *testing.T) {
	useTempHome(t)
	if got := NewTeamManager().GetTeam("never-existed"); got != nil {
		t.Errorf("不存在的团队应返回 nil，实际 %+v", got)
	}
}
