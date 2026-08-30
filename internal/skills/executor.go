package skills

import (
	"context"

	"cody/internal/conversation"
	"cody/internal/tools"
)

// SkillHost 是 Executor 驱动内联模式 skill 所需的 Agent 状态切片。
// 由 *agent.Agent 实现；这里声明为接口，是为了让 skills 包不导入 agent 包
// （一旦 LoadSkillTool 开始引用 skills.Catalog，导入就会形成循环）。
type SkillHost interface {
	// ActivateSkill 记录 skill 激活，用于追踪（/skills 列表
	// 和压缩恢复）。正文不会每轮重新注入。
	ActivateSkill(name, body string)
	// ToolRegistry 暴露实时的 tools.Registry，让 executor 能注册
	// 目录类工具。取名 ToolRegistry（而非 Registry）是因为
	// *agent.Agent 已有一个导出的 Registry 字段，而 Go 禁止
	// 方法/字段同名冲突。
	ToolRegistry() *tools.Registry
}

// SkillForkHost 在 SkillHost 之上扩展了运行隔离子 agent 的能力。
// 由 TUI 层（持有 LLM client + agent 构造器）实现，并传入 Executor.RunFork。
// 把它与 SkillHost 分开，让单元测试可以只打桩 fork 相关行为，而不必
// 伪造整套子 agent 运行时。
type SkillForkHost interface {
	SkillHost
	// RunSubAgent 以 `body` 作为第一条用户消息，在一个用 `seed` 预置（已按
	// ForkContext 策略准备好）的全新对话中运行子 agent，并返回最终的助手文本。
	// ctx 取消应中止子 agent。
	RunSubAgent(ctx context.Context, body string, seed []conversation.Message, model string) (string, error)
	// SnapshotParentMessages 暴露父对话的消息，让 executor 能按 `fork_context`
	// 构建 seed。实现可以返回浅拷贝；executor 不得修改该切片。
	SnapshotParentMessages() []conversation.Message
}

// RunInline 在宿主 agent 上记录 skill 激活，并返回渲染后的 prompt 正文。
// 调用方（斜杠命令处理器）把返回的正文作为用户消息提交到主对话中——它以普通
// 消息的形式存在，不会每轮重新注入。
func RunInline(_ context.Context, skill *Skill, args string, host SkillHost) (string, error) {
	body := skill.Render(args)
	host.ActivateSkill(skill.Meta.Name, body)
	return body, nil
}

// RunFork 在隔离的子 agent 中执行 skill，并返回最终的助手文本。
// 子 agent 不会修改主对话；调用方（斜杠命令处理器）应把返回的字符串
// 作为助手消息插入主聊天历史。
//
// 历史继承方式由 skill.Meta.ForkContext 决定：
//   - "full"：   用父对话的完整消息历史为子 agent 做 seed
//   - "recent"： 用最后 5 条父消息做 seed
//   - "none"：   不预置（默认；如同全新会话一样隔离）
func RunFork(ctx context.Context, skill *Skill, args string, host SkillForkHost) (string, error) {
	body := skill.Render(args)
	seed := buildForkSeed(skill.Meta.ForkContext, host.SnapshotParentMessages())
	return host.RunSubAgent(ctx, body, seed, skill.Meta.Model)
}

// buildForkSeed 按 ForkContext 策略对父消息历史进行切片。
// 对 "none" 或未知值返回 nil，让子 agent 从全新状态开始。
//
// "full" 目前不做 LLM 侧的摘要——它逐字复制父切片。未来若上下文窗口
// 成为问题，可以进一步接入 compact.Summarise；眼下把它做得与 "recent"
// 相同、只是上限更高就足够了。
func buildForkSeed(mode string, parent []conversation.Message) []conversation.Message {
	switch mode {
	case "full":
		out := make([]conversation.Message, len(parent))
		copy(out, parent)
		return out
	case "recent":
		if len(parent) <= 5 {
			out := make([]conversation.Message, len(parent))
			copy(out, parent)
			return out
		}
		out := make([]conversation.Message, 5)
		copy(out, parent[len(parent)-5:])
		return out
	default:
		return nil
	}
}
