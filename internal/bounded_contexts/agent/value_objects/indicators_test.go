package value_objects

import (
	"testing"

	"github.com/shopspring/decimal"

	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// buildKlines 造一段 K 线。high = close+1、low = close-1 让真实波幅恒为 2，
// 这样 ATR 的期望值可以手算出来。交易日从 2024-01-01 起逐日递增。
func buildKlines(closes []decimal.Decimal, volume int64) []stock_vo.Kline {
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		panic(err)
	}
	vol := decimal.NewFromInt(volume)
	one := decimal.NewFromInt(1)
	out := make([]stock_vo.Kline, 0, len(closes))
	day := shared_vo.MustTradeDate("2024-01-01")
	for i, c := range closes {
		out = append(out, stock_vo.Kline{
			Code:      code,
			Period:    stock_vo.PeriodDaily,
			TradeDate: day.AddDays(i),
			Open:      c,
			High:      c.Add(one),
			Low:       c.Sub(one),
			Close:     c,
			Volume:    vol,
			Amount:    c.Mul(vol),
			Source:    "test",
		})
	}
	return out
}

// ramp 生成 start, start+1, ... 共 n 个整数收盘价。
func ramp(start, n int64) []decimal.Decimal {
	out := make([]decimal.Decimal, 0, n)
	for i := int64(0); i < n; i++ {
		out = append(out, decimal.NewFromInt(start+i))
	}
	return out
}

// decs 把字面量列表转成收盘价序列。
func decs(vs ...int64) []decimal.Decimal {
	out := make([]decimal.Decimal, 0, len(vs))
	for _, v := range vs {
		out = append(out, decimal.NewFromInt(v))
	}
	return out
}

// assertEq 断言**精确**相等。
//
// 原先这里只有一个带 1e-9 容差的 assertClose——那个容差不是业务容差，
// 而是 float64 算不准均值的补偿。改用 decimal 后，凡是只涉及加减乘除的指标
// （均线、ATR、RSI 的三个边界、MACD 柱与 DIF/DEA 的关系）都能逐位对上，
// 容差没有存在的理由。只有真正开根的布林带上下轨仍需 assertNear。
func assertEq(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	w := decimal.RequireFromString(want)
	if !got.Equal(w) {
		t.Errorf("%s = %v, 期望 %v", name, got, w)
	}
}

// assertNear 用于无理数结果（开根），容差取落库精度的一个单位。
func assertNear(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	w := decimal.RequireFromString(want)
	tol := decimal.New(1, -decimalx.IndicatorScale)
	if got.Sub(w).Abs().GreaterThan(tol) {
		t.Errorf("%s = %v, 期望 %v（容差 %v）", name, got, w, tol)
	}
}

// TestComputeIndicators_MovingAverages 用等差数列把均线的期望值手算出来。
func TestComputeIndicators_MovingAverages(t *testing.T) {
	// 收盘价 10,11,...,29 共 20 根。
	in, err := ComputeIndicators(buildKlines(ramp(10, 20), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}

	if in.Samples != 20 {
		t.Errorf("Samples = %d, 期望 20", in.Samples)
	}
	assertEq(t, "Close", in.Close, "29")
	// 最后 5 根是 25..29，均值 27。
	assertEq(t, "MA5", in.MA5, "27")
	// 最后 10 根是 20..29，均值 24.5。
	assertEq(t, "MA10", in.MA10, "24.5")
	// 全部 20 根 10..29，均值 19.5。
	assertEq(t, "MA20", in.MA20, "19.5")
	// 样本不足 60 根，MA60 必须是 0 而不是「用现有数据凑一个」。
	assertEq(t, "MA60", in.MA60, "0")
	if in.HasMA60() {
		t.Error("HasMA60 应为 false")
	}
	// 乖离率 = (29-19.5)/19.5*100 = 48.717948...，按指标精度取到 6 位。
	assertNear(t, "DeviationMA20Pct", in.DeviationMA20Pct, "48.717949")
	if in.TrendState() != "多头排列" {
		t.Errorf("TrendState = %q, 期望 多头排列", in.TrendState())
	}
	// 量恒为 100，两条量能均线都应等于 100。
	assertEq(t, "VolMA5", in.VolMA5, "100")
	assertEq(t, "VolMA20", in.VolMA20, "100")
}

// TestComputeIndicators_Bollinger 校验布林带用的是总体标准差。
// 20 个连续整数的总体标准差 = sqrt((n^2-1)/12) = sqrt(399/12) = sqrt(33.25)
// = 5.766281297335398...，中轨 19.5，上下轨 = 19.5 ± 2×sd。
func TestComputeIndicators_Bollinger(t *testing.T) {
	in, err := ComputeIndicators(buildKlines(ramp(10, 20), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	assertEq(t, "BollMid", in.BollMid, "19.5")
	assertNear(t, "BollUpper", in.BollUpper, "31.032563")
	assertNear(t, "BollLower", in.BollLower, "7.967437")
	if in.BollPosition() != "中轨上方" {
		t.Errorf("BollPosition = %q, 期望 中轨上方", in.BollPosition())
	}
}

// TestComputeIndicators_ATR 真实波幅恒为 2，因此 ATR14 必须恰好是 2。
func TestComputeIndicators_ATR(t *testing.T) {
	in, err := ComputeIndicators(buildKlines(ramp(10, 30), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if !in.HasATR() {
		t.Fatal("HasATR 应为 true")
	}
	assertEq(t, "ATR14", in.ATR14, "2")
}

// TestComputeIndicators_RSIEdges 覆盖 RSI 的三个边界。
// 这三种情况在朴素实现里分别会算出 NaN、NaN 和 0/0；
// 在 decimal 下更严重——除零是 panic 而不是 NaN，所以这三条短路是必需的。
func TestComputeIndicators_RSIEdges(t *testing.T) {
	cases := []struct {
		name   string
		closes []decimal.Decimal
		want   string
	}{
		{"单边上涨应为 100", ramp(10, 20), "100"},
		{"单边下跌应为 0", decs(30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11), "0"},
		{"完全横盘应为 50", decs(10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10), "50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 不 panic 本身就是断言的一部分：这三种输入都会走到除法边界。
			in, err := ComputeIndicators(buildKlines(tc.closes, 100))
			if err != nil {
				t.Fatalf("计算失败: %v", err)
			}
			assertEq(t, "RSI14", in.RSI14, tc.want)
		})
	}
}

// TestComputeIndicators_RSIBalanced 交替涨跌一格，涨跌均值相等，RSI 必须是 50。
func TestComputeIndicators_RSIBalanced(t *testing.T) {
	in, err := ComputeIndicators(buildKlines(decs(10, 11, 10, 11, 10, 11, 10), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	assertEq(t, "RSI6", in.RSI6, "50")
	// 样本 7 根不足 14+1，RSI14 无法计算，必须固化成 0。
	assertEq(t, "RSI14", in.RSI14, "0")
	if in.RSIState() != "数据不足" {
		t.Errorf("RSIState = %q, 期望 数据不足", in.RSIState())
	}
}

// TestComputeIndicators_MACD 校验 MACD 的三项关系与样本门槛。
func TestComputeIndicators_MACD(t *testing.T) {
	short, err := ComputeIndicators(buildKlines(ramp(10, 20), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if short.HasMACD() {
		t.Error("20 根样本不应计算 MACD")
	}
	assertEq(t, "MACDDIF(样本不足)", short.MACDDIF, "0")

	long, err := ComputeIndicators(buildKlines(ramp(10, 40), 100))
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if !long.HasMACD() {
		t.Fatal("40 根样本应计算 MACD")
	}
	// 单边上涨时快线必然在慢线上方，DIF > 0。
	if !long.MACDDIF.IsPositive() {
		t.Errorf("单边上涨时 MACDDIF 应为正, got %v", long.MACDDIF)
	}
	// 柱 = 2×(DIF-DEA)，这是国内行情软件的画法，报告里的数字要和图对得上。
	// 三项都按指标精度取整，所以这里是精确相等而不是近似。
	assertEq(t, "MACDHist", long.MACDHist, two.Mul(long.MACDDIF.Sub(long.MACDDEA)).String())
	if long.MACDSignal() == "数据不足" {
		t.Error("MACDSignal 不应为数据不足")
	}
}

// TestComputeIndicators_OrderIndependent 是这次迁移新增的用例。
//
// float64 的加法不满足结合律，同一批 K 线换个顺序喂进来，求和结果可能在末位不同，
// 于是「同一天的 MA20」会随仓储返回顺序变化。decimal 的加法是精确的，
// 正序与倒序必须得到逐位相同的结果。
func TestComputeIndicators_OrderIndependent(t *testing.T) {
	closes := ramp(10, 40)
	forward := buildKlines(closes, 100)

	reversed := make([]stock_vo.Kline, len(forward))
	for i, k := range forward {
		reversed[len(forward)-1-i] = k
	}

	a, err := ComputeIndicators(forward)
	if err != nil {
		t.Fatalf("正序计算失败: %v", err)
	}
	b, err := ComputeIndicators(reversed)
	if err != nil {
		t.Fatalf("倒序计算失败: %v", err)
	}

	for _, c := range []struct {
		name string
		x, y decimal.Decimal
	}{
		{"MA20", a.MA20, b.MA20},
		{"EMA12", a.EMA12, b.EMA12},
		{"MACDDIF", a.MACDDIF, b.MACDDIF},
		{"RSI14", a.RSI14, b.RSI14},
		{"ATR14", a.ATR14, b.ATR14},
		{"BollUpper", a.BollUpper, b.BollUpper},
	} {
		if !c.x.Equal(c.y) {
			t.Errorf("%s 与输入顺序有关: 正序 %v, 倒序 %v", c.name, c.x, c.y)
		}
	}
}
