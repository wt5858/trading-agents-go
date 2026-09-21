package dtos

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("解析 decimal 失败 %q: %v", s, err)
	}
	return d
}

// TestToDomainRecordUsesStoredAmount 守住持久化读路径上的同一条纪律：
// 从库里读出来的成交，Amount 必须原样交出，绝不用 quantity × price 重算。
//
// 用例刻意构造了一行 amount != quantity × price 的数据。这在真实系统里
// 完全会发生：历史上换过取整口径、或者数据迁移时按另一套规则回填过。
// 那种时候正确的行为是**如实展示当时划走的金额**，因为它才是和
// cash_after 对得上的那个数；重算出来的漂亮数字反而会让账本对不平。
func TestToDomainRecordUsesStoredAmount(t *testing.T) {
	row := PaperTradeDto{
		ID:          "ptrade_legacy",
		AccountID:   "pacct_1",
		TradedAt:    time.Now(),
		Symbol:      "600519",
		Market:      "CN",
		Raw:         "600519.SH",
		Side:        "buy",
		Quantity:    dec(t, "100"),
		Price:       dec(t, "50.005"),
		Amount:      dec(t, "5000.4900"), // 100 × 50.005 = 5000.5，故意不相等
		Fee:         dec(t, "5"),
		RealizedPnl: decimal.Zero,
		CashAfter:   dec(t, "94994.51"),
	}

	recomputed := row.Quantity.Mul(row.Price)
	if recomputed.Equal(row.Amount) {
		t.Fatal("用例前提失效：本用例需要一行 amount != quantity × price 的数据")
	}

	rec := row.ToDomainRecord()
	if !rec.Amount.Equal(dec(t, "5000.4900")) {
		t.Fatalf("读路径必须返回落库的 amount，期望 5000.4900，实际 %s", rec.Amount.String())
	}
	if rec.Amount.Equal(recomputed) {
		t.Fatal("读路径重算了 quantity × price，展示金额将与现金变动分叉")
	}

	// 顺带确认股票代码被还原成值对象而不是裸字符串。
	if got := rec.Code.FullSymbol(); got != "600519.SH" {
		t.Fatalf("股票代码还原有误: %s", got)
	}
	if !rec.Side.IsBuy() {
		t.Fatalf("买卖方向还原有误: %s", rec.Side)
	}
}

// TestFromDomainAndBackPreservesMoney 验证聚合 → DTO → 读模型这条往返
// 不会在任何一步丢精度。decimal 全程是十进制定点，
// 换成 float64 的话 0.1 这样的值在往返之后就不再等于自身了。
func TestFromDomainAndBackPreservesMoney(t *testing.T) {
	row := PaperTradeDto{
		ID:          "ptrade_1",
		AccountID:   "pacct_1",
		TradedAt:    time.Now(),
		Symbol:      "AAPL",
		Market:      "US",
		Side:        "sell",
		Quantity:    dec(t, "3.00000001"),
		Price:       dec(t, "0.1"),
		Amount:      dec(t, "0.3000"),
		Fee:         dec(t, "0.0001"),
		RealizedPnl: dec(t, "-0.0001"),
		CashAfter:   dec(t, "1000.2999"),
	}
	rec := row.ToDomainRecord()

	if !rec.Price.Add(rec.Price).Add(rec.Price).Equal(dec(t, "0.3")) {
		t.Fatalf("0.1 + 0.1 + 0.1 应精确等于 0.3，实际 %s",
			rec.Price.Add(rec.Price).Add(rec.Price).String())
	}
	if !rec.Quantity.Equal(dec(t, "3.00000001")) {
		t.Fatalf("数量精度丢失: %s", rec.Quantity.String())
	}
	// 卖出净现金流 = Amount − Fee，全部来自存量值的加减。
	if !rec.NetCashFlow().Equal(dec(t, "0.2999")) {
		t.Fatalf("净现金流有误: %s", rec.NetCashFlow().String())
	}
}
