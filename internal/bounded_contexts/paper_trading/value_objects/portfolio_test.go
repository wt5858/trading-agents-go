package value_objects

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("解析 decimal 失败 %q: %v", s, err)
	}
	return d
}

func code(t *testing.T, raw string) shared_vo.StockCode {
	t.Helper()
	c, err := shared_vo.NewStockCode(raw, shared_vo.MarketUnknown)
	if err != nil {
		t.Fatalf("解析股票代码失败 %q: %v", raw, err)
	}
	return c
}

// TestValuePositionUsesStoredAvgCost 验证浮动盈亏这个「唯一例外」的算式，
// 其中依赖实时报价的只有价格那一半，成本那一半必须是**落库的** AvgCost。
//
// 用例里 CostBasis / Quantity = 33.3333…，而落库的 AvgCost 是 33.3000。
// 断言结果对应后者：如果哪天有人把 AvgCost 改成现场用成本基数除数量，
// 这个测试会失败。
func TestValuePositionUsesStoredAvgCost(t *testing.T) {
	c := code(t, "AAPL")
	quote := NewLiveQuote(c, dec(t, "40"), time.Now())

	v := ValuePosition(c, dec(t, "3"), dec(t, "33.3000"), dec(t, "100"), quote)

	if !v.HasQuote {
		t.Fatal("有可用报价时 HasQuote 应为 true")
	}
	if !v.MarketValue.Equal(dec(t, "120")) {
		t.Fatalf("市值应为 120，实际 %s", v.MarketValue.String())
	}
	// (40 − 33.3) × 3 = 20.1。若用 100/3 重算平均成本会得到 20.0001。
	if !v.UnrealizedPnL.Equal(dec(t, "20.1")) {
		t.Fatalf("浮动盈亏应为 20.1（基于落库均价），实际 %s", v.UnrealizedPnL.String())
	}
	// CostBasis 原样搬运，不被市值覆盖。
	if !v.CostBasis.Equal(dec(t, "100")) {
		t.Fatalf("成本基数应原样搬运，实际 %s", v.CostBasis.String())
	}
}

// TestValuePositionFallsBackWhenQuoteMissing 验证拿不到行情时的降级行为：
// 回退到成本价估值、浮动盈亏为 0，并通过 HasQuote 把这个事实透出去。
//
// 关键在于**不能**用价格 0 去估值：那会让整个组合市值凭空归零，
// 看板上表现为一次假的爆仓。
func TestValuePositionFallsBackWhenQuoteMissing(t *testing.T) {
	c := code(t, "600519.SH")
	// 零值报价 = 没拿到数据。
	v := ValuePosition(c, dec(t, "100"), dec(t, "50.05"), dec(t, "5005"), LiveQuote{})

	if v.HasQuote {
		t.Fatal("没有可用报价时 HasQuote 必须为 false")
	}
	if !v.MarketPrice.Equal(dec(t, "50.05")) {
		t.Fatalf("应回退到成本价，实际 %s", v.MarketPrice.String())
	}
	if !v.MarketValue.Equal(dec(t, "5005")) {
		t.Fatalf("回退市值应等于成本，实际 %s", v.MarketValue.String())
	}
	if !v.UnrealizedPnL.IsZero() {
		t.Fatalf("拿不到行情就不该编造盈亏，实际 %s", v.UnrealizedPnL.String())
	}
}

// TestPortfolioSummaryTotals 验证组合汇总的加法链路，
// 尤其是「已实现盈亏来自账户落库值，不是把持仓再算一遍」。
func TestPortfolioSummaryTotals(t *testing.T) {
	a := code(t, "600519.SH")
	b := code(t, "000001.SZ")

	positions := []PositionValuation{
		ValuePosition(a, dec(t, "100"), dec(t, "50"), dec(t, "5000"), NewLiveQuote(a, dec(t, "60"), time.Now())),
		ValuePosition(b, dec(t, "200"), dec(t, "10"), dec(t, "2000"), NewLiveQuote(b, dec(t, "9"), time.Now())),
	}

	s := NewPortfolioSummary("pacct_1", dec(t, "3000"), dec(t, "10000"), dec(t, "150"), positions)

	if s.PositionCount != 2 {
		t.Fatalf("持仓数应为 2，实际 %d", s.PositionCount)
	}
	if !s.TotalCost.Equal(dec(t, "7000")) {
		t.Fatalf("持仓成本合计应为 7000，实际 %s", s.TotalCost.String())
	}
	// 6000 + 1800 = 7800
	if !s.MarketValue.Equal(dec(t, "7800")) {
		t.Fatalf("市值合计应为 7800，实际 %s", s.MarketValue.String())
	}
	if !s.TotalValue.Equal(dec(t, "10800")) {
		t.Fatalf("总资产应为 10800（现金 3000 + 市值 7800），实际 %s", s.TotalValue.String())
	}
	// (60−50)×100 = 1000，(9−10)×200 = −200，合计 800。
	if !s.UnrealizedPnL.Equal(dec(t, "800")) {
		t.Fatalf("浮动盈亏合计应为 800，实际 %s", s.UnrealizedPnL.String())
	}
	// 已实现盈亏是账户传进来的落库值，原样呈现。
	if !s.RealizedPnL.Equal(dec(t, "150")) {
		t.Fatalf("已实现盈亏应为落库的 150，实际 %s", s.RealizedPnL.String())
	}
	if !s.TotalPnL().Equal(dec(t, "950")) {
		t.Fatalf("总盈亏应为 950，实际 %s", s.TotalPnL().String())
	}
}

// TestMoneyParsingRejectsGarbage 验证边界上的金额解析：
// 字符串入、decimal 出，垃圾输入在入口就被拒。
func TestMoneyParsingRejectsGarbage(t *testing.T) {
	if _, err := ParseMoney("", "初始资金"); err == nil {
		t.Fatal("空字符串应被拒绝")
	}
	if _, err := ParseMoney("1e", "初始资金"); err == nil {
		t.Fatal("非法数字应被拒绝")
	}
	// 多余的小数位在入口就被收敛到落库精度，避免内存值与落库值分叉。
	d, err := ParseMoney("1.123456789", "初始资金")
	if err != nil {
		t.Fatalf("合法数字解析失败: %v", err)
	}
	if !d.Equal(dec(t, "1.1235")) {
		t.Fatalf("金额应收敛到 4 位小数，实际 %s", d.String())
	}
	if got := FormatMoney(dec(t, "100")); got != "100.0000" {
		t.Fatalf("金额应定长渲染，实际 %s", got)
	}
}

// TestOrderSideParsing 验证买卖方向只在唯一构造点做规范化。
func TestOrderSideParsing(t *testing.T) {
	for _, raw := range []string{"buy", "BUY", " Buy "} {
		s, err := NewOrderSide(raw)
		if err != nil || !s.IsBuy() {
			t.Fatalf("%q 应解析为买入，err=%v", raw, err)
		}
	}
	if _, err := NewOrderSide("b"); err == nil {
		t.Fatal("未知方向必须报错，绝不退化成默认值")
	}
}
