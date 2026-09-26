package value_objects

import (
	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// Direction 是方向判定的三态。
//
// 必须有 flat 这一态：把「基本没动」归进涨或跌，会让一个随机猜方向的系统
// 也拿到 50% 的一致率，整个评测就失去了意义。
type Direction string

const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
	DirectionFlat Direction = "flat"
	// DirectionNone 是「没给方向」：持有、观望、待定都落在这里。
	DirectionNone Direction = "none"
)

func (d Direction) String() string { return string(d) }

func (d Direction) DisplayName() string {
	switch d {
	case DirectionUp:
		return "涨"
	case DirectionDown:
		return "跌"
	case DirectionFlat:
		return "横盘"
	}
	return "无方向"
}

// DirectionOfAction 把建议动作翻译成方向。
//
// 持有与待定没有方向：它们是「不动」，而不是「预测横盘」。
// 把它们当成横盘预测会凭空给系统送分——横盘在短窗口里出现得相当频繁。
//
// 这个映射是评分口径的核心，必须只有一份：回测按它判命中，
// 配对实验按它判两边是否给出了相同方向。两处各写一份的话，
// 某天有人给「观望」加了一个方向，两个实验的结论会朝相反方向漂移，
// 而它们会被写进同一份报告。
func DirectionOfAction(a analysis_vo.Action) Direction {
	switch a {
	case analysis_vo.ActionBuy, analysis_vo.ActionIncrease:
		return DirectionUp
	case analysis_vo.ActionSell, analysis_vo.ActionReduce:
		return DirectionDown
	}
	return DirectionNone
}

// SkipReason 是一条样本未参与评分的原因。
//
// 跳过的样本必须带着原因一起留下，不能在统计前静默丢掉：
// 「一致率 70%」和「一致率 70%，但三分之二的样本因为缺行情被跳过了」
// 是两个完全不同的结论，而后者才是真相。
type SkipReason string

const (
	SkipNone SkipReason = ""
	// SkipNoDirection：系统给的是持有/待定，本来就没有方向可对。
	SkipNoDirection SkipReason = "no_direction"
	// SkipNoBasePrice：分析当日没有收盘价，算不出基准。
	SkipNoBasePrice SkipReason = "no_base_price"
	// SkipNoForwardPrice：前瞻窗口内没有足够的交易日，通常是刚跑过的分析。
	SkipNoForwardPrice SkipReason = "no_forward_price"
	// SkipFetchFailed：取行情时出错（数据库超时、连接断）。
	//
	// 它必须与 SkipNoForwardPrice 分开计数：后者是「时候未到，过几天再跑就能评上」，
	// 前者是「基础设施抖了」。混在一起的话，一次 Mongo 故障会伪装成一批
	// 正常的窗口不足，而那恰恰是最该被发现的情况。
	SkipFetchFailed SkipReason = "fetch_failed"
)

func (r SkipReason) DisplayName() string {
	switch r {
	case SkipNoDirection:
		return "系统未给出方向"
	case SkipNoBasePrice:
		return "缺分析当日收盘价"
	case SkipNoForwardPrice:
		return "前瞻窗口内行情不足"
	case SkipFetchFailed:
		return "取行情失败"
	}
	return ""
}

// EvalSample 是一条评分样本：一次分析的建议，对上它之后的实际走势。
type EvalSample struct {
	RunID     string
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate

	Action     analysis_vo.Action
	Confidence decimal.Decimal
	// Predicted 是把建议翻译成的方向。
	Predicted Direction

	// BasePrice 是分析当日收盘价，ForwardPrice 是前瞻窗口末日收盘价。
	BasePrice    decimal.Decimal
	ForwardPrice decimal.Decimal
	// ForwardDate 是实际取到的前瞻交易日，不一定等于「分析日 + N 个自然日」。
	ForwardDate shared_vo.TradeDate
	// ReturnPct 是前瞻收益率百分数，由两个价格相除推出。
	//
	// 它在这里算一次、随样本落库，之后所有读路径（统计、报表、命令行输出）
	// 都读这一份。绝不允许拿两个价格再除一遍：评分阈值是拿它去比的，
	// 一个在别处重算、精度略有出入的数字足以让一条样本从命中翻成未命中。
	ReturnPct decimal.Decimal
	Actual    Direction

	Hit        bool
	SkipReason SkipReason
}

// Scored 判定这条样本是否参与了命中率统计。
func (s EvalSample) Scored() bool { return s.SkipReason == SkipNone }

// ActionStat 是按建议动作拆分的统计。
type ActionStat struct {
	Action  analysis_vo.Action
	Scored  int
	Hits    int
	HitRate decimal.Decimal
}

// ConfidenceInterval 是一个比率的置信区间。
//
// # 为什么一致率必须带区间
//
// 「一致率 58%」这个数字单独拿出来什么都说明不了：n=30 时它的 95% 区间大约是
// [40%, 74%]，与抛硬币完全无法区分；n=1000 时才收窄到 [55%, 61%]。
// 少了区间，读者（包括写代码的人自己）会把小样本的波动当成系统的能力，
// 而这恰恰是这类评测最常见、也最难自我察觉的错误。
type ConfidenceInterval struct {
	// Lower / Upper 是区间两端，0-1 之间。
	Lower decimal.Decimal
	Upper decimal.Decimal
	// Level 是置信水平，当前固定 0.95。显式存下来是因为它会进报告，
	// 而「95%」这三个字符如果只写在展示层，改水平时必然漏改一处。
	Level decimal.Decimal
}

// Width 是区间宽度，用来快速判断这个数字有没有解释价值。
func (c ConfidenceInterval) Width() decimal.Decimal { return c.Upper.Sub(c.Lower) }

// BaselineStat 是对照基线的统计。
//
// # 为什么必须与系统结论用同一批样本
//
// 基线的全部意义在于回答「系统比不动脑子好在哪」。一旦两边的分母不同
// （比如基线用全部运行、系统只用给了方向的那些），两个数字就不再可比——
// 而它们会被并排印在同一张表上，读者没有任何线索察觉这件事。
// 因此基线只在已评分样本上计算，与 EvaluationStats.Scored 同分母。
type BaselineStat struct {
	// Name 是基线的名字，例如「无脑全买入」。
	Name string
	// Hits 是这个基线在同一批样本上「猜对方向」的次数。
	Hits    int
	HitRate decimal.Decimal
	CI      ConfidenceInterval
}

// EvaluationStats 是一次回测的汇总结论。
type EvaluationStats struct {
	// Total 是扫到的运行总数，Scored 是真正参与评分的样本数。
	//
	// 两个数都要露出来：它们的差额就是被跳过的样本，
	// 只报一致率而不报样本量，是这类评测最常见的自欺方式。
	Total   int
	Scored  int
	Skipped int
	Hits    int
	// HitRate 是命中数除以参与评分的样本数，0-1 之间。
	HitRate decimal.Decimal
	// HitRateCI 是 HitRate 的 95% 置信区间。
	//
	// 它与 HitRate 必须成对出现，理由见 ConfidenceInterval 的说明。
	// 展示层的硬约束：任何一致率数字不带 n 与区间，不许进 README、不许进简历。
	HitRateCI ConfidenceInterval
	// Baselines 是同一批样本上的对照基线，顺序固定。
	//
	// 没有基线的一致率是无法解释的：牛市里「无脑全买入」也能有 65%，
	// 此时系统的 58% 其实是负贡献——而单看 58% 这个数字，
	// 它看起来比抛硬币强。
	Baselines []BaselineStat
	// SkipCounts 是各跳过原因的条数。
	SkipCounts map[SkipReason]int
	// Truncated 表示区间里还有运行没被取进本次评估。
	//
	// 它必须与一致率一起露出来：被砍过的样本量看起来和全量一模一样，
	// 而「一致率 70%，基于全部 1000 次运行」与「一致率 70%，基于最早的 1000 次」
	// 是两个结论——后者还带着时间上的选择偏差。
	Truncated bool
	// ByAction 按建议动作拆分，顺序固定，便于稳定展示与比对。
	ByAction []ActionStat
}
