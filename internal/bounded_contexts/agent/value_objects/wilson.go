package value_objects

import (
	"math"

	"github.com/shopspring/decimal"
)

// z95 是 95% 置信水平对应的正态分位数。
const z95 = 1.959963984540054

// confidenceLevel95 是与 z95 配套的水平值，随区间一起返回。
var confidenceLevel95 = decimal.RequireFromString("0.95")

// WilsonInterval 返回二项比例的 Wilson 得分区间（95%）。
//
// # 为什么是 Wilson 而不是教科书上的正态近似
//
// 正态近似（p ± z·sqrt(p(1-p)/n)）在这里会给出明显错误的结论：
// 样本少或比例接近 0/1 时，它的区间会越出 [0,1]，
// 而 10 战 10 胜会得到一个宽度为零的区间——「一致率 100%，区间 [100%, 100%]」，
// 一个看起来无比确凿、实际毫无依据的数字。
// 这个项目的样本量正好落在那个区间（几十到几百条），且早期结果很容易是极端比例，
// 所以近似公式的缺陷不是理论问题，是必然会撞上的。
//
// Wilson 区间在同样的小样本下仍然落在 [0,1] 内，且 10 战 10 胜给出的是
// 大约 [72%, 100%]——它诚实地表达了「样本太少，下限可能低到七成」。
//
// # 为什么这里用 float64 而不是 decimal
//
// 本仓库的约定是「金额与参与计算的量一律 decimal」，那条规则针对的是
// 会被累加、比较、且差一分钱就是账错了的值。置信区间不是那种东西：
// 它是一个统计量，输入是两个整数（命中数、样本数），输出只用于展示与判读，
// 精度到小数点后四位远超它的解释力。而 Wilson 公式需要开平方，
// shopspring/decimal 没有 Sqrt，硬要用 decimal 就得自己实现牛顿迭代——
// 为一个展示用的统计量引入一段自研数值算法，才是真正的风险。
// 折中是：float64 计算，出口立刻转成 decimal，不让浮点值流进领域其他部分。
func WilsonInterval(hits, n int) ConfidenceInterval {
	if n <= 0 {
		// 没有样本时不返回 [0,1]：那看起来像「区间很宽」，
		// 而真相是「根本没有区间」。零值配合 n=0 一起展示，读者不会误解。
		return ConfidenceInterval{Level: confidenceLevel95}
	}
	if hits < 0 {
		hits = 0
	}
	if hits > n {
		hits = n
	}

	nf := float64(n)
	p := float64(hits) / nf
	z2 := z95 * z95

	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	margin := (z95 / denom) * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))

	return ConfidenceInterval{
		Lower: clampUnit(center - margin),
		Upper: clampUnit(center + margin),
		Level: confidenceLevel95,
	}
}

// clampUnit 把结果夹到 [0,1] 并定标到四位小数。
//
// 夹取是防御性的：Wilson 区间在数学上不会越界，但浮点运算的末位误差
// 可以让 p=1 时的上界算出 1.0000000000000002，
// 而那个值一旦进了报告就是「一致率上限 100.00000000000002%」。
func clampUnit(v float64) decimal.Decimal {
	switch {
	case math.IsNaN(v), v < 0:
		v = 0
	case v > 1:
		v = 1
	}
	return decimal.NewFromFloat(v).Round(4)
}
