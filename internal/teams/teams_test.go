package teams

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileMailBoxRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mb := NewFileMailBox(dir)

	if err := mb.Send("alice", FileMailMessage{From: "bob", Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := mb.Send("alice", FileMailMessage{From: "carol", Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	unread, err := mb.ReadUnread("alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(unread) != 2 {
		t.Fatalf("expected 2 unread, got %d", len(unread))
	}

	if err := mb.MarkAllRead("alice"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	unread2, _ := mb.ReadUnread("alice")
	if len(unread2) != 0 {
		t.Errorf("expected 0 unread after MarkAllRead, got %d", len(unread2))
	}
}

func TestFileMailBoxConcurrentSends(t *testing.T) {
	dir := t.TempDir()
	mb := NewFileMailBox(dir)

	const n = 20
	var wg sync.WaitGroup
	sendErrs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := mb.Send("dest", FileMailMessage{From: "sender", Text: "msg"}); err != nil {
				sendErrs <- err
			}
		}(i)
	}
	wg.Wait()
	close(sendErrs)
	// 发送失败会直接导致收件箱少消息，先把错误暴露出来，避免只看到条数对不上
	for err := range sendErrs {
		t.Errorf("send failed: %v", err)
	}

	got, err := mb.ReadUnread("dest")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != n {
		t.Errorf("expected %d messages after concurrent sends, got %d", n, len(got))
	}
}

func TestTeamManagerCRUD(t *testing.T) {
	// 每个用例用独立的 teams 目录，避免复跑时消息累积到同一个收件箱
	useTempHome(t)
	tm := NewTeamManager()

	team := tm.CreateTeam("alpha", ModeInProcess)
	if team == nil {
		t.Fatal("CreateTeam returned nil")
	}
	if got := tm.GetTeam("alpha"); got != team {
		t.Errorf("Get should return same team instance")
	}
	if names := tm.ListTeams(); len(names) != 1 || names[0] != "alpha" {
		t.Errorf("ListTeams = %v, want [alpha]", names)
	}
	tm.DeleteTeam("alpha")
	if got := tm.GetTeam("alpha"); got != nil {
		t.Error("DeleteTeam did not remove team")
	}
}

// TestSendMessageToolRoutesToLead 锁定修复了这个 bug：队友调用 SendMessage(to="lead", ...)
// 时报 "recipient 'lead' not found in any team"，因为 lead 从未被注册为 Member。
// 该工具必须识别 LeadName，并通过发送者所在团队的信箱投递，这样 lead 在下一轮扫描时
// 才能读到回复。
func TestSendMessageToolRoutesToLead(t *testing.T) {
	// 每个用例用独立的 teams 目录，避免复跑时消息累积到同一个收件箱
	useTempHome(t)
	tm := NewTeamManager()
	team := tm.CreateTeam("demo", ModeInProcess)
	team.AddMember("alice", nil, nil, "")

	tool := &SendMessageTool{TeamMgr: tm, SenderName: "alice"}
	res := tool.Execute(context.Background(), map[string]any{
		"to":      LeadName,
		"content": "here is the README summary",
	})
	if res.IsError {
		t.Fatalf("SendMessage to lead errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, LeadName) {
		t.Errorf("expected confirmation mentioning %q, got %q", LeadName, res.Output)
	}

	msgs, err := team.MailBox.ReadUnread(LeadName)
	if err != nil {
		t.Fatalf("read lead inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in lead inbox, got %d", len(msgs))
	}
	if msgs[0].From != "alice" || msgs[0].Text != "here is the README summary" {
		t.Errorf("unexpected message: %+v", msgs[0])
	}
}

// TestSendMessageToolUnknownSenderToLead 覆盖失败路径：如果发送者不在任何团队中，
// 发给 lead 的消息无法选定信箱，必须报出明确错误，而不是静默丢弃消息。
func TestSendMessageToolUnknownSenderToLead(t *testing.T) {
	// 每个用例用独立的 teams 目录，避免复跑时消息累积到同一个收件箱
	useTempHome(t)
	tm := NewTeamManager()
	tm.CreateTeam("demo", ModeInProcess) // 未添加任何成员

	tool := &SendMessageTool{TeamMgr: tm, SenderName: "ghost"}
	res := tool.Execute(context.Background(), map[string]any{
		"to":      LeadName,
		"content": "anyone?",
	})
	if !res.IsError {
		t.Fatalf("expected error when sender has no team, got: %s", res.Output)
	}
}

var _ = filepath.Join
var _ = os.Getwd
