package value_objects

import (
	"sort"

	"github.com/shopspring/decimal"

	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// 指标窗口参数。写成常量而不是散在函数里的字面量：
// 这些数字会同时出现在计算、展示和提示词里，必须只有一个出处。
const (
	maShort  = 5
	maMid    = 10
	maLong   = 20
	maExtra  = 60
	emaFast  = 12
	emaSlow  = 26
	deaSpan  = 9
	rsiShort = 6
	rsiLong  = 14
	bollSpan = 20
	atrSpan  = 14
)

// calcScale 是迭代计算的中间精度，比落库精度多 4 位。
//
// 多留这 4 位不是保险，是必需的：EMA、RSI、ATR 都是「拿上一步的结果算下一步」的
// 平滑递推，每一步的截断误差都会被带进下一步。中间量按落库精度取整的话，
// 几百根 K 线之后末位会持续摆动；多留 4 位则让截断误差始终低于落库精度两个数量级。
const calcScale = decimalx.IndicatorScale + 4

// 计算里反复出现的常量。decimal 是结构体不能做 const，
// 提到包级变量避免在循环里反复构造。
var (
	two      = decimal.NewFromInt(2)
	hundred  = decimal.NewFromInt(100)
	fifty    = decimal.NewFromInt(50)
	dOne     = decimal.NewFromInt(1)
	rsiUpper = decimal.NewFromInt(80)
	rsiHigh  = decimal.NewFromInt(70)
	rsiLower = decimal.NewFromInt(20)
	rsiLow   = decimal.NewFromInt(30)

	// bollK 是布林带的标准差倍数（2 倍）。
	bollK = two
)

// Indicators 是一组技术指标的计算结果值对象。
//
// # 为什么每一项都是字段，而且必须落库
//
// 这里的每个数字都是从 K 线用乘除推导出来的派生量：MA 是求和再除以窗口长度，
// RSI 是涨跌幅均值相除，MACD 是指数加权（乘系数）之差，BOLL 是均值加减标准差的倍数。
// 派生量一旦允许在读路径上重算，就会出现三个具体故障：
//
//  1. 结果随输入窗口漂移。读路径拿到的 K 线条数受 limit、日期区间、
//     甚至「那天有没有补数」影响，同一只票同一天的 MA20 会因为多取了一根
//     或少取了一根而不同。用户看到报告里写着 MA20=12.31，回头点开图表却是 12.28，
//     没有任何办法解释。
//  2. 复权口径污染。K 线的 adjusted 标记会随数据源切换而变，重算时拿到的是
//     当下的口径，而报告是当时的口径，两者对不上。
//  3. 重跑不可复现。一次失败的分析被重试时，如果指标是现算的，
//     第二次跑出来的技术面结论可能和第一次相反——而任务日志里只留了一份。
//
// 因此本 VO 只在「数据准备阶段」被计算一次，随即由 IndicatorRepository 落库；
// 此后所有读路径（工具 get_technical_indicators、提示词渲染、重跑）
// 一律从库里读回，绝不重算。ComputeIndicators 是全系统唯一的计算入口，
// 它只被写路径调用。
//
// DeviationMA20Pct 同理：它是 (Close-MA20)/MA20×100，是派生量的派生量，
// 所以也占一个字段随快照一起固化，而不是给 Indicators 加一个现算的方法。
//
// # 为什么全部是 decimal
//
// 上面第 3 条「重跑不可复现」在 float64 下还有一个更隐蔽的版本：
// 同一批 K 线、同一份代码，只要求和顺序变了（比如切片来源从正序换成倒序再排），
// float64 的结果就可能在末位不同，因为浮点加法不满足结合律。
// decimal 的加法是精确的，顺序无关，这条不确定性被彻底消除。
type Indicators struct {
	Close decimal.Decimal `json:"close"`

	MA5  decimal.Decimal `json:"ma5"`
	MA10 decimal.Decimal `json:"ma10"`
	MA20 decimal.Decimal `json:"ma20"`
	MA60 decimal.Decimal `json:"ma60"`

	EMA12 decimal.Decimal `json:"ema12"`
	EMA26 decimal.Decimal `json:"ema26"`

	MACDDIF  decimal.Decimal `json:"macd_dif"`
	MACDDEA  decimal.Decimal `json:"macd_dea"`
	MACDHist decimal.Decimal `json:"macd_hist"`

	RSI6  decimal.Decimal `json:"rsi6"`
	RSI14 decimal.Decimal `json:"rsi14"`

	BollUpper decimal.Decimal `json:"boll_upper"`
	BollMid   decimal.Decimal `json:"boll_mid"`
	BollLower decimal.Decimal `json:"boll_lower"`

	ATR14 decimal.Decimal `json:"atr14"`

	VolMA5  decimal.Decimal `json:"vol_ma5"`
	VolMA20 decimal.Decimal `json:"vol_ma20"`

	// DeviationMA20Pct 是收盘价相对 20 日均线的乖离率（百分数）。
	DeviationMA20Pct decimal.Decimal `json:"deviation_ma20_pct"`

	// Samples 是参与计算的 K 线根数，用于判断哪些窗口是有效的。
	// 不足窗口长度的指标固化成 0，Samples 让消费方分得清「0 是算出来的」
	// 还是「0 是样本不够」。
	Samples int `json:"samples"`
}

// ComputeIndicators 是全系统唯一的技术指标计算入口，只允许在写路径上调用。
//
// 入参是任意顺序的 K 线（仓储按 trade_date 倒序返回，数据源多半是正序），
// 这里统一复制一份并按交易日升序排列后再算——就地排序会改掉调用方的切片，
// 而调用方通常还要拿它渲染图表。
func ComputeIndicators(klines []stock_vo.Kline) (Indicators, error) {
	if len(klines) == 0 {
		return Indicators{}, custom_errors.Invalid("没有 K 线数据，无法计算技术指标")
	}

	series := append([]stock_vo.Kline(nil), klines...)
	sort.SliceStable(series, func(i, j int) bool {
		return series[i].TradeDate.Before(series[j].TradeDate)
	})

	n := len(series)
	closes := make([]decimal.Decimal, n)
	highs := make([]decimal.Decimal, n)
	lows := make([]decimal.Decimal, n)
	vols := make([]decimal.Decimal, n)
	for i, k := range series {
		closes[i] = k.Close
		highs[i] = k.High
		lows[i] = k.Low
		vols[i] = k.Volume
	}

	out := Indicators{
		Close:   closes[n-1],
		Samples: n,

		MA5:  lastSMA(closes, maShort),
		MA10: lastSMA(closes, maMid),
		MA20: lastSMA(closes, maLong),
		MA60: lastSMA(closes, maExtra),

		RSI6:  wilderRSI(closes, rsiShort),
		RSI14: wilderRSI(closes, rsiLong),

		ATR14: wilderATR(highs, lows, closes, atrSpan),

		VolMA5:  lastSMA(vols, maShort),
		VolMA20: lastSMA(vols, maLong),
	}

	// MACD：DIF = EMA12 - EMA26，DEA = DIF 的 9 日 EMA，柱 = 2×(DIF-DEA)。
	// 柱乘 2 是国内行情软件的通行画法，报告里的数值要和用户看到的图对得上。
	if n >= emaSlow {
		fast := emaSeries(closes, emaFast)
		slow := emaSeries(closes, emaSlow)
		out.EMA12 = decimalx.RoundIndicator(fast[n-1])
		out.EMA26 = decimalx.RoundIndicator(slow[n-1])

		dif := make([]decimal.Decimal, n)
		for i := range dif {
			dif[i] = fast[i].Sub(slow[i])
		}
		dea := emaSeries(dif, deaSpan)
		out.MACDDIF = decimalx.RoundIndicator(dif[n-1])
		out.MACDDEA = decimalx.RoundIndicator(dea[n-1])
		out.MACDHist = decimalx.RoundIndicator(two.Mul(out.MACDDIF.Sub(out.MACDDEA)))
	}

	// BOLL：中轨取 20 日均线，上下轨为中轨 ± 2 倍总体标准差。
	// 用总体标准差（除以 N）而不是样本标准差（除以 N-1），与主流行情软件一致。
	if n >= bollSpan {
		mid := out.MA20
		sd := decimalx.PopulationStdDev(closes[n-bollSpan:], mid, calcScale)
		out.BollMid = mid
		out.BollUpper = decimalx.RoundIndicator(mid.Add(bollK.Mul(sd)))
		out.BollLower = decimalx.RoundIndicator(mid.Sub(bollK.Mul(sd)))
	}

	if !out.MA20.IsZero() {
		out.DeviationMA20Pct = decimalx.RoundIndicator(
			out.Close.Sub(out.MA20).DivRound(out.MA20, calcScale).Mul(hundred),
		)
	}
	return out, nil
}

func (in Indicators) IsZero() bool { return in.Samples == 0 }

// HasMA20 等一组谓词用于提示词渲染：样本不足时该指标应当写「数据不足」，
// 而不是把 0 当成真实值喂给模型。
func (in Indicators) HasMA20() bool { return in.Samples >= bollSpan }

func (in Indicators) HasMA60() bool { return in.Samples >= maExtra }

func (in Indicators) HasMACD() bool { return in.Samples >= emaSlow }

func (in Indicators) HasATR() bool { return in.Samples > atrSpan }

// MACDSignal 给出金叉/死叉的定性判断。
// 它只做比较不做乘除，因此不属于「派生量必须落库」的范畴。
func (in Indicators) MACDSignal() string {
	if !in.HasMACD() {
		return "数据不足"
	}
	switch {
	case in.MACDDIF.GreaterThan(in.MACDDEA) && in.MACDDIF.IsPositive():
		return "零轴上方金叉，多头动能占优"
	case in.MACDDIF.GreaterThan(in.MACDDEA):
		return "零轴下方金叉，超跌反弹迹象"
	case in.MACDDIF.LessThan(in.MACDDEA) && in.MACDDIF.IsNegative():
		return "零轴下方死叉，空头动能占优"
	default:
		return "零轴上方死叉，涨势转弱"
	}
}

// RSIState 给出 14 日 RSI 的超买超卖定性判断。
func (in Indicators) RSIState() string {
	switch {
	case in.RSI14.IsZero():
		return "数据不足"
	case in.RSI14.GreaterThanOrEqual(rsiUpper):
		return "严重超买"
	case in.RSI14.GreaterThanOrEqual(rsiHigh):
		return "超买"
	case in.RSI14.LessThanOrEqual(rsiLower):
		return "严重超卖"
	case in.RSI14.LessThanOrEqual(rsiLow):
		return "超卖"
	default:
		return "中性"
	}
}

// TrendState 用均线排列给出趋势定性判断。
func (in Indicators) TrendState() string {
	if in.MA5.IsZero() || in.MA20.IsZero() {
		return "数据不足"
	}
	switch {
	case in.MA5.GreaterThan(in.MA10) && in.MA10.GreaterThan(in.MA20):
		return "多头排列"
	case in.MA5.LessThan(in.MA10) && in.MA10.LessThan(in.MA20):
		return "空头排列"
	default:
		return "均线纠缠"
	}
}

// BollPosition 判断收盘价在布林带中的位置。
func (in Indicators) BollPosition() string {
	if !in.HasMA20() || in.BollUpper.Equal(in.BollLower) {
		return "数据不足"
	}
	switch {
	case in.Close.GreaterThanOrEqual(in.BollUpper):
		return "触及上轨"
	case in.Close.LessThanOrEqual(in.BollLower):
		return "触及下轨"
	case in.Close.GreaterThanOrEqual(in.BollMid):
		return "中轨上方"
	default:
		return "中轨下方"
	}
}

// ---------------------------------------------------------------------------
// 计算内核
//
// 这几个函数是纯函数，没有任何外部依赖，因此可以被单元测试逐条对照手算结果。
// 它们全部返回「最后一个点」的值，而不是整条序列：落库的是快照，
// 保留整条序列会让一次分析写进去几千个数字，而没有任何消费方需要它们。
//
// 递推型的三个（emaSeries / wilderRSI / wilderATR）每一步都要显式取整到
// calcScale。这不只是精度考量，也是性能考量：decimal 的乘法会把系数位数相加，
// 不取整的话一条 1000 根的序列递推下来，系数会膨胀到几千位，
// 每一步 big.Int 运算都在搬运这些无意义的尾数。
// ---------------------------------------------------------------------------

// lastSMA 返回最后 span 个样本的简单算术平均，样本不足返回 0。
func lastSMA(xs []decimal.Decimal, span int) decimal.Decimal {
	if span <= 0 || len(xs) < span {
		return decimal.Zero
	}
	sum := decimalx.Sum(xs[len(xs)-span:])
	return decimalx.RoundIndicator(sum.DivRound(decimal.NewFromInt(int64(span)), calcScale))
}

// emaSeries 返回整条指数移动平均序列，用首个样本添加。
//
// 用首值添加而不是用前 span 个样本的 SMA 添加：两种做法在国内外行情软件里都存在，
// 差异随样本增加迅速衰减，而首值添加能让 span 长于样本数时仍然给出有意义的值。
// 关键是全系统只用这一种，报告里的数字才能和自己前后一致。
func emaSeries(xs []decimal.Decimal, span int) []decimal.Decimal {
	out := make([]decimal.Decimal, len(xs))
	if len(xs) == 0 || span <= 0 {
		return out
	}
	// k = 2/(span+1)，是个无限小数（span=12 时为 0.1538...），
	// 按 calcScale 截断；1-k 用同一个截断值算，保证两个权重严格互补。
	k := two.DivRound(decimal.NewFromInt(int64(span+1)), calcScale)
	oneMinusK := dOne.Sub(k)
	out[0] = xs[0]
	for i := 1; i < len(xs); i++ {
		out[i] = xs[i].Mul(k).Add(out[i-1].Mul(oneMinusK)).Round(calcScale)
	}
	return out
}

// wilderRSI 按 Wilder 原始定义计算相对强弱指标：
// 先取前 span 个涨跌幅的算术平均作为初始均值，之后用 (prev*(span-1)+cur)/span 平滑。
//
// 全跌（avgGain=0）返回 0、全涨（avgLoss=0）返回 100，这两个边界必须显式处理：
// 在 float64 下不处理会得到 NaN，在 decimal 下更直接——除零是 panic。
// 也就是说这几行短路不是防御性代码，是让进程不崩的必要条件。
func wilderRSI(closes []decimal.Decimal, span int) decimal.Decimal {
	if span <= 0 || len(closes) <= span {
		return decimal.Zero
	}

	spanDec := decimal.NewFromInt(int64(span))
	spanLess := decimal.NewFromInt(int64(span - 1))

	gain, loss := decimal.Zero, decimal.Zero
	for i := 1; i <= span; i++ {
		d := closes[i].Sub(closes[i-1])
		if d.IsPositive() {
			gain = gain.Add(d)
		} else {
			loss = loss.Sub(d)
		}
	}
	avgGain := gain.DivRound(spanDec, calcScale)
	avgLoss := loss.DivRound(spanDec, calcScale)

	for i := span + 1; i < len(closes); i++ {
		d := closes[i].Sub(closes[i-1])
		up, down := decimal.Zero, decimal.Zero
		if d.IsPositive() {
			up = d
		} else {
			down = d.Neg()
		}
		avgGain = avgGain.Mul(spanLess).Add(up).DivRound(spanDec, calcScale)
		avgLoss = avgLoss.Mul(spanLess).Add(down).DivRound(spanDec, calcScale)
	}

	switch {
	case avgLoss.IsZero() && avgGain.IsZero():
		return fifty // 完全横盘：多空力量相等。
	case avgLoss.IsZero():
		return hundred
	case avgGain.IsZero():
		return decimal.Zero
	}
	rs := avgGain.DivRound(avgLoss, calcScale)
	return decimalx.RoundIndicator(hundred.Sub(hundred.DivRound(dOne.Add(rs), calcScale)))
}

// wilderATR 计算平均真实波幅：TR = max(high-low, |high-prevClose|, |low-prevClose|)，
// 初值取前 span 个 TR 的算术平均，之后用 Wilder 平滑。
func wilderATR(highs, lows, closes []decimal.Decimal, span int) decimal.Decimal {
	n := len(closes)
	if span <= 0 || n <= span {
		return decimal.Zero
	}

	tr := make([]decimal.Decimal, 0, n-1)
	for i := 1; i < n; i++ {
		hl := highs[i].Sub(lows[i])
		hc := highs[i].Sub(closes[i-1]).Abs()
		lc := lows[i].Sub(closes[i-1]).Abs()
		tr = append(tr, decimal.Max(hl, hc, lc))
	}
	if len(tr) < span {
		return decimal.Zero
	}

	spanDec := decimal.NewFromInt(int64(span))
	spanLess := decimal.NewFromInt(int64(span - 1))

	atr := decimalx.Sum(tr[:span]).DivRound(spanDec, calcScale)
	for i := span; i < len(tr); i++ {
		atr = atr.Mul(spanLess).Add(tr[i]).DivRound(spanDec, calcScale)
	}
	return decimalx.RoundIndicator(atr)
}
