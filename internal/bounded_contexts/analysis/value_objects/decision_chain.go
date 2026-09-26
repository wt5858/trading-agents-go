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

	// Model 是这次发言真正落到的模型名（路由解析之后的）。
	// 同一条链上不同成员可以跑在不同模型上，不透出这一列，
	// 「为什么这一位又贵又慢」就只能靠猜。
	Model string
	// ToolRounds 是工具调用的往返轮数，Truncated 表示撞到了轮数上限、产出可能不完整。
	ToolRounds int
	Truncated  bool
	// CacheHit 为真时 TotalTokens 与 CostUSD 都是 0，因为这次发言取自缓存。
	// 少了这一列，界面上会出现「零成本却有完整报告」的条目，看起来像计费漏记。
	CacheHit bool
	// ToolCalls 是这次发言调用过的工具明细。
	//
	// 它是回答「这位分析师的结论是在拿到哪些数据的情况下写的」的唯一依据——
	// 工具失败在本系统里是静默的（失败被包装成一句说明回灌给模型，发言照常成功），
	// 因此「财务数据没取到」这件事除了这里，在任何地方都看不出来。
	ToolCalls []ChainToolCall

	Failed     bool
	FailReason string
}

// ChainToolCall 是决策链视角下的一次工具调用。
//
// 本上下文自己定义而不是复用 agent 的 ToolCallRecord，理由同 DecisionChain：
// analysis 不认识另一个限界上下文的类型。翻译发生在 agent 那一侧。
type ChainToolCall struct {
	Round int
	Name  string
	OK    bool
	// FailReason 仅在 OK 为 false 时有值。
	FailReason string
	DurationS  decimal.Decimal
	// ResultChars 是真正回灌给模型的字符数，Truncated 表示原始结果被截断过。
	ResultChars int
	Truncated   bool
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
