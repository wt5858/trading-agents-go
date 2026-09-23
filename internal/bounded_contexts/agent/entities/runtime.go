package entities

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

// Turn 是交给 Runtime 执行的一次「成员发言」。
type Turn struct {
	Contract value_objects.Contract
	Snapshot ContextSnapshot
}

// TurnResult 是一次发言的产出。
type TurnResult struct {
	Content    string
	Usage      value_objects.Usage
	ToolRounds int
	// Truncated 表示工具循环撞到了轮数上限，产出可能不完整。
	Truncated bool

	// Model 是路由解析之后真正用上的模型名。它由 Runtime 回填而不是由成员自己填：
	// 快照里的 Model 可以是空串（走默认）或一个别名，只有 Runtime 知道最终落到了谁身上。
	Model string
	// PromptChars / PromptDigest 是入参提示词的摘要，用途见 value_objects.TurnRecord。
	// 失败时也要带回来：排查「为什么这位成员挂了」的第一个问题就是它到底看到了什么。
	PromptChars  int
	PromptDigest string
	// CacheHit 表示这份产出直接取自缓存。命中时 Usage 为零值，
	// 因为这次运行确实没有向模型发过一个字。
	CacheHit bool
}

// Runtime 是实体层对「让一位成员真正开口说话」这件事的抽象。
//
// # 为什么这个接口声明在 entities 而不是 domain_services
//
// 和 identity 上下文把 PasswordHasher 放进 entities 是同一个理由：
// 「分析师要先想清楚再发言」是本上下文的业务规则，属于实体；
// 而「用哪家模型、提示词怎么拼、工具循环转几圈」是实现细节，属于 domain_services。
// 把接口放在实体这一侧，依赖方向就是 domain_services -> entities，
// 实体永远不必为了换一个模型厂商而改动。
//
// 实现者是 domain_services.RuntimeService，测试里则是一个返回固定文本的桩。
type Runtime interface {
	// Execute 渲染提示词、驱动工具调用循环，返回这位成员的最终发言。
	Execute(ctx context.Context, turn Turn) (TurnResult, error)
}

// ProgressSink 是编排器向外汇报进度的窄接口。
//
// 它的方法签名与 analysis 上下文的 ProgressReporter 逐字一致，
// 因此那个接口的实现可以直接传进来，中间不需要任何适配器——
// 这是刻意的：多一层适配器就多一个「key 在转换时被改写」的机会，
// 而 key 一旦对不上，进度条会安静地停住，不报任何错。
//
// 另一方面，这里重新声明一次而不是直接 import analysis 的接口：
// entities 依赖另一个限界上下文的 domain_services 会形成一条毫无必要的编译期耦合。
type ProgressSink interface {
	Step(key, detail string)
	StepFailed(key, detail string)
}

// noopSink 用于没有订阅者的场景（批量回测、测试）。
type noopSink struct{}

func (noopSink) Step(string, string)       {}
func (noopSink) StepFailed(string, string) {}

// NoopProgressSink 返回一个什么都不做的进度接收器。
func NoopProgressSink() ProgressSink { return noopSink{} }
