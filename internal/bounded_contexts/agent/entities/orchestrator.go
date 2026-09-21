package entities

import (
	"context"
	"sync"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 并发上限。两个数字都是显式常量而不是裸字面量，因为它们是需要被解释的业务决策。
const (
	// AnalystFanOutLimit 是分析师阶段的并发上限。
	//
	// 取 3 而不是 6（全并行）的理由是限流而非内存：每位分析师一轮至少一次模型调用，
	// 带工具时是三到四次，六位同时开跑意味着瞬时 20+ 个在途请求。
	// 主流厂商的每分钟 token 配额在这个量级上会直接开始 429，
	// 而 429 之后的指数退避带来的总耗时，比一开始就分两批跑更长。
	// 3 让六位分析师分两批完成，峰值并发与批量分析场景（多只票同时跑）也能叠加得住。
	AnalystFanOutLimit = 3

	// RiskFanOutLimit 是风控辩论阶段的并发上限。
	//
	// 恰好等于辩手数量：三位辩手不带重工具（只读已落库的行情与指标），
	// 单轮就能出结论，再压低并发只会平白拉长阶段耗时。
	RiskFanOutLimit = 3
)

// Stage 是编排计划中的一个阶段。
//
// 失败语义刻意不放在 Stage 上，而是取每位成员契约里的 FailurePolicy。
// 原因是多空辩论阶段本身就是混合的：多头、空头可以缺席（还有另一方的论证可用），
// 研究经理不能（它的裁决是下一阶段的唯一输入）。
// 一个阶段一个策略的模型表达不了这种情况，只能把整段降级成「全容错」或「全严格」，
// 两种都是错的。
type Stage struct {
	Phase   value_objects.Phase
	Mode    value_objects.ExecutionMode
	Members []Agent
	// Limit 是并行模式下的并发上限，串行模式忽略。
	Limit int
	// MinSuccess 是本阶段至少要有几位成员成功，0 表示不设下限。
	//
	// 它是「全员容错」的兜底：六位分析师全部失败时，每个人的失败都是可容忍的，
	// 但整体结果是一个空上下文，继续往下跑只会让多空研究员凭空编造论据。
	MinSuccess int
}

// HasStrictMember 判定阶段内是否存在严格成员。
// 并行模式下它决定用 Map（快速失败）还是 Settle（全员结算）。
func (s Stage) HasStrictMember() bool {
	for _, m := range s.Members {
		if !m.Contract().Policy.Tolerant() {
			return true
		}
	}
	return false
}

// StageOutcome 是一个阶段的执行结果，用于回溯与计费归因。
//
// 它是实体层自己的类型，不直接产出 analysis_vo.PhaseOutcome：
// 那是另一个限界上下文的展示用值对象，由引擎在装配 Result 时翻译。
// 中间这一层翻译的代价是十几行代码，换来的是编排器不必知道
// analysis 上下文长什么样。
type StageOutcome struct {
	Phase    value_objects.Phase
	Agents   []value_objects.AgentKind
	Failed   []value_objects.AgentKind
	Duration time.Duration
}

// Plan 是一次分析的完整编排计划。
//
// 计划在开跑前一次性算好，而不是边跑边决定下一步：
// 进度条的步骤集合是在任务创建时按同一套规则生成的
// （analysis/value_objects/progress.go 的 NewProgress），
// 两边必须能对得上。把计划固化成数据，也让「深度 1 到底跑哪几步」
// 这个问题有了一个可以被测试逐条比对的答案。
type Plan struct {
	Stages []Stage
	// Unsupported 是请求里点名了、但花名册里没有对应成员的分析师所占的进度步骤。
	//
	// 它必须被记下来，不能悄悄丢掉：NewProgress 是按 req.Analysts 逐个生成步骤的，
	// 它不认识「这个分析师已下线」这回事。计划这边直接跳过的话，
	// 进度条上就会留下一个永远不亮的格子——任务其实早就跑完了，
	// 而百分比停在 90%，用户与看板都会判定它卡死。
	// 编排器会在开跑前把这些步骤显式标记为失败，让进度如实收尾。
	Unsupported []value_objects.StepKey
}

// NewPlan 依据分析请求与花名册生成编排计划。
//
// 阶段的启停条件与 NewProgress 逐条对应：
//   - 分析师阶段取 req.Analysts（进度里是 "analyst:"+id）；
//   - 深度 >= 2 才有多空辩论（Depth.IncludesDebate）；
//   - 交易阶段恒有；
//   - 深度 >= 3 才有风控阶段（Depth.IncludesRiskPhase）。
//
// 改动这里的任何一条，都必须同步改 NewProgress，否则进度条会停在半路。
// entities 的 plan_progress 测试会把两边全量比对一次，作为这条约束的执行点。
func NewPlan(req analysis_vo.Request, crew Crew) (Plan, error) {
	analysts := make([]value_objects.AgentKind, 0, len(req.Analysts))
	var unsupported []value_objects.StepKey
	for _, id := range req.Analysts {
		kind, err := value_objects.NewAgentKind(id)
		// 未知或已下线的分析师 ID 不报错：它来自前端的勾选框，
		// 混进一个历史遗留的 ID 不该让整次分析失败。但它占着一个进度步骤，
		// 必须记下来交给编排器如实标记为失败。
		if err != nil || !kind.IsAnalyst() || crew.Member(kind) == nil {
			unsupported = append(unsupported, value_objects.AnalystStepKey(id))
			continue
		}
		analysts = append(analysts, kind)
	}

	members := crew.Members(analysts...)
	if len(members) == 0 {
		return Plan{}, custom_errors.Invalid("没有可用的分析师，无法编排分析流程")
	}

	stages := []Stage{{
		Phase:   value_objects.PhaseAnalyst,
		Mode:    value_objects.ModeParallel,
		Members: members,
		Limit:   AnalystFanOutLimit,
		// 至少要有一位分析师活着：一份报告都没有时，
		// 后面十位成员全是在对着空气发言。
		MinSuccess: 1,
	}}

	if req.Depth.IncludesDebate() {
		stages = append(stages, Stage{
			Phase: value_objects.PhaseDebate,
			// 串行是本质需求，不是性能取舍：空头要逐条反驳多头的论证，
			// 研究经理要看完双方才能裁决。并行跑出来的是三段自说自话。
			Mode: value_objects.ModeSequential,
			Members: crew.Members(
				value_objects.KindBullResearcher,
				value_objects.KindBearResearcher,
				value_objects.KindResearchManager,
			),
		})
	}

	stages = append(stages, Stage{
		Phase:   value_objects.PhaseTrading,
		Mode:    value_objects.ModeSequential,
		Members: crew.Members(value_objects.KindTrader),
	})

	if req.Depth.IncludesRiskPhase() {
		stages = append(stages,
			Stage{
				Phase: value_objects.PhaseRisk,
				// 三位辩手互不引用对方的发言，是真正的并行；
				// 他们的分歧由风控经理在下一阶段消化。
				Mode: value_objects.ModeParallel,
				Members: crew.Members(
					value_objects.KindRiskAggressive,
					value_objects.KindRiskConservative,
					value_objects.KindRiskNeutral,
				),
				Limit: RiskFanOutLimit,
				// 不设下限：三位辩手全挂时，风控经理仍可基于交易方案独立终裁，
				// 这比让整次分析作废有价值得多。
			},
			Stage{
				Phase:   value_objects.PhaseRisk,
				Mode:    value_objects.ModeSequential,
				Members: crew.Members(value_objects.KindRiskManager),
			},
		)
	}

	return Plan{Stages: stages, Unsupported: unsupported}, nil
}

// StepKeys 返回本计划会汇报的全部进度键，顺序即执行顺序。
// 它是「计划与进度条一致」这条约束的可测断言点。
func (p Plan) StepKeys() []value_objects.StepKey {
	out := make([]value_objects.StepKey, 0, 16)
	for _, st := range p.Stages {
		for _, m := range st.Members {
			out = append(out, m.Contract().Step)
		}
	}
	return out
}

// Orchestrator 按计划驱动全体成员，是本上下文的流程不变式所在。
//
// 它不知道模型、提示词、数据库的存在：跑一位成员就是调它的 Act，
// 至于 Act 背后发生了什么，由注入的 Runtime 决定。
// 因此这个类型可以被完整地单元测试——测试里的 Runtime 就是一个返回固定文本的桩。
type Orchestrator struct {
	plan Plan
	sink ProgressSink

	// reportMu 串行化进度回调。
	//
	// 并行阶段里三到六个 goroutine 会几乎同时完成，而 ProgressSink 的实现
	// （analysis 上下文的 taskReporter）会去修改 Task 聚合——那是一个
	// 明确声明了「不要并发修改」的聚合。锁加在这里，是因为只有编排器
	// 知道自己正在并行；让每个 sink 实现各自加锁，等于把一个并发约束
	// 分发给所有实现者，迟早有人漏掉。
	reportMu sync.Mutex
}

func NewOrchestrator(plan Plan, sink ProgressSink) *Orchestrator {
	if sink == nil {
		sink = NoopProgressSink()
	}
	return &Orchestrator{plan: plan, sink: sink}
}

func (o *Orchestrator) Plan() Plan { return o.plan }

// Run 按顺序执行全部阶段，返回各阶段结果。
//
// 阶段之间永远是串行的：后一阶段的输入就是前一阶段写进 AnalysisContext 的产出。
// 阶段内部的并行度由 Stage.Mode 决定。
//
// 任何一个阶段返回错误即整体中止：阶段级失败的含义是「这一阶段没有产出」，
// 而每一阶段的产出都是下一阶段的唯一输入。
func (o *Orchestrator) Run(ctx context.Context, rt Runtime, ac *AnalysisContext) ([]StageOutcome, error) {
	// 先给「点了名却没人能上」的步骤收尾。放在最前面是因为进度条是按步骤顺序渲染的，
	// 一个迟迟不表态的格子会被用户读成「卡在这一步」。
	for _, step := range o.plan.Unsupported {
		o.report(false, step, "该分析师当前不可用，已跳过")
	}

	outcomes := make([]StageOutcome, 0, len(o.plan.Stages))
	for _, stage := range o.plan.Stages {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		outcome, err := o.runStage(ctx, stage, rt, ac)
		outcomes = append(outcomes, outcome)
		if err != nil {
			return outcomes, err
		}
	}
	return outcomes, nil
}

func (o *Orchestrator) runStage(ctx context.Context, stage Stage, rt Runtime, ac *AnalysisContext) (StageOutcome, error) {
	started := time.Now()
	outcome := StageOutcome{Phase: stage.Phase, Agents: kindsOf(stage.Members)}

	var (
		failed []value_objects.AgentKind
		err    error
	)
	if stage.Mode.Parallel() {
		failed, err = o.runParallel(ctx, stage, rt, ac)
	} else {
		failed, err = o.runSequential(ctx, stage, rt, ac)
	}

	outcome.Failed = failed
	outcome.Duration = time.Since(started)
	if err != nil {
		return outcome, err
	}

	// MinSuccess 在阶段收尾时统一判定。放在这里而不是边跑边判，
	// 是因为容错阶段本来就要跑完全部成员才知道到底活下来几个。
	if succeeded := len(stage.Members) - len(failed); stage.MinSuccess > 0 && succeeded < stage.MinSuccess {
		return outcome, custom_errors.Unavailable(
			"%s阶段全部失败（%d/%d 成功，至少需要 %d）",
			stage.Phase.DisplayName(), succeeded, len(stage.Members), stage.MinSuccess)
	}
	return outcome, nil
}

// runParallel 并行执行阶段成员。
func (o *Orchestrator) runParallel(ctx context.Context, stage Stage, rt Runtime, ac *AnalysisContext) ([]value_objects.AgentKind, error) {
	limit := stage.Limit
	if limit <= 0 {
		limit = AnalystFanOutLimit
	}

	if stage.HasStrictMember() {
		_, err := concurrency.Map(ctx, stage.Members, limit,
			func(ctx context.Context, m Agent) (struct{}, error) {
				return struct{}{}, o.runMember(ctx, m, rt, ac)
			})
		if err != nil {
			// 快速失败模式下无法分辨「谁先失败」与「谁被取消」，
			// 具体的失败原因已经由 Act 记进了 AnalysisContext。
			return ac.FailedKinds(), err
		}
		return nil, nil
	}

	outcomes, err := concurrency.Settle(ctx, stage.Members, limit,
		func(ctx context.Context, m Agent) (struct{}, error) {
			return struct{}{}, o.runMember(ctx, m, rt, ac)
		})
	if err != nil {
		// Settle 只在父 ctx 被取消时返回错误，单个成员的失败在 outcomes 里。
		return failedKindsOf(stage.Members, outcomes), err
	}
	return failedKindsOf(stage.Members, outcomes), nil
}

// runSequential 串行执行阶段成员，遇到严格成员失败即中止。
//
// 容错成员失败时继续往下跑：多头研究员挂了，空头的论证依然有价值，
// 研究经理也依然能在单方论证的基础上裁决。
func (o *Orchestrator) runSequential(ctx context.Context, stage Stage, rt Runtime, ac *AnalysisContext) ([]value_objects.AgentKind, error) {
	var failed []value_objects.AgentKind
	for _, m := range stage.Members {
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		if err := o.runMember(ctx, m, rt, ac); err != nil {
			failed = append(failed, m.Contract().Kind)
			if !m.Contract().Policy.Tolerant() {
				return failed, err
			}
		}
	}
	return failed, nil
}

// runMember 跑一位成员并汇报进度。
//
// 进度在这里汇报而不是在 Act 里：Act 是实体的行为，不该知道外面有没有人在看进度条；
// 而「什么时候算完成一步」是编排层面的判断。
func (o *Orchestrator) runMember(ctx context.Context, m Agent, rt Runtime, ac *AnalysisContext) error {
	contract := m.Contract()
	if err := m.Act(ctx, rt, ac); err != nil {
		o.report(false, contract.Step, custom_errors.MessageOf(err))
		return err
	}
	o.report(true, contract.Step, contract.DisplayName+"完成")
	return nil
}

func (o *Orchestrator) report(ok bool, step value_objects.StepKey, detail string) {
	if step.IsZero() {
		return
	}
	o.reportMu.Lock()
	defer o.reportMu.Unlock()
	if ok {
		o.sink.Step(step.String(), detail)
		return
	}
	o.sink.StepFailed(step.String(), detail)
}

func kindsOf(members []Agent) []value_objects.AgentKind {
	out := make([]value_objects.AgentKind, 0, len(members))
	for _, m := range members {
		out = append(out, m.Contract().Kind)
	}
	return out
}

// failedKindsOf 按位置把 Settle 的结算结果映射回成员身份。
// Settle 保证 outcomes 与入参等长且按下标对齐，这里依赖的正是那条保证。
func failedKindsOf(members []Agent, outcomes []concurrency.Outcome[struct{}]) []value_objects.AgentKind {
	var failed []value_objects.AgentKind
	for i, o := range outcomes {
		if o.Err != nil && i < len(members) {
			failed = append(failed, members[i].Contract().Kind)
		}
	}
	return failed
}
