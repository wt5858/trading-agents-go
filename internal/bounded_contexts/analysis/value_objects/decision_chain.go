package value_objects

import (
	"github.com/shopspring/decimal"
)

// Stance 是一次发言在辩论里站的位置。
//
// 它不是从报告正文里猜出来的，而是由成员身份直接决定的：
// 多头研究员的职责就是论证买入，空头的职责就是反驳，这写在他们的契约里。
// 让解析器去正文里找「我认为应该买入」既不可靠，也会把
// 「空头承认某个利好成立」这种句子误判成立场反转。
type Stance string

const (
	// StanceBullish / StanceBearish 是多空辩论的两方。
	StanceBullish Stance = "bullish"
	StanceBearish Stance = "bearish"
	// StanceAggressive / StanceConservative / StanceNeutral 是风控辩论的三方。
	StanceAggressive   Stance = "aggressive"
	StanceConservative Stance = "conservative"
	StanceNeutral      Stance = "neutral"
	// StanceArbiter 是裁决者：研究经理与风控经理。他们不站队，只收敛分歧。
	StanceArbiter Stance = "arbiter"
	// StanceAnalysis 是不参与辩论的陈述者：六位分析师与交易员。
	StanceAnalysis Stance = "analysis"
)

func (s Stance) String() string { return string(s) }

func (s Stance) DisplayName() string {
	switch s {
	case StanceBullish:
		return "看多"
	case StanceBearish:
		return "看空"
	case StanceAggressive:
		return "激进"
	case StanceConservative:
		return "保守"
	case StanceNeutral:
		return "中性"
	case StanceArbiter:
		return "裁决"
	case StanceAnalysis:
		return "陈述"
	}
	return "未知"
}

// ChainLink 是决策链上的一环：一位成员的一次发言。
type ChainLink struct {
	// Seq 是真实完成顺序，从 1 起。
	Seq       int
	Agent     string
	AgentName string
	Phase     string
	Stance    Stance
	// Claim 是这次发言的核心论点，从正文里截出来的一句话。
	// 它是给「一屏看完整条链」用的，完整论证在 Content 里。
	Claim   string
	Content string

	DurationS   decimal.Decimal
	TotalTokens int
	CostUSD     decimal.Decimal

	Failed     bool
	FailReason string
}

// ChainVerdict 是决策链末端的终裁。
type ChainVerdict struct {
	// DecidedBy 是真正给出这个结论的成员。
	//
	// 它必须显式记下来而不是默认写「风控经理」：深度低于 3 的分析根本不跑风控阶段，
	// 结论是从交易员的方案里解析出来的。把那种情况也标成风控终裁，
	// 等于在界面上凭空造出一位没发过言的裁决者。
	DecidedBy     string
	DecidedByName string
	Decision      Decision
	// Reasoning 是终裁理由，取自决策块里的「决策依据」。
	Reasoning string
}

// DecisionChain 是一次分析的完整决策链：从分析师陈述、多空辩论、交易方案，
// 到风控辩论与终裁，按真实发生顺序串成一条。
//
// 它是纯读值对象，没有生命周期也没有不变式要守——一次跑完的分析不会再变。
// 数据来自 agent 上下文的运行轨迹，由那一侧翻译成本上下文的形状：
// 本上下文不认识 AgentKind、Phase 这些另一个限界上下文的类型。
type DecisionChain struct {
	TaskID    string
	Symbol    string
	TradeDate string

	// Links 按 Seq 升序，也就是真实完成顺序。
	// 并行阶段（六位分析师、三位风控辩手）的相对顺序因此每次都可能不同，
	// 这是如实记录而不是缺陷：谁先出结果本身就是一条有用的信息。
	Links   []ChainLink
	Verdict ChainVerdict

	Usage     TokenUsage
	DurationS decimal.Decimal
	// Failed 表示这次分析整体没跑完，链是断的。
	Failed     bool
	FailReason string
}

// LinksOfPhase 取某一阶段的环节，顺序不变。
func (c DecisionChain) LinksOfPhase(phase string) []ChainLink {
	out := make([]ChainLink, 0, len(c.Links))
	for _, l := range c.Links {
		if l.Phase == phase {
			out = append(out, l)
		}
	}
	return out
}
