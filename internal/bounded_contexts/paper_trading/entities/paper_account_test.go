package entities

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 测试全程用 decimal 断言精确值，不用 float64 的容差比较。
// 「差不多对」正是这个上下文要杜绝的东西：一个只能近似对平的账本，
// 无法回答「我的策略到底赚了多少」。

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("解析 decimal 失败 %q: %v", s, err)
	}
	return d
}

func mustCode(t *testing.T, raw string) shared_vo.StockCode {
	t.Helper()
	c, err := shared_vo.NewStockCode(raw, shared_vo.MarketUnknown)
	if err != nil {
		t.Fatalf("解析股票代码失败 %q: %v", raw, err)
	}
	return c
}

func mustAccount(t *testing.T, initialCash string) *PaperAccount {
	t.Helper()
	a, err := OpenPaperAccount("pacct_test", 42, "测试账户", dec(t, initialCash))
	if err != nil {
		t.Fatalf("开户失败: %v", err)
	}
	return a
}

func assertDec(t *testing.T, got decimal.Decimal, want string, what string) {
	t.Helper()
	w := dec(t, want)
	if !got.Equal(w) {
		t.Fatalf("%s 期望 %s，实际 %s", what, w.String(), got.String())
	}
}

func assertCode(t *testing.T, err error, want custom_errors.Code, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s 期望报错 %s，实际成功", what, want)
	}
	if got := custom_errors.CodeOf(err); got != want {
		t.Fatalf("%s 期望错误码 %s，实际 %s（%v）", what, want, got, err)
	}
}

// TestBuyThenSellRealizedPnL 验证「买入 → 卖出」全链路的已实现盈亏，含两端手续费。
//
// 最关键的断言是最后一条账本恒等式：
//
//	期末现金 == 初始资金 + 已实现盈亏
//
// 清仓之后账户里只剩现金，如果这条等式不成立，说明手续费、成本基数
// 或者取整口径中至少有一处在某个环节被算了两次或漏算了一次。
func TestBuyThenSellRealizedPnL(t *testing.T) {
	a := mustAccount(t, "100000")
	code := mustCode(t, "600519.SH")

	// 买入 100 股 @ 50，手续费 5。
	// 成交金额 = 5000，含费总成本 = 5005，平均成本 = 50.05。
	buy, err := a.Buy(code, dec(t, "100"), dec(t, "50"), dec(t, "5"))
	if err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	assertDec(t, buy.Amount, "5000", "买入成交金额")
	assertDec(t, buy.RealizedPnL, "0", "买入不实现盈亏")
	assertDec(t, a.Cash, "94995", "买入后现金")

	pos := a.PositionOf(code)
	if pos == nil {
		t.Fatal("买入后应有持仓")
	}
	assertDec(t, pos.AvgCost, "50.05", "平均成本（含买入手续费）")
	assertDec(t, pos.CostBasis, "5005", "成本基数")

	// 卖出 100 股 @ 60，手续费 6。
	// 已实现盈亏 = (60 − 50.05) × 100 − 6 = 995 − 6 = 989。
	sell, err := a.Sell(code, dec(t, "100"), dec(t, "60"), dec(t, "6"))
	if err != nil {
		t.Fatalf("卖出失败: %v", err)
	}
	assertDec(t, sell.Amount, "6000", "卖出成交金额")
	assertDec(t, sell.Fee, "6", "卖出手续费")
	assertDec(t, sell.RealizedPnL, "989", "本笔已实现盈亏")
	assertDec(t, a.RealizedPnL, "989", "账户已实现盈亏累计")
	assertDec(t, a.TotalFee, "11", "累计手续费")
	assertDec(t, a.Cash, "100989", "卖出后现金")

	// 账本恒等式：清仓后现金必须精确等于 初始资金 + 已实现盈亏。
	assertDec(t, a.InitialCash.Add(a.RealizedPnL), "100989", "初始资金 + 已实现盈亏")

	// 成交记录挂在聚合上等待随事务落库，两笔都在。
	if len(a.NewTrades) != 2 {
		t.Fatalf("期望累积 2 笔待落库成交，实际 %d", len(a.NewTrades))
	}
	if drained := a.TakeNewTrades(); len(drained) != 2 || len(a.NewTrades) != 0 {
		t.Fatalf("TakeNewTrades 应取走并清空，实际取走 %d，剩余 %d", len(drained), len(a.NewTrades))
	}
}

// TestAverageCostAcrossMultipleBuys 验证多次买入后的移动加权平均成本，
// 以及卖出之后平均成本保持不变（平均成本法的核心性质）。
func TestAverageCostAcrossMultipleBuys(t *testing.T) {
	a := mustAccount(t, "1000000")
	code := mustCode(t, "000001.SZ")

	// 第一笔：100 @ 10，无手续费 → 成本 1000，均价 10。
	if _, err := a.Buy(code, dec(t, "100"), dec(t, "10"), decimal.Zero); err != nil {
		t.Fatalf("第一笔买入失败: %v", err)
	}
	assertDec(t, a.PositionOf(code).AvgCost, "10", "首笔后平均成本")

	// 第二笔：300 @ 20 → 成本 1000 + 6000 = 7000，数量 400，均价 17.5。
	if _, err := a.Buy(code, dec(t, "300"), dec(t, "20"), decimal.Zero); err != nil {
		t.Fatalf("第二笔买入失败: %v", err)
	}
	assertDec(t, a.PositionOf(code).AvgCost, "17.5", "两笔后平均成本")
	assertDec(t, a.PositionOf(code).CostBasis, "7000", "两笔后成本基数")

	// 第三笔：100 @ 12.5，手续费 50 → 含费成本 1300，
	// 总成本 8300，数量 500，均价 16.6。手续费计入成本基数。
	if _, err := a.Buy(code, dec(t, "100"), dec(t, "12.5"), dec(t, "50")); err != nil {
		t.Fatalf("第三笔买入失败: %v", err)
	}
	pos := a.PositionOf(code)
	assertDec(t, pos.Quantity, "500", "三笔后持仓数量")
	assertDec(t, pos.CostBasis, "8300", "三笔后成本基数")
	assertDec(t, pos.AvgCost, "16.6", "三笔后平均成本")

	// 账本恒等式：还没有卖出，现金必须精确等于 初始资金 − 成本基数。
	assertDec(t, a.Cash, "991700", "三笔买入后现金")
	assertDec(t, a.InitialCash.Sub(pos.CostBasis), "991700", "初始资金 − 成本基数")

	// 部分卖出 200 @ 20：已实现盈亏 = (20 − 16.6) × 200 = 680，
	// 释放成本 = 16.6 × 200 = 3320，剩余成本基数 4980，
	// 而平均成本必须**保持 16.6 不变**——卖出不改变剩余持仓的单位成本。
	sell, err := a.Sell(code, dec(t, "200"), dec(t, "20"), decimal.Zero)
	if err != nil {
		t.Fatalf("部分卖出失败: %v", err)
	}
	assertDec(t, sell.RealizedPnL, "680", "部分卖出已实现盈亏")

	pos = a.PositionOf(code)
	assertDec(t, pos.Quantity, "300", "部分卖出后数量")
	assertDec(t, pos.CostBasis, "4980", "部分卖出后成本基数")
	assertDec(t, pos.AvgCost, "16.6", "部分卖出后平均成本不变")
}

// TestInsufficientCashRejected 验证资金不足被拒，且账户状态**完全不变**。
//
// 校验与变更在同一个方法里，所以一次失败的买入不允许留下任何痕迹：
// 现金不能少一分，持仓不能多一行，成交记录也不能多一条。
func TestInsufficientCashRejected(t *testing.T) {
	a := mustAccount(t, "1000")
	code := mustCode(t, "AAPL")

	// 需要 100 × 10 + 1 = 1001，账户只有 1000。
	_, err := a.Buy(code, dec(t, "100"), dec(t, "10"), dec(t, "1"))
	assertCode(t, err, custom_errors.CodeConflict, "资金不足的买入")

	assertDec(t, a.Cash, "1000", "被拒后现金")
	if a.PositionOf(code) != nil {
		t.Fatal("被拒的买入不应建仓")
	}
	if len(a.NewTrades) != 0 {
		t.Fatalf("被拒的买入不应产生成交记录，实际 %d 条", len(a.NewTrades))
	}

	// 刚好用完全部现金是允许的：边界是「超过」才拒绝，不是「用尽」就拒绝。
	if _, err := a.Buy(code, dec(t, "100"), dec(t, "9.99"), dec(t, "1")); err != nil {
		t.Fatalf("恰好用尽现金的买入应当成功: %v", err)
	}
	assertDec(t, a.Cash, "0", "用尽现金后余额")
}

// TestInsufficientSharesRejected 验证持仓不足与空仓卖出都被拒，且状态不变。
func TestInsufficientSharesRejected(t *testing.T) {
	a := mustAccount(t, "100000")
	code := mustCode(t, "00700.HK")

	// 完全没有持仓时卖出。
	_, err := a.Sell(code, dec(t, "1"), dec(t, "100"), decimal.Zero)
	assertCode(t, err, custom_errors.CodeConflict, "空仓卖出")

	if _, err := a.Buy(code, dec(t, "100"), dec(t, "100"), decimal.Zero); err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	cashAfterBuy := a.Cash

	// 持有 100，卖 101。
	_, err = a.Sell(code, dec(t, "101"), dec(t, "120"), decimal.Zero)
	assertCode(t, err, custom_errors.CodeConflict, "超量卖出")

	assertDec(t, a.Cash, cashAfterBuy.String(), "被拒后现金不变")
	assertDec(t, a.PositionOf(code).Quantity, "100", "被拒后持仓不变")
	assertDec(t, a.RealizedPnL, "0", "被拒后已实现盈亏不变")

	// 数量非正是参数错误（Invalid），不是状态冲突（Conflict）——
	// 这个区分让调用方知道该改参数还是该重试。
	_, err = a.Sell(code, decimal.Zero, dec(t, "120"), decimal.Zero)
	assertCode(t, err, custom_errors.CodeInvalidArgument, "数量为 0 的卖出")
	_, err = a.Buy(code, dec(t, "-1"), dec(t, "120"), decimal.Zero)
	assertCode(t, err, custom_errors.CodeInvalidArgument, "数量为负的买入")
}

// TestSellEntirePositionRemovesIt 验证清仓后持仓行被移除，而不是留一行数量为 0。
//
// 留空行会让持仓数统计多算一只，也会让「我现在持有哪些票」给出错误答案；
// 在持久化侧它还会让仓储的 diff 永远删不掉那一行。
func TestSellEntirePositionRemovesIt(t *testing.T) {
	a := mustAccount(t, "100000")
	held := mustCode(t, "600519.SH")
	other := mustCode(t, "000001.SZ")

	if _, err := a.Buy(held, dec(t, "100"), dec(t, "50"), decimal.Zero); err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	if _, err := a.Buy(other, dec(t, "200"), dec(t, "10"), decimal.Zero); err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	if len(a.Positions) != 2 {
		t.Fatalf("期望 2 个持仓，实际 %d", len(a.Positions))
	}

	if _, err := a.Sell(held, dec(t, "100"), dec(t, "55"), decimal.Zero); err != nil {
		t.Fatalf("清仓卖出失败: %v", err)
	}
	if a.PositionOf(held) != nil {
		t.Fatal("清仓后该标的不应再有持仓")
	}
	if len(a.Positions) != 1 {
		t.Fatalf("清仓后应只剩 1 个持仓，实际 %d", len(a.Positions))
	}
	// 移除的是正确的那一行，另一只票必须原封不动。
	if a.PositionOf(other) == nil {
		t.Fatal("清仓不应影响其它标的的持仓")
	}
	assertDec(t, a.PositionOf(other).Quantity, "200", "另一标的持仓数量")
}

// TestStoredTradeAmountIsNeverRecomputed 是这份测试里最「反直觉」的一条，
// 也是最重要的一条。
//
// 它构造了一笔 Amount 与 Quantity × Price 故意不相等的成交
// （现实里的来源：历史上用过不同的取整口径、或者数据迁移留下的行），
// 然后断言读路径把**落库的 Amount** 原样交出来。
//
// 如果哪天有人把 ToRecord 里的 Amount 改成 Quantity.Mul(Price)，
// 这个测试会立刻失败——这正是它存在的意义：真正从现金里划走的是落库的那个数，
// 读路径重算出来的是另一个数字，两者一旦分叉，账本就永远对不平。
func TestStoredTradeAmountIsNeverRecomputed(t *testing.T) {
	stored := &Trade{
		ID:        "ptrade_legacy",
		AccountID: "pacct_test",
		Code:      mustCode(t, "600519.SH"),
		Side:      value_objects.SideBuy,
		Quantity:  dec(t, "100"),
		Price:     dec(t, "50.005"),
		// 数量 × 价格 = 5000.5，但当时真正划走的是 5000.4900。
		Amount:      dec(t, "5000.4900"),
		Fee:         dec(t, "5"),
		RealizedPnL: decimal.Zero,
		CashAfter:   dec(t, "94994.51"),
		TradedAt:    time.Now(),
	}

	recomputed := stored.Quantity.Mul(stored.Price)
	if recomputed.Equal(stored.Amount) {
		t.Fatal("用例前提失效：本用例需要一个 Amount != Quantity × Price 的成交")
	}

	record := stored.ToRecord()
	assertDec(t, record.Amount, "5000.4900", "读路径上的成交金额必须是落库的存量值")
	if record.Amount.Equal(recomputed) {
		t.Fatal("读路径重算了 Quantity × Price，这会让展示金额与现金变动分叉")
	}

	// 净现金流也只用两个存量值做加减（Amount 与 Fee），不涉及乘除。
	assertDec(t, record.NetCashFlow(), "-5005.4900", "买入净现金流")
}

// TestRehydratePositionKeepsStoredAvgCost 验证从库里读回持仓时不重算平均成本。
//
// 与 stock 上下文 RehydrateSyncStats 不重算成功率是同一条纪律：
// 存量值才是当时的事实，用 CostBasis / Quantity 现除一遍会得到一个
// 略微不同的数，于是同一笔持仓在不同代码路径上算出不同的已实现盈亏。
func TestRehydratePositionKeepsStoredAvgCost(t *testing.T) {
	code := mustCode(t, "AAPL")
	// 故意让 costBasis / quantity = 33.3333…，与落库的 33.3000 不等。
	pos := RehydratePosition(code, dec(t, "3"), dec(t, "33.3000"), dec(t, "100"), time.Now(), time.Now())
	assertDec(t, pos.AvgCost, "33.3000", "读回的平均成本必须是存量值")

	a := mustAccount(t, "100000")
	a.Positions = []*Position{pos}

	// 卖出必须用存量的 33.3000 计算：(40 − 33.3) × 3 = 20.1。
	// 若改用 100/3 = 33.3333… 重算，结果会是 20.0001，与库里的成本对不上。
	sell, err := a.Sell(code, dec(t, "3"), dec(t, "40"), decimal.Zero)
	if err != nil {
		t.Fatalf("卖出失败: %v", err)
	}
	assertDec(t, sell.RealizedPnL, "20.1", "已实现盈亏必须基于落库的平均成本")
}

// TestResetRestoresInitialState 验证重置把账户恢复到开户状态，
// 并且明确记录了被丢弃的已实现盈亏（成交历史另行保留，不在本聚合里）。
func TestResetRestoresInitialState(t *testing.T) {
	a := mustAccount(t, "100000")
	code := mustCode(t, "600519.SH")

	if _, err := a.Buy(code, dec(t, "100"), dec(t, "50"), dec(t, "5")); err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	if _, err := a.Sell(code, dec(t, "50"), dec(t, "60"), dec(t, "3")); err != nil {
		t.Fatalf("卖出失败: %v", err)
	}
	a.GetAllPendingEvents() // 清掉成交事件，只看重置事件

	if err := a.Reset(); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	assertDec(t, a.Cash, "100000", "重置后现金")
	assertDec(t, a.RealizedPnL, "0", "重置后已实现盈亏")
	assertDec(t, a.TotalFee, "0", "重置后累计手续费")
	if len(a.Positions) != 0 {
		t.Fatalf("重置后不应有持仓，实际 %d", len(a.Positions))
	}

	evts := a.GetAllPendingEvents()
	if len(evts) != 1 {
		t.Fatalf("重置应产生 1 个领域事件，实际 %d", len(evts))
	}
	if evts[0].Name() != "paper_trading.account_reset" {
		t.Fatalf("事件名不符: %s", evts[0].Name())
	}
}

// TestOpenAccountValidation 验证开户的形状校验。
func TestOpenAccountValidation(t *testing.T) {
	if _, err := OpenPaperAccount("", 1, "x", dec(t, "100")); err == nil {
		t.Fatal("空 ID 应被拒绝")
	}
	if _, err := OpenPaperAccount("id", 0, "x", dec(t, "100")); err == nil {
		t.Fatal("无归属用户应被拒绝")
	}
	_, err := OpenPaperAccount("id", 1, "x", decimal.Zero)
	assertCode(t, err, custom_errors.CodeInvalidArgument, "初始资金为 0 的开户")
}
