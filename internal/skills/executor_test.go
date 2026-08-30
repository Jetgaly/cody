package skills

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cody/internal/conversation"
	"cody/internal/tools"
)

// stubHost 捕获每一次 SkillHost 调用，使测试能验证执行器
// 触发了正确的副作用。它同时实现 SkillHost 和 SkillForkHost，
// 因此同一个夹具即可覆盖 RunInline 和 RunFork。
type stubHost struct {
	activated     map[string]string
	registry      *tools.Registry
	parentMsgs    []conversation.Message
	subAgentBody  string
	subAgentSeed  []conversation.Message
	subAgentReply string
	subAgentErr   error
}

func newStubHost(reg *tools.Registry) *stubHost {
	return &stubHost{activated: map[string]string{}, registry: reg}
}

func (s *stubHost) ActivateSkill(name, body string) { s.activated[name] = body }
func (s *stubHost) ToolRegistry() *tools.Registry   { return s.registry }
func (s *stubHost) SnapshotParentMessages() []conversation.Message {
	out := make([]conversation.Message, len(s.parentMsgs))
	copy(out, s.parentMsgs)
	return out
}

func (s *stubHost) RunSubAgent(_ context.Context, body string, seed []conversation.Message, _ string) (string, error) {
	s.subAgentBody = body
	s.subAgentSeed = seed
	return s.subAgentReply, s.subAgentErr
}

func TestRunInlineActivates(t *testing.T) {
	reg := tools.NewRegistry()
	host := newStubHost(reg)
	skill := &Skill{
		Meta: SkillMeta{
			Name: "commit",
			Mode: "inline",
		},
		PromptBody: "Body with $ARGUMENTS",
		BodyLoaded: true,
	}

	body, err := RunInline(context.Background(), skill, "extra ctx", host)
	if err != nil {
		t.Fatalf("RunInline: %v", err)
	}
	if !strings.Contains(body, "extra ctx") {
		t.Errorf("body did not interpolate $ARGUMENTS: %q", body)
	}
	if host.activated["commit"] == "" {
		t.Errorf("ActivateSkill was not called")
	}
}

func TestRunForkPassesSeed(t *testing.T) {
	reg := tools.NewRegistry()
	host := newStubHost(reg)
	host.parentMsgs = []conversation.Message{
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: "msg2"},
		{Role: "user", Content: "msg3"},
		{Role: "assistant", Content: "msg4"},
		{Role: "user", Content: "msg5"},
		{Role: "assistant", Content: "msg6"},
	}
	host.subAgentReply = "review complete"

	skill := &Skill{
		Meta: SkillMeta{
			Name:        "review",
			Mode:        "fork",
			ForkContext: "recent",
		},
		PromptBody: "Review this: $ARGUMENTS",
	}

	out, err := RunFork(context.Background(), skill, "main.go", host)
	if err != nil {
		t.Fatalf("RunFork: %v", err)
	}
	if out != "review complete" {
		t.Errorf("unexpected fork output: %q", out)
	}
	if !strings.Contains(host.subAgentBody, "main.go") {
		t.Errorf("sub-agent body missing args: %q", host.subAgentBody)
	}
	if len(host.subAgentSeed) != 5 {
		t.Errorf("recent seed should be last 5; got %d", len(host.subAgentSeed))
	}
	if host.subAgentSeed[0].Content != "msg2" {
		t.Errorf("seed should start at msg2 (last-5 window), got %q", host.subAgentSeed[0].Content)
	}
}

func TestRunForkContextNone(t *testing.T) {
	reg := tools.NewRegistry()
	host := newStubHost(reg)
	host.parentMsgs = []conversation.Message{
		{Role: "user", Content: "should not leak"},
	}
	skill := &Skill{
		Meta:       SkillMeta{Name: "isolated", Mode: "fork", ForkContext: "none"},
		PromptBody: "Pure isolation.",
	}
	_, err := RunFork(context.Background(), skill, "", host)
	if err != nil {
		t.Fatalf("RunFork: %v", err)
	}
	if len(host.subAgentSeed) != 0 {
		t.Errorf("none mode must seed nothing; got %d msgs", len(host.subAgentSeed))
	}
}

func TestRunForkPropagatesAgentError(t *testing.T) {
	reg := tools.NewRegistry()
	host := newStubHost(reg)
	host.subAgentErr = errors.New("upstream failure")

	skill := &Skill{Meta: SkillMeta{Name: "review", Mode: "fork"}}
	_, err := RunFork(context.Background(), skill, "", host)
	if err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("expected upstream failure, got %v", err)
	}
}

func TestLoadSkillToolReturnsConfirmation(t *testing.T) {
	reg := tools.NewRegistry()
	cat := NewCatalog()
	cat.Register(&Skill{
		Meta:       SkillMeta{Name: "commit", Mode: "inline"},
		PromptBody: "do commit stuff",
		BodyLoaded: true,
	}, "builtin")

	host := newStubHost(reg)
	tool := &LoadSkillTool{Catalog: cat, Host: host}
	res := tool.Execute(context.Background(), map[string]any{"name": "commit"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "# Skill: commit") || !strings.Contains(res.Output, "do commit stuff") {
		t.Errorf("expected full SOP body in output: %q", res.Output)
	}
	if host.activated["commit"] == "" {
		t.Errorf("ActivateSkill not called")
	}
}

func TestLoadSkillToolUnknown(t *testing.T) {
	reg := tools.NewRegistry()
	cat := NewCatalog()
	host := newStubHost(reg)
	tool := &LoadSkillTool{Catalog: cat, Host: host}

	res := tool.Execute(context.Background(), map[string]any{"name": "missing"})
	if !res.IsError {
		t.Fatalf("expected error for missing skill")
	}
}
