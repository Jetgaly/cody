package teams

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 把每个 teams 测试指向一个一次性的信箱根目录，避免
// 运行测试套件时在仓库里留下 .cody/teams/ 目录。
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "cody-teams-test-")
	if err != nil {
		panic(err)
	}
	// 团队目录是 <home>/.cody/teams，把主目录整个指到临时目录，
	// 整个包的用例都落在沙箱里。Windows 读 USERPROFILE，其余平台读 HOME。
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestIsShutdownRequest(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"[shutdown] please stop", true},
		{"  [shutdown]  ", true},
		{"shutdown", false},
		{"hello [shutdown] there", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsShutdownRequest(FileMailMessage{Text: c.text}); got != c.want {
			t.Errorf("IsShutdownRequest(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestCreateIdleNotification(t *testing.T) {
	msg := CreateIdleNotification("alice", "available")
	if msg.From != "alice" {
		t.Errorf("From = %q, want alice", msg.From)
	}
	if !strings.Contains(msg.Text, "[idle]") {
		t.Errorf("Text missing [idle] marker: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "alice") {
		t.Errorf("Text missing member name: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "available") {
		t.Errorf("Text missing reason: %q", msg.Text)
	}
	if msg.Timestamp == "" {
		t.Error("Timestamp should be set")
	}
}

func TestFormatInboundAsPromptEmpty(t *testing.T) {
	if got := formatInboundAsPrompt(nil); got != "" {
		t.Errorf("empty input should yield empty prompt, got %q", got)
	}
}

func TestFormatInboundAsPromptMultiple(t *testing.T) {
	msgs := []FileMailMessage{
		{From: "lead", Text: "go review file X"},
		{From: "bob", Text: "I'll handle the tests"},
	}
	got := formatInboundAsPrompt(msgs)
	if !strings.Contains(got, "From lead: go review file X") {
		t.Errorf("missing first message: %q", got)
	}
	if !strings.Contains(got, "From bob: I'll handle the tests") {
		t.Errorf("missing second message: %q", got)
	}
	if !strings.Contains(got, "new messages from your team") {
		t.Errorf("missing header: %q", got)
	}
}

func TestWaitForNextPromptOrShutdownShutdown(t *testing.T) {
	dir := t.TempDir()
	team := &Team{
		Name:    "x",
		Mode:    ModeInProcess,
		Members: map[string]*Member{},
		MailBox: NewFileMailBox(dir),
	}

	// 投递一条 shutdown 消息，并验证等待会立即返回 shutdown=true。
	if err := team.MailBox.Send("alice", FileMailMessage{From: LeadName, Text: "[shutdown] done"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prompt, shutdown, err := waitForNextPromptOrShutdown(ctx, team, "alice")
	if err != nil {
		t.Fatalf("waitForNextPromptOrShutdown: %v", err)
	}
	if shutdown == nil {
		t.Errorf("expected a shutdown message, got nil")
	}
	if prompt != "" {
		t.Errorf("expected empty prompt on shutdown, got %q", prompt)
	}
}

func TestWaitForNextPromptOrShutdownMessage(t *testing.T) {
	dir := t.TempDir()
	team := &Team{
		Name:    "x",
		Mode:    ModeInProcess,
		Members: map[string]*Member{},
		MailBox: NewFileMailBox(dir),
	}

	if err := team.MailBox.Send("alice", FileMailMessage{From: LeadName, Text: "do the thing"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prompt, shutdown, err := waitForNextPromptOrShutdown(ctx, team, "alice")
	if err != nil {
		t.Fatalf("waitForNextPromptOrShutdown: %v", err)
	}
	if shutdown != nil {
		t.Error("unexpected shutdown message on regular message")
	}
	if !strings.Contains(prompt, "do the thing") {
		t.Errorf("prompt missing message body: %q", prompt)
	}

	// 信箱应该已被排空。
	leftover, _ := team.MailBox.ReadUnread("alice")
	if len(leftover) != 0 {
		t.Errorf("expected inbox drained, %d unread remain", len(leftover))
	}
}

func TestWaitForNextPromptOrShutdownCancel(t *testing.T) {
	dir := t.TempDir()
	team := &Team{
		Name:    "x",
		Mode:    ModeInProcess,
		Members: map[string]*Member{},
		MailBox: NewFileMailBox(dir),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 在调用返回前取消

	_, _, err := waitForNextPromptOrShutdown(ctx, team, "alice")
	if err == nil {
		t.Error("expected ctx error, got nil")
	}
}

func TestDrainLeadMailbox(t *testing.T) {
	// 用显式的信箱目录构建团队，避免经由 teamsBaseDir() 污染仓库根目录。
	mgr := NewTeamManager()
	t1 := &Team{Name: "alpha", Mode: ModeInProcess, Members: map[string]*Member{}, MailBox: NewFileMailBox(t.TempDir())}
	t2 := &Team{Name: "beta", Mode: ModeInProcess, Members: map[string]*Member{}, MailBox: NewFileMailBox(t.TempDir())}
	mgr.CreateTeamWith(t1)
	mgr.CreateTeamWith(t2)

	_ = t1.MailBox.Send(LeadName, FileMailMessage{From: "ann", Text: "[idle] ann (reason: available)"})
	_ = t2.MailBox.Send(LeadName, FileMailMessage{From: "bob", Text: "[idle] bob (reason: failed)"})

	notes := DrainLeadMailbox(mgr)
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "team=\"alpha\"") || !strings.Contains(joined, "team=\"beta\"") {
		t.Errorf("notes missing team labels: %s", joined)
	}
	if !strings.Contains(joined, "ann") || !strings.Contains(joined, "bob") {
		t.Errorf("notes missing senders: %s", joined)
	}

	// 第二次排空应该什么也拿不到，因为消息现在已被标记为已读。
	if again := DrainLeadMailbox(mgr); len(again) != 0 {
		t.Errorf("expected empty drain after mark-read, got %d", len(again))
	}
}

func TestDrainLeadMailboxNilSafe(t *testing.T) {
	if got := DrainLeadMailbox(nil); got != nil {
		t.Errorf("nil manager should yield nil, got %v", got)
	}
}

func TestBuildTeammateCLIFormat(t *testing.T) {
	cmd, err := BuildTeammateCLI("my team", "alice/dev", "/tmp/work dir")
	if err != nil {
		t.Fatalf("BuildTeammateCLI: %v", err)
	}
	// 空格和斜杠必须加引号，--teammate 必须出现，
	// 且 cd 前缀必须使用传入的 workdir。
	if !strings.Contains(cmd, "--teammate") {
		t.Errorf("command missing --teammate flag: %s", cmd)
	}
	if !strings.Contains(cmd, "--team-name 'my team'") {
		t.Errorf("team-name not quoted with spaces: %s", cmd)
	}
	if !strings.Contains(cmd, "--agent-name alice/dev") {
		t.Errorf("agent-name missing: %s", cmd)
	}
	if !strings.HasPrefix(cmd, "cd '/tmp/work dir'") {
		t.Errorf("missing cd prefix: %s", cmd)
	}
}

func TestSpawnTeammateValidation(t *testing.T) {
	ctx := context.Background()

	// 缺少团队
	if _, err := SpawnTeammate(ctx, TeammateSpawnConfig{MemberName: "x"}); err == nil {
		t.Error("expected error when Team is nil")
	}

	// 缺少名称
	team := NewTeam("t", ModeInProcess)
	if _, err := SpawnTeammate(ctx, TeammateSpawnConfig{Team: team}); err == nil {
		t.Error("expected error when MemberName is empty")
	}

	// 未知模式
	bad := NewTeam("t", "bogus")
	if _, err := SpawnTeammate(ctx, TeammateSpawnConfig{Team: bad, MemberName: "x"}); err == nil {
		t.Error("expected error for unknown team mode")
	}
}

func TestRecordExternalMember(t *testing.T) {
	team := NewTeam("ops", ModeTmux)
	team.recordExternalMember("alice", "pane-1")

	m, ok := team.Members["alice"]
	if !ok {
		t.Fatal("member not recorded")
	}
	if m.PaneID != "pane-1" {
		t.Errorf("PaneID = %q, want pane-1", m.PaneID)
	}
	if !m.Active {
		t.Error("recorded member should be Active=true")
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"safe", "safe"},
		{"hello world", "'hello world'"},
		{"it's", "'it'\\''s'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
