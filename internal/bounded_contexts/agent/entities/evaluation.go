package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// FlatBandPct 是「基本没动」的判定带宽，单位为百分数。
//
// 它是这套评测里唯一一个可以被质疑的数字，因此必须显式且可解释：
// 取 1 表示前瞻窗口内涨跌幅在 ±1% 以内一律判为横盘。
//
// 不取 0 的理由是它会让评测失去意义——用 0 分界的话，任何一次分析
// 都必然落在涨或跌的一侧，一个每次都喊「买入」的系统在长期上涨的样本里
// 能拿到很高的一致率，而它其实什么都没预测。
// 取 1 而不是更大，是因为默认前瞻窗口只有 5 个交易日，
// 带宽再宽会把真实的小幅趋势也吞掉。
const FlatBandPct = 1

// EvaluationHorizonDays 是默认前瞻窗口（自然日）。
//
// 取 7 个自然日而不是 5 个交易日：行情按自然日区间查，7 天覆盖一个完整周
// （含一个周末），落到交易日大约是 5 根 K 线。
const EvaluationHorizonDays = 7

// Evaluation 是一次回测评估，本上下文的聚合根。
//
// # 它守的是什么
//
// 一条建议算不算「说对了」，是这套评测唯一的业务规则，也是最容易被
// 悄悄改掉的那一条。把它放在聚合里而不是散在统计脚本里，
// 意味着命中判定只有一处定义，换一个展示方式、换一个命令行参数，
// 都不会顺手把口径改了——而改了口径的两次回测之间没有任何可比性。
//
// # 子项由根持有
//
// 样本只能通过 Score/Skip 加进来，外部拿不到可变的样本引用。
// 统计量由 Settle 一次算出并固化，读路径直接取，不重算。
type Evaluation struct {
	ID        string
	CreatedAt time.Time
	// Window 是被评估的分析所在的交易日区间。
	Window shared_vo.DateRange
	// HorizonDays 是前瞻窗口的自然日数。
	HorizonDays int
	// FlatBandPct 随评估一起落库：它是判定口径的一部分，
	// 不存的话，半年后拿到两份一致率不同的报告，无从知道是系统变好了
	// 还是当初把带宽调窄了。
	FlatBandPct decimal.Decimal

	samples []value_objects.EvalSample
	stats   value_objects.EvaluationStats
	settled bool
	// truncated 表示区间里还有运行没被取进来，由编排层在取完样本后告知。
	truncated bool
}

// MarkTruncated 记下「样本没取全」。
//
// 它是一条必须被展示的事实而不是可选的元数据：被砍过的样本量
// 看起来和全量一模一样，只报一致率就是在隐瞒选择偏差。
func (e *Evaluation) MarkTruncated() {
	e.truncated = true
	e.settled = false
}

// NewEvaluation 开一次回测评估。
func NewEvaluation(id string, window shared_vo.DateRange, horizonDays int) (*Evaluation, error) {
	if id == "" {
		return nil, custom_errors.Invalid("评估 ID 不能为空")
	}
	if horizonDays <= 0 {
		horizonDays = EvaluationHorizonDays
	}
	return &Evaluation{
		ID:          id,
		CreatedAt:   time.Now(),
		Window:      window,
		HorizonDays: horizonDays,
		FlatBandPct: decimal.NewFromInt(FlatBandPct),
		samples:     make([]value_objects.EvalSample, 0, 64),
	}, nil
}

// RehydrateEvaluation 从落库形态还原聚合，仓储专用。
// 统计量原样带回来，不重算——它是落库的派生量。
func RehydrateEvaluation(
	id string,
	createdAt time.Time,
	window shared_vo.DateRange,
	horizonDays int,
	flatBandPct decimal.Decimal,
	samples []value_objects.EvalSample,
	stats value_objects.EvaluationStats,
) *Evaluation {
	return &Evaluation{
		ID: id, CreatedAt: createdAt, Window: window,
		HorizonDays: horizonDays, FlatBandPct: flatBandPct,
		// 拷一份而不是直接持有调用方的切片：Samples() 特意做了防御性拷贝，
		// 重建路径少这一层就成了唯一一个外部还能改到内部状态的口子。
		samples:   append([]value_objects.EvalSample(nil), samples...),
		stats:     stats,
		settled:   true,
		truncated: stats.Truncated,
	}
}

// Skip 登记一条无法评分的样本，并记下原因。
//
// 跳过的样本照样进聚合：统计里的 Skipped 就是从它们数出来的，
// 而「多少样本没能评上分」与「一致率是多少」同样重要。
func (e *Evaluation) Skip(run value_objects.RunSummary, reason value_objects.SkipReason) {
	e.samples = append(e.samples, value_objects.EvalSample{
		RunID:      run.RunID,
		Code:       run.Code,
		TradeDate:  run.TradeDate,
		Action:     run.Decision.Action,
		Confidence: run.Decision.Confidence,
		Predicted:  predictedDirection(run.Decision.Action),
		SkipReason: reason,
	})
	e.settled = false
}

// Score 评一条样本：算出前瞻收益率、判定实际方向、与建议方向比对。
//
// 收益率在这里算一次就固化进样本，之后任何读路径都取这一份。
// 命中判定与收益率计算写在同一个方法里，而不是先算再判——
// 分成两步意味着判定方随时可能拿到一个在别处重算过的数字。
//
// # 基准价的口径，以及它带的前视成分
//
// basePrice 取的是分析所属交易日的**收盘价**，等价于假设「按当日收盘价建仓」。
// 对盘中提交的分析，这个价格在分析发生的那一刻还不存在——严格说是前视的。
//
// 仍然这么取，是因为替代方案各有更大的问题：用 T+1 开盘价会把隔夜跳空
// 整个算进策略收益，而那与分析质量无关；用提交时刻的实时价则要求
// 逐笔行情，这个项目根本没存。
// 这里记下来，是为了让看到一致率的人知道它是在什么口径下算出来的——
// 一个没写明口径的回测数字，比没有回测更容易误导人。
func (e *Evaluation) Score(
	run value_objects.RunSummary,
	basePrice, forwardPrice decimal.Decimal,
	forwardDate shared_vo.TradeDate,
) {
	// 先判方向再判价格：方向取决于当时那次分析给了什么建议，与行情有没有取到无关。
	// 反过来的话，一条「系统说持有」且恰好缺行情的样本会被记成「缺基准价」，
	// 而跳过明细正是用来判断「这份一致率有多少水分」的，归错因就白记了。
	predicted := predictedDirection(run.Decision.Action)
	if predicted == value_objects.DirectionNone {
		e.Skip(run, value_objects.SkipNoDirection)
		return
	}
	if basePrice.IsZero() || basePrice.IsNegative() {
		e.Skip(run, value_objects.SkipNoBasePrice)
		return
	}

	returnPct := forwardPrice.Sub(basePrice).
		DivRound(basePrice, decimalx.RatioScale+2).
		Mul(hundred)
	actual := actualDirection(returnPct, e.FlatBandPct)

	e.samples = append(e.samples, value_objects.EvalSample{
		RunID:      run.RunID,
		Code:       run.Code,
		TradeDate:  run.TradeDate,
		Action:     run.Decision.Action,
		Confidence: run.Decision.Confidence,
		Predicted:  predicted,

		BasePrice:    basePrice,
		ForwardPrice: forwardPrice,
		ForwardDate:  forwardDate,
		ReturnPct:    returnPct,
		Actual:       actual,

		// 实际走成横盘算未命中而不是跳过：系统当时确实给了一个方向性建议，
		// 而行情没有朝那个方向走。把它排除掉等于替系统把不利样本挑走了。
		Hit: predicted == actual,
	})
	e.settled = false
}

// Settle 算出全部统计量并固化。读路径取 Stats，永远不重算。
func (e *Evaluation) Settle() {
	stats := value_objects.EvaluationStats{
		Total:      len(e.samples),
		Truncated:  e.truncated,
		SkipCounts: map[value_objects.SkipReason]int{},
	}
	perAction := map[analysis_vo.Action]*value_objects.ActionStat{}
	// alwaysBuyHits 是「无脑全买入」基线的命中数。
	//
	// 它在同一个循环里数，而不是另起一次遍历：两者必须落在**同一批样本**上，
	// 分开算迟早有人给其中一边加个过滤条件，而两个数字会被并排印在同一张表上，
	// 读者没有任何线索察觉分母已经不同了。
	alwaysBuyHits := 0

	for _, s := range e.samples {
		if !s.Scored() {
			stats.Skipped++
			stats.SkipCounts[s.SkipReason]++
			continue
		}
		stats.Scored++
		if s.Hit {
			stats.Hits++
		}
		// 基线的预测恒为「涨」，因此它的命中就是实际方向为涨。
		// 注意横盘不算命中——与系统结论的判定口径完全一致，
		// 否则基线会凭空拿到一批系统拿不到的分。
		if s.Actual == value_objects.DirectionUp {
			alwaysBuyHits++
		}
		st, ok := perAction[s.Action]
		if !ok {
			st = &value_objects.ActionStat{Action: s.Action}
			perAction[s.Action] = st
		}
		st.Scored++
		if s.Hit {
			st.Hits++
		}
	}

	stats.HitRate = ratio(stats.Hits, stats.Scored)
	stats.HitRateCI = value_objects.WilsonInterval(stats.Hits, stats.Scored)
	// 基线与系统结论共用 Scored 作分母，见上面 alwaysBuyHits 的说明。
	stats.Baselines = []value_objects.BaselineStat{{
		Name:    "无脑全买入",
		Hits:    alwaysBuyHits,
		HitRate: ratio(alwaysBuyHits, stats.Scored),
		CI:      value_objects.WilsonInterval(alwaysBuyHits, stats.Scored),
	}}
	// 按固定顺序输出而不是遍历 map：Go 的 map 遍历顺序是随机的，
	// 两次回测的报告行序不同会让 diff 完全没法看。
	for _, a := range scoredActions() {
		st, ok := perAction[a]
		if !ok {
			continue
		}
		st.HitRate = ratio(st.Hits, st.Scored)
		stats.ByAction = append(stats.ByAction, *st)
	}

	e.stats = stats
	e.settled = true
}

// Samples 返回样本副本。
func (e *Evaluation) Samples() []value_objects.EvalSample {
	return append([]value_objects.EvalSample(nil), e.samples...)
}

// Stats 返回已固化的统计量。没结算过就先结算一次，
// 避免调用方拿到一份全零的统计还以为是真结果。
func (e *Evaluation) Stats() value_objects.EvaluationStats {
	if !e.settled {
		e.Settle()
	}
	return e.stats
}

// predictedDirection 把建议动作翻译成方向。
//
// 实现收在 value_objects.DirectionOfAction：这条映射是评分口径的核心，
// 回测按它判命中、配对实验按它判两边方向是否一致，两处各写一份
// 迟早会朝相反方向漂移，而它们的结论会被写进同一份报告。
func predictedDirection(a analysis_vo.Action) value_objects.Direction {
	return value_objects.DirectionOfAction(a)
}

func actualDirection(returnPct, band decimal.Decimal) value_objects.Direction {
	switch {
	case returnPct.GreaterThan(band):
		return value_objects.DirectionUp
	case returnPct.LessThan(band.Neg()):
		return value_objects.DirectionDown
	}
	return value_objects.DirectionFlat
}

// scoredActions 是会参与评分的动作，顺序即报告行序。
func scoredActions() []analysis_vo.Action {
	return []analysis_vo.Action{
		analysis_vo.ActionBuy, analysis_vo.ActionIncrease,
		analysis_vo.ActionReduce, analysis_vo.ActionSell,
	}
}

func ratio(hits, total int) decimal.Decimal {
	if total == 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(hits)).
		DivRound(decimal.NewFromInt(int64(total)), decimalx.RatioScale)
}

var hundred = decimal.NewFromInt(100)
