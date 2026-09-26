// Package value_objects 提供 agent 上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不感知数据库、HTTP、也不感知任何一家大模型厂商的线上协议。
// 它只回答两类问题：
//   - 「一个智能体是什么」——Layer / Phase / AgentKind / Contract；
//   - 「一次模型对话长什么样」——Message / ToolCall / CompletionRequest ...
//
// 需要「身份 + 生命周期 + 不变式」的概念（AnalysisContext / Orchestrator / 各位成员）
// 都不属于这里，它们是 entities/ 的职责。
package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Layer 是分析上下文的分层标识。
//
// 一次完整分析的共享状态被切成五层，每层只由本层的智能体写入、由下游层只读消费。
// 把「谁能写哪一层」固化成类型而不是靠约定，是因为并行分析师是并发写同一个
// AnalysisContext 的：没有分层，六个分析师和多空研究员会在同一张 map 上互相覆盖，
// 而这种错误在跑通一次之后极难复现。
type Layer int

const (
	// LayerData 是数据层：行情、K 线、已落库的技术指标、财务、资讯、舆情。
	// 它在分析开始前一次性填好，之后全程只读。
	LayerData Layer = iota + 1
	// LayerAnalysis 是分析师层：六位分析师并行写入，彼此不可见。
	LayerAnalysis
	// LayerResearch 是研究层：多头/空头/研究经理，顺序写入，可读分析师层。
	LayerResearch
	// LayerTrading 是交易层：交易员的方案。
	LayerTrading
	// LayerRisk 是风控层：三位风控辩手与风控经理的终裁。
	LayerRisk
)

func (l Layer) Valid() bool { return l >= LayerData && l <= LayerRisk }

func (l Layer) String() string {
	switch l {
	case LayerData:
		return "data"
	case LayerAnalysis:
		return "analysis"
	case LayerResearch:
		return "research"
	case LayerTrading:
		return "trading"
	case LayerRisk:
		return "risk"
	}
	return "unknown"
}

func (l Layer) DisplayName() string {
	switch l {
	case LayerData:
		return "数据层"
	case LayerAnalysis:
		return "分析层"
	case LayerResearch:
		return "研究层"
	case LayerTrading:
		return "交易层"
	case LayerRisk:
		return "风控层"
	}
	return "未知层"
}

// Phase 是编排阶段标识，与 Layer 一一对应但语义不同：
// Layer 说的是「数据写在哪」，Phase 说的是「什么时候跑」。
// 两者分开是因为风控辩手与风控经理同属风控层，却分属并行与串行两段执行。
type Phase string

const (
	PhaseDataCollection Phase = "data_collection"
	PhaseAnalyst        Phase = "analyst"
	PhaseDebate         Phase = "debate"
	PhaseTrading        Phase = "trading"
	PhaseRisk           Phase = "risk"
)

func (p Phase) Valid() bool {
	switch p {
	case PhaseDataCollection, PhaseAnalyst, PhaseDebate, PhaseTrading, PhaseRisk:
		return true
	}
	return false
}

func (p Phase) String() string { return string(p) }

func (p Phase) DisplayName() string {
	switch p {
	case PhaseDataCollection:
		return "数据准备"
	case PhaseAnalyst:
		return "分析师研判"
	case PhaseDebate:
		return "多空辩论"
	case PhaseTrading:
		return "交易决策"
	case PhaseRisk:
		return "风险评估"
	}
	return "未知阶段"
}

// AgentKind 是成员身份。
//
// 六位分析师的 Kind 字面量与 analysis 上下文 Request.Analysts 里的分析师 ID 完全一致
// （market / fundamentals / news / sentiment / sector / index）。
// 这不是巧合而是契约：进度条的步骤 key 由 Request.Analysts 生成
// （analysis/value_objects/progress.go 的 NewProgress），
// 引擎必须用同一套字面量汇报，否则 Advance 找不到步骤，进度条会永远停在第一格。
type AgentKind string

const (
	KindMarketAnalyst       AgentKind = "market"
	KindFundamentalsAnalyst AgentKind = "fundamentals"
	KindNewsAnalyst         AgentKind = "news"
	KindSentimentAnalyst    AgentKind = "sentiment"
	KindSectorAnalyst       AgentKind = "sector"
	KindIndexAnalyst        AgentKind = "index"

	KindBullResearcher  AgentKind = "bull"
	KindBearResearcher  AgentKind = "bear"
	KindResearchManager AgentKind = "research_manager"

	KindTrader AgentKind = "trader"

	KindRiskAggressive   AgentKind = "risk_aggressive"
	KindRiskConservative AgentKind = "risk_conservative"
	KindRiskNeutral      AgentKind = "risk_neutral"
	KindRiskManager      AgentKind = "risk_manager"

	// KindSolo 是对照组：一位独立分析师，看同一份素材，一次给出决策。
	//
	// # 它不是花名册的一员
	//
	// 它刻意不在 AnalystKinds / AllKinds 里，也不进 NewCrew。
	// 它存在的唯一目的是回答「十四位成员的分工与辩论，相对一次直答多值多少」——
	// 把它混进编排，这个问题就无从谈起了（对照组跑在实验组内部）。
	//
	// 它的层与阶段都取交易层：它产出的是决策块，与交易员同构，
	// 这样决策解析、轨迹落库、决策链渲染全都不用为它开分支。
	KindSolo AgentKind = "solo"
)

// AnalystKinds 是六位分析师，顺序即默认执行顺序（并行时仅影响报告排版）。
func AnalystKinds() []AgentKind {
	return []AgentKind{
		KindMarketAnalyst, KindFundamentalsAnalyst, KindNewsAnalyst,
		KindSentimentAnalyst, KindSectorAnalyst, KindIndexAnalyst,
	}
}

// AllKinds 是全部 14 位成员。
func AllKinds() []AgentKind {
	return append(AnalystKinds(),
		KindBullResearcher, KindBearResearcher, KindResearchManager,
		KindTrader,
		KindRiskAggressive, KindRiskConservative, KindRiskNeutral, KindRiskManager,
	)
}

// NewAgentKind 解析成员标识。非法值报错而不是静默降级：
// 静默降级会让配置里的一个笔误变成「某位分析师神秘地不干活」，排查成本极高。
func NewAgentKind(s string) (AgentKind, error) {
	k := AgentKind(strings.ToLower(strings.TrimSpace(s)))
	if k.Valid() {
		return k, nil
	}
	return "", custom_errors.Invalid("未知的智能体标识: %s", s)
}

func (k AgentKind) Valid() bool {
	switch k {
	case KindMarketAnalyst, KindFundamentalsAnalyst, KindNewsAnalyst,
		KindSentimentAnalyst, KindSectorAnalyst, KindIndexAnalyst,
		KindBullResearcher, KindBearResearcher, KindResearchManager,
		KindTrader,
		KindRiskAggressive, KindRiskConservative, KindRiskNeutral, KindRiskManager,
		KindSolo:
		return true
	}
	return false
}

func (k AgentKind) String() string { return string(k) }

// IsAnalyst 判定是否属于并行分析师阶段。
func (k AgentKind) IsAnalyst() bool {
	switch k {
	case KindMarketAnalyst, KindFundamentalsAnalyst, KindNewsAnalyst,
		KindSentimentAnalyst, KindSectorAnalyst, KindIndexAnalyst:
		return true
	}
	return false
}

// Layer 返回该成员写入的层。
func (k AgentKind) Layer() Layer {
	switch {
	case k.IsAnalyst():
		return LayerAnalysis
	case k == KindBullResearcher, k == KindBearResearcher, k == KindResearchManager:
		return LayerResearch
	case k == KindTrader, k == KindSolo:
		return LayerTrading
	default:
		return LayerRisk
	}
}

func (k AgentKind) Phase() Phase {
	switch {
	case k.IsAnalyst():
		return PhaseAnalyst
	case k == KindBullResearcher, k == KindBearResearcher, k == KindResearchManager:
		return PhaseDebate
	case k == KindTrader, k == KindSolo:
		return PhaseTrading
	default:
		return PhaseRisk
	}
}

func (k AgentKind) DisplayName() string {
	switch k {
	case KindMarketAnalyst:
		return "市场技术面分析师"
	case KindFundamentalsAnalyst:
		return "基本面分析师"
	case KindNewsAnalyst:
		return "新闻面分析师"
	case KindSentimentAnalyst:
		return "市场情绪分析师"
	case KindSectorAnalyst:
		return "板块轮动分析师"
	case KindIndexAnalyst:
		return "大盘环境分析师"
	case KindBullResearcher:
		return "多头研究员"
	case KindBearResearcher:
		return "空头研究员"
	case KindResearchManager:
		return "研究经理"
	case KindTrader:
		return "交易员"
	case KindSolo:
		return "独立分析师（对照组）"
	case KindRiskAggressive:
		return "激进派风控辩手"
	case KindRiskConservative:
		return "保守派风控辩手"
	case KindRiskNeutral:
		return "中性派风控辩手"
	case KindRiskManager:
		return "风控经理"
	}
	return string(k)
}

// StepKey 是进度步骤键值对象。
//
// 它必须与 analysis/value_objects/progress.go 里 NewProgress 生成的 key 逐字相同。
// 做成 VO 而不是散落的字符串字面量，是因为这条契约没有编译期保护：
// Progress.Advance 对未知 key 的处理是「原样返回」（为了让引擎新增步骤不打崩旧任务），
// 于是一个拼错的 key 不会报错，只会让进度条永远卡住——最难排查的那种哑故障。
// 收敛到这里之后，至少所有拼写只有一处，且可以被测试逐一比对。
type StepKey string

const (
	// StepPrepare 对应 NewProgress 的第一步「准备数据」。
	StepPrepare StepKey = "prepare"
	// StepTrade 对应「交易员制定方案」。
	StepTrade StepKey = "trade"
	// StepReport 对应最后一步「生成分析报告」。
	StepReport StepKey = "report"
	// StepSolo 是对照组独立分析师的步骤键。
	//
	// 它刻意不出现在 NewProgress 生成的任何进度里——对照组不是主流程的一步，
	// 跑它的场景（配对实验）也不需要进度条。这个键存在只是因为 Contract
	// 要求非空步骤键，而那条校验挡的是「正式成员忘了填」。
	StepSolo StepKey = "solo"

	stepAnalystPrefix = "analyst:"
	stepDebatePrefix  = "debate:"
	stepRiskPrefix    = "risk:"
)

func (s StepKey) String() string { return string(s) }

func (s StepKey) IsZero() bool { return s == "" }

// AnalystStepKey 由分析师 ID 生成步骤键，与 NewProgress 的 "analyst:"+id 对齐。
func AnalystStepKey(analystID string) StepKey {
	return StepKey(stepAnalystPrefix + strings.ToLower(strings.TrimSpace(analystID)))
}

// StepKeyOf 返回某位成员汇报进度时使用的步骤键。
//
// 这里的每一条分支都对应 progress.go 里的一行，改这里必须同步改那里，
// 反之亦然；entities 的 plan_progress 测试会把两边全量比对一次。
func StepKeyOf(k AgentKind) StepKey {
	switch k {
	case KindBullResearcher:
		return StepKey(stepDebatePrefix + "bull")
	case KindBearResearcher:
		return StepKey(stepDebatePrefix + "bear")
	case KindResearchManager:
		// 注意不是 "debate:research_manager"：进度里这一步叫 manager。
		return StepKey(stepDebatePrefix + "manager")
	case KindTrader:
		return StepTrade
	case KindRiskAggressive:
		return StepKey(stepRiskPrefix + "aggressive")
	case KindRiskConservative:
		return StepKey(stepRiskPrefix + "conservative")
	case KindRiskNeutral:
		return StepKey(stepRiskPrefix + "neutral")
	case KindRiskManager:
		return StepKey(stepRiskPrefix + "manager")
	case KindSolo:
		return StepSolo
	}
	if k.IsAnalyst() {
		return AnalystStepKey(k.String())
	}
	return ""
}

// FailurePolicy 决定一个阶段里单个成员失败时整个阶段的结论。
type FailurePolicy string

const (
	// PolicyTolerant：单个成员失败只记账，阶段继续。用于分析师与风控辩论——
	// 六个视角里瞎掉一个，剩下五个的结论依然有价值。
	PolicyTolerant FailurePolicy = "tolerant"
	// PolicyStrict：任何一个成员失败即阶段失败。用于研究经理、交易员、风控经理——
	// 它们的产出是下一阶段的唯一输入，缺了就没有可分析的东西，
	// 硬撑下去只会让模型对着空白上下文编一份报告出来。
	PolicyStrict FailurePolicy = "strict"
)

func (p FailurePolicy) Valid() bool { return p == PolicyTolerant || p == PolicyStrict }

func (p FailurePolicy) Tolerant() bool { return p == PolicyTolerant }

func (p FailurePolicy) String() string { return string(p) }

// ExecutionMode 决定一个阶段内部是并行还是串行执行。
type ExecutionMode string

const (
	// ModeParallel：阶段内成员彼此独立，可并发跑。
	ModeParallel ExecutionMode = "parallel"
	// ModeSequential：后一位成员要读前一位的产出（多头看完空头才能反驳），
	// 必须串行。
	ModeSequential ExecutionMode = "sequential"
)

func (m ExecutionMode) Valid() bool { return m == ModeParallel || m == ModeSequential }

func (m ExecutionMode) Parallel() bool { return m == ModeParallel }

func (m ExecutionMode) String() string { return string(m) }

// Contract 是一位成员的声明式契约：它是谁、写哪一层、报哪个进度步骤、
// 能用哪些工具、允许几轮工具调用、失败了算不算数。
//
// 做成值对象而不是散在各个 entity 的字段里，有两个直接好处：
//   - 编排器只认 Contract，不必为 14 个类型写 14 个分支；
//   - 「新闻分析师不该能读财务报表」这种授权规则有了唯一落点（Access），
//     运行期由 Runtime 强制执行，模型再怎么被提示词注入也越不过去。
type Contract struct {
	Kind        AgentKind
	Layer       Layer
	Phase       Phase
	DisplayName string
	// Step 是该成员完成/失败时汇报的进度键。
	Step StepKey
	// Access 是工具授权白名单，空集表示不给工具。
	Access DataAccess
	// Temperature / MaxTokens 留 0 表示走 Runtime 的默认值。
	Temperature float64
	MaxTokens   int
	// MaxToolRounds 是工具调用循环的最大轮数，0 表示走默认值。
	// 它是硬上限而不是建议值：模型陷入「查完再查」的循环时，
	// 唯一能保证一次分析终止的就是这个计数器。
	MaxToolRounds int
	// Policy 是该成员在所属阶段里的失败语义。
	Policy FailurePolicy
}

// NewContract 构造并校验契约。
func NewContract(kind AgentKind, step StepKey, access DataAccess, policy FailurePolicy) (Contract, error) {
	if !kind.Valid() {
		return Contract{}, custom_errors.Invalid("未知的智能体标识: %s", kind)
	}
	if step.IsZero() {
		return Contract{}, custom_errors.Invalid("智能体 %s 缺少进度步骤键", kind)
	}
	if !policy.Valid() {
		return Contract{}, custom_errors.Invalid("智能体 %s 的失败策略非法: %s", kind, policy)
	}
	return Contract{
		Kind:        kind,
		Layer:       kind.Layer(),
		Phase:       kind.Phase(),
		DisplayName: kind.DisplayName(),
		Step:        step,
		Access:      access,
		Policy:      policy,
	}, nil
}

// WithTuning 返回调整了采样参数的新契约。VO 不可变，因此返回副本。
func (c Contract) WithTuning(temperature float64, maxTokens, maxToolRounds int) Contract {
	out := c
	out.Temperature = temperature
	out.MaxTokens = maxTokens
	out.MaxToolRounds = maxToolRounds
	return out
}

func (c Contract) IsZero() bool { return c.Kind == "" }
