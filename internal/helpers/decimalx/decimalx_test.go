package decimalx

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestSqrt 覆盖完全平方数、无理数、零与负数。
// 完全平方数必须精确相等——如果 Sqrt 内部借道了 float64，
// 大数的完全平方根就会在末位出现偏差，这条用例就是用来钉死这一点的。
func TestSqrt(t *testing.T) {
	cases := []struct {
		in    string
		scale int32
		want  string
	}{
		{"4", 6, "2"},
		{"2", 6, "1.414213"},
		{"0.25", 6, "0.5"},
		{"1000000", 6, "1000"},
		// 17 位完全平方数：float64 只有 53 位尾数（约 15.9 位十进制），
		// 借道 math.Sqrt 的实现在这里会错位。
		{"12345678987654321", 0, "111111111"},
		{"0", 6, "0"},
		{"-1", 6, "0"},
	}
	for _, c := range cases {
		got := Sqrt(decimal.RequireFromString(c.in), c.scale)
		want := decimal.RequireFromString(c.want)
		if !got.Equal(want) {
			t.Errorf("Sqrt(%s, %d) = %s, want %s", c.in, c.scale, got, want)
		}
	}
}

// TestPopulationStdDev 用一组手算可验的数据核对标准差。
// xs = 2,4,4,4,5,5,7,9，mean = 5，总体标准差 = 2。
func TestPopulationStdDev(t *testing.T) {
	xs := make([]decimal.Decimal, 0, 8)
	for _, v := range []int64{2, 4, 4, 4, 5, 5, 7, 9} {
		xs = append(xs, decimal.NewFromInt(v))
	}
	mean := Mean(xs, 6)
	if !mean.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("Mean = %s, want 5", mean)
	}
	got := PopulationStdDev(xs, mean, 6)
	if !got.Equal(decimal.NewFromInt(2)) {
		t.Errorf("PopulationStdDev = %s, want 2", got)
	}
}

// TestDecimalArithmeticIsExact 是这次迁移的立论：同样的连加，
// float64 会漂，decimal 不会。
func TestDecimalArithmeticIsExact(t *testing.T) {
	a := decimal.RequireFromString("0.1")
	b := decimal.RequireFromString("0.2")
	if !a.Add(b).Equal(decimal.RequireFromString("0.3")) {
		t.Fatal("0.1 + 0.2 != 0.3")
	}

	// 一万次累加 0.01，精确等于 100。float64 在这里会得到 100.00000000001884。
	acc := decimal.Zero
	step := decimal.RequireFromString("0.01")
	for i := 0; i < 10000; i++ {
		acc = acc.Add(step)
	}
	if !acc.Equal(decimal.NewFromInt(100)) {
		t.Errorf("累加 10000 次 0.01 = %s, want 100", acc)
	}
}

func TestPercentChange(t *testing.T) {
	cases := []struct{ cur, base, want string }{
		{"11", "10", "10"},
		{"9", "10", "-10"},
		{"10", "10", "0"},
		// 前收盘价为零（新股首日）不应 panic，返回零。
		{"10", "0", "0"},
	}
	for _, c := range cases {
		got := PercentChange(decimal.RequireFromString(c.cur), decimal.RequireFromString(c.base))
		if !got.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("PercentChange(%s, %s) = %s, want %s", c.cur, c.base, got, c.want)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := ParseMoney("", "金额"); err == nil {
		t.Error("空串应当报错")
	}
	if _, err := ParseMoney("abc", "金额"); err == nil {
		t.Error("非法数字应当报错")
	}
	// 超出精度的输入在入口就收敛，而不是流进领域层。
	d, err := ParseMoney("1.123456789", "金额")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := FormatMoney(d); got != "1.1235" {
		t.Errorf("ParseMoney 精度收敛 = %s, want 1.1235", got)
	}
}

func TestFormatPtrDistinguishesNilFromZero(t *testing.T) {
	if got := FormatPtr(nil, MoneyScale); got != "" {
		t.Errorf("nil 应渲染成空串, got %q", got)
	}
	zero := decimal.Zero
	if got := FormatPtr(&zero, MoneyScale); got != "0.0000" {
		t.Errorf("零值应渲染成 0.0000, got %q", got)
	}
}
