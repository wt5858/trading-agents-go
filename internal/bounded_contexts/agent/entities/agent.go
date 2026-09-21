package entities

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Agent 是一位成员的行为契约。
//
// 只有两个方法：它是谁（Contract），以及它干活（Act）。
// 编排器只认这个接口，因此新增一位成员不需要改动编排逻辑的任何一行。
type Agent interface {
	// Contract 返回声明式契约：层、阶段、进度键、工具授权、失败语义。
	Contract() value_objects.Contract
	// Act 执行一次发言，并把产出写回共享上下文。
	//
	// 返回 error 表示这位成员没有产出。是否因此中断整个阶段，
	// 由编排器结合契约里的 FailurePolicy 决定——成员自己不做这个判断，
	// 因为「一个人的失败算不算全队的失败」是编排层面的规则。
	Act(ctx context.Context, rt Runtime, ac *AnalysisContext) error
}

// CrewMember 是全部十四位成员的共同实现。
//
// # 为什么不是十四个结构体
//
// 十四位成员的差异全部落在三处：契约（含提示词身份与工具授权）、前置条件、
// 以及对产出的额外处理。行为骨架——渲染快照、调 Runtime、写回上下文、记账——
// 逐字相同。写成十四份会得到十四份几乎一样的 Act，
// 而这种重复的危险不在于行数，在于「其中一份忘了 RecordFailure」这类偏差
// 只会在那位成员恰好失败时才暴露。
//
// 因此差异用字段表达，骨架只有一份。每位成员仍有自己的具名构造函数
// （NewMarketAnalyst / NewRiskManager ...），阅读与装配时的具体感不受影响。
type CrewMember struct {
	contract value_objects.Contract
	// requires 是前置条件：上游素材不足时直接失败，不去浪费一次模型调用。
	// 它是业务不变式（「没有多空论据就不存在裁决」），所以在实体里。
	requires func(ContextSnapshot) error
	// absorb 是产出的额外处理，目前只有风控经理用到（解析终局决策）。
	absorb func(*AnalysisContext, TurnResult)
}

var _ Agent = (*CrewMember)(nil)

func (m *CrewMember) Contract() value_objects.Contract { return m.contract }

// Act 是全体成员共用的行为骨架。
//
// 三件事按顺序发生，任何一步失败都会被如实记进上下文：
//  1. 取快照并校验前置条件；
//  2. 交给 Runtime 执行（提示词渲染与工具循环都在那一侧）；
//  3. 把产出写回共享上下文，并让特化的 absorb 再处理一次。
//
// 失败路径同样调用 RecordFailure：失败的那次调用是真花了 token 的，
// 不记账会让成本统计长期偏低；而且下游智能体需要知道「情绪面缺席」这件事，
// 缺席和「情绪面中性」是两个完全不同的结论。
func (m *CrewMember) Act(ctx context.Context, rt Runtime, ac *AnalysisContext) error {
	if rt == nil {
		return custom_errors.Internal("%s 缺少运行时", m.contract.DisplayName)
	}
	snapshot := ac.Snapshot()

	if m.requires != nil {
		if err := m.requires(snapshot); err != nil {
			ac.RecordFailure(m.contract.Kind, custom_errors.MessageOf(err), value_objects.Usage{})
			return err
		}
	}

	res, err := rt.Execute(ctx, Turn{Contract: m.contract, Snapshot: snapshot})
	if err != nil {
		// Runtime 即使失败也可能已经消耗了 token（例如工具循环跑了两轮才超时），
		// 所以这里照样把 res.Usage 记进去。
		ac.RecordFailure(m.contract.Kind, custom_errors.MessageOf(err), res.Usage)
		return err
	}
	if res.Content == "" {
		reason := "模型未返回任何内容"
		ac.RecordFailure(m.contract.Kind, reason, res.Usage)
		return custom_errors.Unavailable("%s %s", m.contract.DisplayName, reason)
	}

	ac.PutReport(m.contract.Kind, res.Content, res.Usage)
	if m.absorb != nil {
		m.absorb(ac, res)
	}
	return nil
}
