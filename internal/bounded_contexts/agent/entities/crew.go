package entities

import (
	"slices"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 采样参数按职责分档。它们是业务决策而不是调参玄学：
//   - 分析师要的是可核对的事实陈述，温度必须低；
//   - 多空研究员要的是有说服力的不同视角，温度高一点才吵得起来；
//   - 三位裁决者（研究经理/交易员/风控经理）的产出会被直接采信甚至被解析成结构化决策，
//     温度越低越好，风控经理尤甚——它的末尾块一旦被润色成散文就解析不出来了。
const (
	tempAnalyst   = 0.3
	tempDebater   = 0.7
	tempArbiter   = 0.2
	tempTrader    = 0.3
	tempRiskVoice = 0.6
	tempRiskFinal = 0.1

	tokensAnalyst = 2400
	tokensDebater = 2000
	tokensArbiter = 2600
	tokensFinal   = 3000

	// 工具轮数上限。分析师要查数据，给 3 轮；辩手只做佐证，给 2 轮；
	// 不带工具的裁决者给 0。上限的意义是保证终止：
	// 模型陷入「再查一次」的循环时，只有这个计数器能让一次分析停下来。
	roundsAnalyst = 3
	roundsDebater = 2
	roundsNone    = 0
)

// Crew 是全体成员的花名册，按身份索引。
type Crew map[value_objects.AgentKind]Agent

// NewCrew 组建全部十四位成员。
//
// 工具授权在这里一次性定死：市场分析师拿价格类工具，新闻分析师拿资讯类工具，
// 两位裁决者不拿任何工具。这不只是省 token——多空辩论的前提是双方视角确实不同，
// 如果每个人都能看到全部数据，六份报告会迅速收敛成同一份。
func NewCrew() Crew {
	members := []*CrewMember{
		NewMarketAnalyst(),
		NewFundamentalsAnalyst(),
		NewNewsAnalyst(),
		NewSentimentAnalyst(),
		NewSectorAnalyst(),
		NewIndexAnalyst(),
		NewBullResearcher(),
		NewBearResearcher(),
		NewResearchManager(),
		NewTrader(),
		NewAggressiveRiskDebator(),
		NewConservativeRiskDebator(),
		NewNeutralRiskDebator(),
		NewRiskManager(),
	}
	crew := make(Crew, len(members))
	for _, m := range members {
		crew[m.contract.Kind] = m
	}
	return crew
}

// Member 取一位成员，不存在返回 nil。
func (c Crew) Member(kind value_objects.AgentKind) Agent { return c[kind] }

// Members 按给定顺序取一组成员，缺席的直接跳过。
// 跳过而不是报错：分析师集合由用户在前端勾选，里面混进一个已下线的分析师 ID
// 不该让整次分析失败。
func (c Crew) Members(kinds ...value_objects.AgentKind) []Agent {
	out := make([]Agent, 0, len(kinds))
	for _, k := range kinds {
		if m, ok := c[k]; ok && m != nil {
			out = append(out, m)
		}
	}
	return out
}

// newMember 是内部构造器，把「契约一定合法」这件事收敛到一处。
//
// 这里用 panic 而不是返回 error：契约是硬编码的常量组合，构造失败只可能是
// 有人改坏了代码，而不是运行期的外部输入出了问题。让它在进程启动的第一秒炸掉，
// 远好过在凌晨三点的某次分析里返回一个 nil 成员。
func newMember(
	kind value_objects.AgentKind,
	access value_objects.DataAccess,
	policy value_objects.FailurePolicy,
	temperature float64,
	maxTokens, rounds int,
) *CrewMember {
	c, err := value_objects.NewContract(kind, value_objects.StepKeyOf(kind), access, policy)
	if err != nil {
		panic(err)
	}
	return &CrewMember{contract: c.WithTuning(temperature, maxTokens, rounds)}
}

// ---------------------------------------------------------------------------
// 分析师层：六位并行执行，彼此不可见，各自只拿本职工具。
// 全部是容错的——六个视角里瞎掉一个，剩下五个的结论依然成立。
// ---------------------------------------------------------------------------

// NewMarketAnalyst 市场技术面分析师：量价、均线、MACD/RSI/BOLL。
func NewMarketAnalyst() *CrewMember {
	return newMember(value_objects.KindMarketAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetKlines,
			value_objects.ToolGetTechnicalIndics,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// NewFundamentalsAnalyst 基本面分析师：盈利能力、成长性、估值、负债。
func NewFundamentalsAnalyst() *CrewMember {
	return newMember(value_objects.KindFundamentalsAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetFinancials,
			value_objects.ToolGetQuote,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// NewNewsAnalyst 新闻面分析师：公告、政策、行业事件。
// 刻意不给财务工具：新闻分析师用财报数据论证，等于把基本面报告写第二遍。
func NewNewsAnalyst() *CrewMember {
	return newMember(value_objects.KindNewsAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetNews,
			value_objects.ToolGetQuote,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// NewSentimentAnalyst 市场情绪分析师：社交舆情热度与情感倾向。
func NewSentimentAnalyst() *CrewMember {
	return newMember(value_objects.KindSentimentAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetSocialSentiment,
			value_objects.ToolGetNews,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// NewSectorAnalyst 板块轮动分析师：所属行业的相对强弱与资金流向。
func NewSectorAnalyst() *CrewMember {
	return newMember(value_objects.KindSectorAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetKlines,
			value_objects.ToolGetFinancials,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// NewIndexAnalyst 大盘环境分析师：指数趋势与系统性风险。
func NewIndexAnalyst() *CrewMember {
	return newMember(value_objects.KindIndexAnalyst,
		value_objects.NewDataAccess(
			value_objects.ToolGetKlines,
			value_objects.ToolGetTechnicalIndics,
			value_objects.ToolGetQuote,
		),
		value_objects.PolicyTolerant, tempAnalyst, tokensAnalyst, roundsAnalyst)
}

// ---------------------------------------------------------------------------
// 研究层：多头 -> 空头 -> 研究经理，严格串行。
// 串行是本质需求：空头必须看见多头的论证才谈得上反驳。
// ---------------------------------------------------------------------------

// NewBullResearcher 多头研究员：基于分析师报告构建看多论证。
//
// 前置条件是「至少有一份分析师报告」：六位分析师全军覆没时，
// 多头只能凭空编造看多理由，那比没有报告更危险。
func NewBullResearcher() *CrewMember {
	m := newMember(value_objects.KindBullResearcher,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetTechnicalIndics,
		),
		value_objects.PolicyTolerant, tempDebater, tokensDebater, roundsDebater)
	m.requires = requireAnyAnalystReport
	return m
}

// NewBearResearcher 空头研究员：逐条反驳多头论证并给出看空理由。
func NewBearResearcher() *CrewMember {
	m := newMember(value_objects.KindBearResearcher,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetTechnicalIndics,
		),
		value_objects.PolicyTolerant, tempDebater, tokensDebater, roundsDebater)
	m.requires = requireAnyAnalystReport
	return m
}

// NewResearchManager 研究经理：裁决多空辩论，给出研究结论。
//
// 严格策略：它的裁决是交易员的唯一输入，裁决缺席等于让交易员对着空白写方案。
// 前置条件放宽到「多空至少有一方发言」——一方挂掉时，
// 让经理在单方论证的基础上给结论，仍然比中止整次分析有价值。
func NewResearchManager() *CrewMember {
	m := newMember(value_objects.KindResearchManager,
		value_objects.NoDataAccess(),
		value_objects.PolicyStrict, tempArbiter, tokensArbiter, roundsNone)
	m.requires = func(s ContextSnapshot) error {
		if s.HasReport(value_objects.KindBullResearcher) || s.HasReport(value_objects.KindBearResearcher) {
			return nil
		}
		return custom_errors.Unavailable("多空双方均未产出论证，无可裁决内容")
	}
	return m
}

// ---------------------------------------------------------------------------
// 交易层：一位交易员，严格策略。
// ---------------------------------------------------------------------------

// NewTrader 交易员：把研究结论翻译成可执行的交易方案。
func NewTrader() *CrewMember {
	m := newMember(value_objects.KindTrader,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetTechnicalIndics,
		),
		value_objects.PolicyStrict, tempTrader, tokensArbiter, roundsDebater)
	// 交易员在深度 1（不跑辩论）时也要能工作，因此前置条件只要求「有素材」，
	// 不指名一定要有研究经理的裁决。
	m.requires = func(s ContextSnapshot) error {
		if len(s.Reports) == 0 {
			return custom_errors.Unavailable("没有任何分析报告，无法制定交易方案")
		}
		return nil
	}
	return m
}

// ---------------------------------------------------------------------------
// 风控层：三位辩手并行，风控经理终裁。
// ---------------------------------------------------------------------------

func newRiskDebator(kind value_objects.AgentKind) *CrewMember {
	m := newMember(kind,
		value_objects.NewDataAccess(
			value_objects.ToolGetQuote,
			value_objects.ToolGetTechnicalIndics,
		),
		value_objects.PolicyTolerant, tempRiskVoice, tokensDebater, roundsDebater)
	m.requires = func(s ContextSnapshot) error {
		if s.HasReport(value_objects.KindTrader) {
			return nil
		}
		return custom_errors.Unavailable("交易方案缺失，无可评估对象")
	}
	return m
}

// NewAggressiveRiskDebator 激进派：论证方案过于保守、错失收益的风险。
func NewAggressiveRiskDebator() *CrewMember {
	return newRiskDebator(value_objects.KindRiskAggressive)
}

// NewConservativeRiskDebator 保守派：论证方案的下行风险与最坏情形。
func NewConservativeRiskDebator() *CrewMember {
	return newRiskDebator(value_objects.KindRiskConservative)
}

// NewNeutralRiskDebator 中性派：校准激进与保守双方的偏差。
func NewNeutralRiskDebator() *CrewMember {
	return newRiskDebator(value_objects.KindRiskNeutral)
}

// NewRiskManager 风控经理：给出终局决策。
//
// 它是唯一一位产出会被解析成结构化决策的成员，因此：
//   - 温度压到最低，保证末尾的结构化块格式稳定；
//   - absorb 钩子在报告落地的同时解析决策并交给聚合收敛；
//   - 策略为严格——终局决策缺席，这次分析就没有结论可言。
//
// 解析失败不会让它失败：ParseDecision 在认不出格式时退化为全文关键词匹配，
// 再不行就是 undecided。让一次跑了十几分钟的分析因为模型忘了写冒号而作废，
// 是完全不成比例的代价。
func NewRiskManager() *CrewMember {
	m := newMember(value_objects.KindRiskManager,
		value_objects.NoDataAccess(),
		value_objects.PolicyStrict, tempRiskFinal, tokensFinal, roundsNone)
	m.requires = func(s ContextSnapshot) error {
		if s.HasReport(value_objects.KindTrader) {
			return nil
		}
		return custom_errors.Unavailable("交易方案缺失，无法进行风险终裁")
	}
	m.absorb = func(ac *AnalysisContext, res TurnResult) {
		ac.SetDecision(value_objects.ParseDecision(res.Content))
	}
	return m
}

// NewSoloAnalyst 是对照组：一位独立分析师，看同一份素材，一次给出决策。
//
// # 它不进 NewCrew
//
// 它不是花名册的一员，也不参与任何编排。它存在的唯一目的，是回答
// 「十四位成员的分工、辩论与终裁，相对一次直答到底多值多少」。
// 把它塞进 Crew，这个问题就问不成了——对照组会跑在实验组内部。
//
// # 工具授权为什么给满
//
// 这个实验要测的是**编排**的边际价值，不是「有工具 vs 没工具」。
// 只给它数据不给它工具，得到的差异里会混进一个完全无关的变量，
// 而那个变量的影响大概率比编排本身还大。
//
// # 为什么 token 与轮数给到与仲裁者同级
//
// 同理：一次直答要在一条回复里覆盖十四位成员分头做的事，
// 给它分析师级别的配额等于让它写到一半被截断，
// 那样测出来的差距是「谁的额度大」，不是「谁的方法好」。
func NewSoloAnalyst() *CrewMember {
	m := newMember(value_objects.KindSolo,
		value_objects.NewDataAccess(value_objects.AllToolNames()...),
		value_objects.PolicyStrict, tempTrader, tokensArbiter, roundsAnalyst)
	// 前置条件只要求有素材：它拿到的就是数据层，不依赖任何其他成员的产出——
	// 那正是「独立」的含义。
	m.requires = func(s ContextSnapshot) error {
		if s.Market.Quote.Code.IsZero() && len(s.Market.Klines) == 0 &&
			len(s.Market.Financials) == 0 && len(s.Market.News) == 0 {
			return custom_errors.Unavailable("没有任何素材，无法独立研判")
		}
		return nil
	}
	// 与风控经理一样把结论收进决策：对照组的产出必须能和实验组逐条比对，
	// 而比对的单位是 Decision，不是一段自由文本。
	m.absorb = func(ac *AnalysisContext, res TurnResult) {
		ac.SetDecision(value_objects.ParseDecision(res.Content))
	}
	return m
}

// requireAnyAnalystReport 是多空研究员的共同前置条件。
func requireAnyAnalystReport(s ContextSnapshot) error {
	if slices.ContainsFunc(value_objects.AnalystKinds(), s.HasReport) {
		return nil
	}
	return custom_errors.Unavailable("没有任何分析师报告，无法展开论证")
}
