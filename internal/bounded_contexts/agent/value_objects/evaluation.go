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
