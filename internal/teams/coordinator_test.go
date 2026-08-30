package teams

import (
	"context"
	"os"
	"strings"
	"testing"

	"cody/internal/prompt"
)

// Lead 在 coordinator 模式下只做调度，不下场碰代码。
func TestCoordinatorBlocksCodeTools(t *testing.T) {
	blocked := []string{"ReadFile", "WriteFile", "EditFile", "Glob", "Grep", "Bash"}
	for _, name := range blocked {
		if IsCoordinatorTool(name) {
			t.Errorf("%s 不应出现在 coordinator 工具集里，看代码和改代码都该派给队员", name)
		}
	}
}

// 任务表是队员之间协调用的，Lead 靠 task-notification 掌握进度。
func TestCoordinatorBlocksTaskBoardTools(t *testing.T) {
	for _, name := range []string{"TaskCreate", "TaskGet", "TaskList", "TaskUpdate"} {
		if IsCoordinatorTool(name) {
			t.Errorf("%s 属于队员的协调工具，不该给 Lead", name)
		}
	}
}

func TestCoordinatorAllowsSchedulingTools(t *testing.T) {
	for _, name := range []string{"Agent", "SendMessage", "TaskStop", "SyntheticOutput"} {
		if !IsCoordinatorTool(name) {
			t.Errorf("%s 是调度必需的工具，缺了 Lead 没法干活", name)
		}
	}
}

// TeamDelete 是拆除 Team 的唯一入口，而 coordinator 模式由「是否存在 Team」触发。
// 一旦把它挡在外面，Lead 建完 Team 就再也退不出 coordinator 模式。
func TestCoordinatorKeepsTeamDeleteToAvoidLockIn(t *testing.T) {
	if !IsCoordinatorTool("TeamDelete") {
		t.Fatal("TeamDelete 必须放行，否则 Lead 会被锁死在 coordinator 模式里")
	}
}

func TestCoordinatorToolFilterIsStatic(t *testing.T) {
	filter := CoordinatorToolFilter(true)
	if filter == nil {
		t.Fatal("enabled 为 true 时应返回过滤器")
	}

	// 配置说了算，从第一轮起就收窄，不看有没有团队
	if filter("Bash") || filter("ReadFile") || filter("TeamCreate") {
		t.Error("开启后就该只放行白名单，与团队是否存在无关")
	}
	if !filter("Agent") || !filter("SendMessage") {
		t.Error("调度工具应始终放行")
	}
}

// 调度指引和工具收窄必须同时生效：只收窄不给指引，
// Lead 只会发现自己读不了文件，却不知道该派队员去读。
func TestCoordinatorActiveFnTracksToolFilter(t *testing.T) {
	filter := CoordinatorToolFilter(true)
	active := CoordinatorActiveFn(true)
	if active == nil {
		t.Fatal("enabled 为 true 时应返回判定函数")
	}
	if !active() {
		t.Error("开启后应始终生效")
	}
	// 两者判定必须一致，否则会出现「工具收窄了但没给指引」的状态
	if active() != !filter("Bash") {
		t.Error("指引注入与工具收窄的判定条件不一致")
	}
}

func TestCoordinatorDisabledReturnsNil(t *testing.T) {
	if CoordinatorToolFilter(false) != nil {
		t.Error("关闭时不应返回过滤器")
	}
	if CoordinatorActiveFn(false) != nil {
		t.Error("关闭时不应返回判定函数")
	}
}

// TaskStop 停的是队员，不是后台任务表里的条目：coordinator 模式下
// Lead 通过 Agent 工具加 team_name 派出去的是队员，后台任务表里没有它们。
func TestTaskStopStopsTeammate(t *testing.T) {
	useTempHome(t)
	mgr := NewTeamManager()
	team := mgr.CreateTeam("squad", ModeInProcess)
	member := team.AddMember("scout", nil, nil, "anthropic")
	member.Active = true
	stopped := false
	member.Cancel = func() { stopped = true }

	tool := &TaskStopTool{TeamMgr: mgr}
	res := tool.Execute(context.Background(), map[string]any{"teammate": "scout"})
	if res.IsError {
		t.Fatalf("停一个在跑的队员不该报错：%s", res.Output)
	}
	if !stopped {
		t.Error("队员的 Cancel 没有被调用")
	}
	if member.Active {
		t.Error("停止后 Active 应为 false")
	}
}

func TestTaskStopOnUnknownTeammate(t *testing.T) {
	useTempHome(t)
	mgr := NewTeamManager()
	mgr.CreateTeam("squad", ModeInProcess)

	tool := &TaskStopTool{TeamMgr: mgr}
	res := tool.Execute(context.Background(), map[string]any{"teammate": "ghost"})
	if !res.IsError {
		t.Error("停一个不存在的队员应该报错")
	}
}

// 已经停下的队员再停一次不该报错，避免模型拿着报错反复重试
func TestTaskStopOnIdleTeammate(t *testing.T) {
	useTempHome(t)
	mgr := NewTeamManager()
	team := mgr.CreateTeam("squad", ModeInProcess)
	team.AddMember("scout", nil, nil, "anthropic")

	tool := &TaskStopTool{TeamMgr: mgr}
	res := tool.Execute(context.Background(), map[string]any{"teammate": "scout"})
	if res.IsError {
		t.Errorf("停一个已经空闲的队员不该报错：%s", res.Output)
	}
}

// Cody 的内建类型是 general-purpose / plan / explore，没有 worker
func TestCoordinatorPromptUsesRealSubagentType(t *testing.T) {
	p := prompt.CoordinatorReminder(1)
	if strings.Contains(p, `subagent_type: "worker"`) || strings.Contains(p, "subagent_type `worker`") {
		t.Error("提示词提到了不存在的 subagent_type")
	}
}

// 提示词列出的工具必须就是白名单放行的那几个，否则模型会去调用被过滤掉的工具
func TestCoordinatorPromptListsOnlyAllowedTools(t *testing.T) {
	p := prompt.CoordinatorReminder(1)
	toolsSection := p[strings.Index(p, "## 2. Your Tools"):strings.Index(p, "### Worker Results")]
	for name := range CoordinatorAllowedTools {
		if !strings.Contains(toolsSection, "**"+name+"**") {
			t.Errorf("白名单里的 %s 没有出现在提示词的工具清单里", name)
		}
	}
	for _, name := range []string{"ReadFile", "Bash", "Grep", "TaskCreate", "TeamCreate"} {
		if strings.Contains(toolsSection, "**"+name+"**") {
			t.Errorf("提示词的工具清单列了被过滤掉的 %s", name)
		}
	}
}

// 指引里描述的回传格式必须和 DrainLeadMailbox 真正投递的一致，
// 否则 Lead 会照着一个不存在的字段去找队员名。
func TestCoordinatorPromptMatchesTeamNotificationFormat(t *testing.T) {
	p := prompt.CoordinatorReminder(1)
	if !strings.Contains(p, "<team-notification") || !strings.Contains(p, "from=") {
		t.Error("指引应描述 <team-notification> 与 from= 的真实格式")
	}
	// <task_id> 是后台子 agent 的通道，coordinator 模式下 Lead 用不到
	if strings.Contains(p, "<task_id>") {
		t.Error("指引描述的是后台子 agent 的通道，不是队员回传的通道")
	}
}

// 这份指引 8KB 出头，而 AddSystemReminder 是纯 append，
// 每轮原样重发会把这个模式省下来的上下文又填回去。
func TestCoordinatorReminderGoesSparseAfterFirstTurn(t *testing.T) {
	full := prompt.CoordinatorReminder(1)
	second := prompt.CoordinatorReminder(2)
	if len(second) >= len(full) {
		t.Fatalf("第二轮应发精简版，实际 %d 字节 vs 首轮 %d 字节", len(second), len(full))
	}
	// 精简版仍要守住最容易被忘掉的硬约束
	for _, must := range []string{"cannot read files", "TaskStop", "from="} {
		if !strings.Contains(second, must) {
			t.Errorf("精简版丢了关键约束：%s", must)
		}
	}
	// 隔一段时间要复述全文，避免长会话里彻底漂移
	var sawFull bool
	for i := 2; i <= 12; i++ {
		if prompt.CoordinatorReminder(i) == full {
			sawFull = true
			break
		}
	}
	if !sawFull {
		t.Error("长会话中应周期性复述全文")
	}
}

// 三个入口（TUI / remote / print）都要装上团队工具并接同一套 coordinator 判定，
// 否则同一个功能在不同入口行为不一致：有的能派队员，有的建了团队也派不出去。
func TestCoordinatorWiringIsSameAcrossEntrypoints(t *testing.T) {
	roots := map[string]string{
		"tui":    "../tui/tui.go",
		"remote": "../remote/server.go",
		"print":  "../../cmd/cody/print.go",
	}
	// 每个入口都必须出现的装配片段
	required := []string{
		"teams.TeamCreateTool",
		"teams.TeamDeleteTool",
		"teams.TaskStopTool",
		"tools.SyntheticOutputTool",
		"CoordinatorToolFilter",
		"CoordinatorActiveFn",
		"DrainLeadMailbox",
	}
	for entry, path := range roots {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读不到 %s 的源码：%v", entry, err)
		}
		for _, want := range required {
			if !strings.Contains(string(src), want) {
				t.Errorf("%s 入口缺少 %s，coordinator 在这个入口是残的", entry, want)
			}
		}
	}
}

// coordinator 模式下 TeamCreate 不在白名单里，Agent 工具必须能自己把团队建起来，
// 否则 Lead 想派第一个队员就卡住了。
func TestTeamCreateNotNeededUnderCoordinator(t *testing.T) {
	if IsCoordinatorTool("TeamCreate") {
		t.Error("TeamCreate 不该在白名单里，Agent 工具会自动建团队")
	}
	// 收尾要靠 TeamDelete：队员挂在 Team 上，得有办法停掉它们
	if !IsCoordinatorTool("TeamDelete") {
		t.Error("TeamDelete 应保留，否则团队无法收尾")
	}
}
