package domain_services

import (
	"context"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// DecisionChainService 把一次运行的轨迹翻译成 analysis 上下文的决策链。
//
// # 为什么单独一个服务
//
// 它与 EngineService 的依赖完全不同：引擎要行情、要指标、要运行时、要发事件，
// 而这里只读一个集合。挂进引擎意味着任何一个想看决策链的地方都得先把
// 整台引擎装配起来，测试里也要跟着塞一堆用不上的桩。
//
// # 翻译在这一侧
//
// 与 Engine 把 StageOutcome 翻成 analysis_vo.PhaseOutcome 是同一个道理：
// analysis 上下文不该认识 AgentKind、Phase 这些属于本上下文的类型。
// 十几行翻译的代价，换的是 analysis 不必为了「本上下文多了一位成员」而改动。
type DecisionChainService struct {
	runs *repositories.AnalysisRunRepository
}

var _ analysis_services.DecisionChainReader = (*DecisionChainService)(nil)

func NewDecisionChainService(runs *repositories.AnalysisRunRepository) *DecisionChainService {
	return &DecisionChainService{runs: runs}
}

// DecisionChain 按运行 ID 取决策链。
func (s *DecisionChainService) DecisionChain(ctx context.Context, runID string) (*analysis_vo.DecisionChain, error) {
	if s.runs == nil {
		return nil, custom_errors.Unavailable("未配置分析轨迹仓储")
	}
	trace, err := s.runs.FindByRunID(ctx, runID)
	if err != nil {
		return nil, err
	}

	links := make([]analysis_vo.ChainLink, 0, len(trace.Turns))
	for _, t := range trace.Turns {
		links = append(links, toChainLink(t))
	}

	chain := analysis_vo.DecisionChain{
		TaskID:    trace.RunID,
		Symbol:    trace.Code.FullSymbol(),
		TradeDate: trace.TradeDate.String(),
		Links:     links,
		Verdict:   verdictOf(trace),
		Usage: analysis_vo.TokenUsage{
			PromptTokens:     trace.Usage.PromptTokens,
			CompletionTokens: trace.Usage.CompletionTokens,
			TotalTokens:      trace.Usage.TotalTokens,
			Calls:            trace.Usage.Calls,
			CostUSD:          trace.Usage.CostUSD,
		},
		DurationS:  msToSeconds(trace.Duration.Milliseconds()),
		Failed:     trace.Failed,
		FailReason: trace.FailReason,
	}
	return &chain, nil
}

func toChainLink(t value_objects.TurnRecord) analysis_vo.ChainLink {
	return analysis_vo.ChainLink{
		Seq:       t.Seq,
		Agent:     t.Kind.String(),
		AgentName: t.Kind.DisplayName(),
		Phase:     t.Phase.String(),
		Stance:    stanceOf(t.Kind),
		// 失败的发言没有正文可截，Claim 留空，失败原因在 FailReason 里。
		Claim:   value_objects.ClaimOf(t.Content),
		Content: t.Content,

		DurationS:   msToSeconds(t.DurationMS()),
		TotalTokens: t.Usage.TotalTokens,
		// 成本直取轨迹里存下的那一份，不拿 token 数重乘单价：
		// 单价表会随厂商调价而变，重算出来的历史成本不是当初真花掉的钱。
		CostUSD: t.Usage.CostUSD,

		Failed:     t.Failed,
		FailReason: t.FailReason,
	}
}

// stanceOf 由成员身份决定立场。
//
// 这张表是契约的直接映射，不是对报告正文的猜测：
// 多头研究员的职责就是论证买入，这写在 crew.go 的提示词身份里。
func stanceOf(k value_objects.AgentKind) analysis_vo.Stance {
	switch k {
	case value_objects.KindBullResearcher:
		return analysis_vo.StanceBullish
	case value_objects.KindBearResearcher:
		return analysis_vo.StanceBearish
	case value_objects.KindRiskAggressive:
		return analysis_vo.StanceAggressive
	case value_objects.KindRiskConservative:
		return analysis_vo.StanceConservative
	case value_objects.KindRiskNeutral:
		return analysis_vo.StanceNeutral
	case value_objects.KindResearchManager, value_objects.KindRiskManager:
		return analysis_vo.StanceArbiter
	}
	return analysis_vo.StanceAnalysis
}

// verdictOf 组装终裁。
//
// 归属直接取轨迹里存着的那一个，绝不在这里重新推导。
// 这里曾经按「风控经理有没有发言」自行判定，结果与聚合的降级条件分叉：
// 风控经理正常写了报告、但末尾结构化块格式坏掉时，结论已经回落到交易员的方案，
// 归属却还写着风控经理终裁——而这正是 ChainVerdict.DecidedBy 存在的意义。
// 现在判定只有一处（AnalysisContext.FinalDecision），落库一次，读路径直取。
func verdictOf(trace value_objects.RunTrace) analysis_vo.ChainVerdict {
	decidedBy := trace.Decision.DecidedBy
	v := analysis_vo.ChainVerdict{
		Decision:  trace.Decision.Decision,
		Reasoning: trace.Decision.Decision.Reasoning,
	}
	// 归属为空表示谁都没给出方向，此时不编造一个终裁者。
	if decidedBy != "" {
		v.DecidedBy = decidedBy.String()
		v.DecidedByName = decidedBy.DisplayName()
	}
	return v
}

// msToSeconds 毫秒转秒。
// 与 engine_service.go 的 toPhaseOutcome 同样用 decimal 而不是 float64：
// 这些秒数会被前端相加，浮点累加会让同一批数字在不同顺序下得到不同的总和。
func msToSeconds(ms int64) decimal.Decimal {
	return decimal.NewFromInt(ms).DivRound(thousand, 3)
}
