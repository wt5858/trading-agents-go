package value_objects

import (
	"testing"

	"github.com/shopspring/decimal"
)

// one 是比率的上界，供区间越界断言使用。
var one = decimal.NewFromInt(1)

// TestWilsonInterval_KnownValues 用几个手算得出的点位钉住公式。
//
// 这些数字不是从实现里跑出来再贴回去的——那样测试只能保证「以后和现在一样」，
// 连公式整个写错都发现不了。它们取自 Wilson 区间的标准结果，
// 容差放到 0.005 以吸收查表用的 z 值精度差异。
func TestWilsonInterval_KnownValues(t *testing.T) {
	cases := []struct {
		name           string
		hits, n        int
		wantLo, wantHi float64
	}{
		// 教科书上的经典例子：p=0.5 且样本适中。
		{"50/100", 50, 100, 0.4038, 0.5962},
		// 小样本下区间应当很宽——这正是要它的理由。
		{"15/30 小样本", 15, 30, 0.3304, 0.6696},
		// 大样本收窄。
		{"500/1000", 500, 1000, 0.4692, 0.5308},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WilsonInterval(tc.hits, tc.n)
			lo, _ := got.Lower.Float64()
			hi, _ := got.Upper.Float64()
			if diff := lo - tc.wantLo; diff > 0.005 || diff < -0.005 {
				t.Errorf("下界 = %.4f, 期望约 %.4f", lo, tc.wantLo)
			}
			if diff := hi - tc.wantHi; diff > 0.005 || diff < -0.005 {
				t.Errorf("上界 = %.4f, 期望约 %.4f", hi, tc.wantHi)
			}
		})
	}
}

// TestWilsonInterval_ExtremeProportions 是选用 Wilson 而非正态近似的全部理由。
//
// 正态近似在 p=0 或 p=1 时给出宽度为零的区间——「10 战 10 胜，
// 一致率 100%，区间 [100%, 100%]」，一个看起来无比确凿、实际毫无依据的数字。
// 这个项目早期的样本量正好会撞上这种情况。
func TestWilsonInterval_ExtremeProportions(t *testing.T) {
	all := WilsonInterval(10, 10)
	if all.Upper.String() != "1" {
		t.Errorf("全胜时上界应为 1，得到 %s", all.Upper.String())
	}
	lo, _ := all.Lower.Float64()
	if lo >= 0.9 {
		t.Errorf("全胜且样本仅 10 时下界不应高到 %.4f——那等于宣称已经确定无疑", lo)
	}
	if lo <= 0.5 {
		t.Errorf("下界 %.4f 过低，Wilson 在 10/10 时约为 0.72", lo)
	}

	none := WilsonInterval(0, 10)
	if none.Lower.String() != "0" {
		t.Errorf("全负时下界应为 0，得到 %s", none.Lower.String())
	}
	hi, _ := none.Upper.Float64()
	if hi <= 0.1 {
		t.Errorf("全负且样本仅 10 时上界不应低到 %.4f", hi)
	}
}

// TestWilsonInterval_StaysInUnitRange 守住「区间永远落在 [0,1]」。
//
// 这条性质正是正态近似做不到的：它在小样本极端比例下会给出负下界
// 或大于 1 的上界，而那样的数字印在报告上只会让人怀疑整套评测。
func TestWilsonInterval_StaysInUnitRange(t *testing.T) {
	for n := 1; n <= 50; n++ {
		for hits := 0; hits <= n; hits++ {
			ci := WilsonInterval(hits, n)
			if ci.Lower.IsNegative() {
				t.Fatalf("hits=%d n=%d 下界为负: %s", hits, n, ci.Lower)
			}
			if ci.Upper.GreaterThan(one) {
				t.Fatalf("hits=%d n=%d 上界超过 1: %s", hits, n, ci.Upper)
			}
			if ci.Lower.GreaterThan(ci.Upper) {
				t.Fatalf("hits=%d n=%d 下界大于上界: %s > %s", hits, n, ci.Lower, ci.Upper)
			}
		}
	}
}

// TestWilsonInterval_WidthShrinksWithSampleSize 锁定「样本越多区间越窄」。
//
// 这是区间存在的意义所在。反过来说，如果哪天有人把它改成一个与 n 无关的
// 固定容差，这个测试会立刻失败——而单看某一个点位的数值是发现不了的。
func TestWilsonInterval_WidthShrinksWithSampleSize(t *testing.T) {
	prev := WilsonInterval(5, 10).Width()
	for _, n := range []int{50, 100, 500, 1000} {
		cur := WilsonInterval(n/2, n).Width()
		if !cur.LessThan(prev) {
			t.Errorf("n=%d 的区间宽度 %s 没有比上一档 %s 更窄", n, cur, prev)
		}
		prev = cur
	}
}

// TestWilsonInterval_ZeroSamples 没有样本时不能伪装成「区间很宽」。
func TestWilsonInterval_ZeroSamples(t *testing.T) {
	ci := WilsonInterval(0, 0)
	if !ci.Lower.IsZero() || !ci.Upper.IsZero() {
		t.Errorf("零样本应返回零值区间，得到 [%s, %s]", ci.Lower, ci.Upper)
	}
	if ci.Level.String() != "0.95" {
		t.Errorf("置信水平应始终带出，得到 %s", ci.Level)
	}
}

// TestWilsonInterval_ClampsInvalidInput 防御越界输入。
// hits > n 只可能来自调用方的 bug，但静默算出一个荒唐的区间比夹取更糟。
func TestWilsonInterval_ClampsInvalidInput(t *testing.T) {
	if ci := WilsonInterval(20, 10); ci.Upper.GreaterThan(one) || ci.Lower.IsNegative() {
		t.Errorf("hits 超过 n 时应被夹取，得到 [%s, %s]", ci.Lower, ci.Upper)
	}
	if ci := WilsonInterval(-5, 10); ci.Lower.IsNegative() {
		t.Errorf("负命中数应被夹取，得到 [%s, %s]", ci.Lower, ci.Upper)
	}
}
