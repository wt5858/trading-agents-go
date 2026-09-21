// Package decimalx 是全仓库十进制数值的唯一工具箱。
//
// # 为什么整个仓库不再出现 float64
//
// float64 表示金额与价格是缺陷而不是风格问题：0.1 + 0.2 != 0.3，二进制浮点
// 根本表示不了十进制小数。这条缺陷在交易系统里会以两种方式浮现——
// 账本对不平（买入扣的现金与卖出收的现金之和不等于「初始资金 + 已实现盈亏」），
// 以及同一个数在不同路径上算出不同结果（内存里 12.345，落库 decimal(20,4) 后
// 读回来 12.3449999）。两种都无法靠「再取一次整」收敛，因为误差是累积的。
//
// 技术指标同样不豁免：均线、MACD、布林带都是长序列的连加连乘，
// 浮点误差会随窗口长度单调累积，而指标的输出会直接进入 LLM 的提示词与
// 选股的 $gte 比较——一个在边界上的 PE 因为末位误差被筛掉或漏进来，
// 排查起来没有任何线索。
//
// # 精度口径必须与列定义一致
//
// 每一个乘除结果都要过一次对应的 Round*：应用层的取整口径和数据库列精度
// 对不齐时，MySQL 会在写入时按列精度再截一次，落库值与内存值就此悄悄分叉。
package decimalx

import (
	"math/big"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 各类数值的落库精度。改这里的同时必须改对应迁移脚本里的列定义，反之亦然。
const (
	// MoneyScale 是金额精度，对应 decimal(20,4)。
	MoneyScale int32 = 4
	// QuantityScale 是数量精度，对应 decimal(20,8)。
	// 8 位是给港股碎股与将来可能的份额类标的留的余量。
	QuantityScale int32 = 8
	// PriceScale 是价格精度，与金额同为 4 位：价格要直接参与「数量 × 价格」，
	// 两者精度不一致的话乘积的取整口径就没有唯一答案。
	PriceScale int32 = 4
	// RatioScale 是比率精度，对应 decimal(5,4)，用于置信度这类 0~1 的值。
	RatioScale int32 = 4
	// PercentScale 是百分比精度，对应 decimal(5,2)，用于涨跌幅、成功率、进度。
	PercentScale int32 = 2
	// IndicatorScale 是技术指标精度。比金额多两位是刻意的：
	// 指标是中间量而非落库金额，多留两位可以让连续迭代（EMA、RSI 的平滑）
	// 的截断误差不至于在末位反复摆动。
	IndicatorScale int32 = 6
)

// Zero 转发 decimal.Zero，省得调用点为了一个零值再 import 一次 decimal。
var Zero = decimal.Zero

// 取整：每个乘除结果都应当过一次，保证内存里的值就是将来库里的值。
func RoundMoney(d decimal.Decimal) decimal.Decimal     { return d.Round(MoneyScale) }
func RoundQuantity(d decimal.Decimal) decimal.Decimal  { return d.Round(QuantityScale) }
func RoundPrice(d decimal.Decimal) decimal.Decimal     { return d.Round(PriceScale) }
func RoundRatio(d decimal.Decimal) decimal.Decimal     { return d.Round(RatioScale) }
func RoundPercent(d decimal.Decimal) decimal.Decimal   { return d.Round(PercentScale) }
func RoundIndicator(d decimal.Decimal) decimal.Decimal { return d.Round(IndicatorScale) }

// 渲染：一律定长小数位而不是 String()。
// String() 会把 100.0000 输出成 100，同一列在不同行呈现出不同的小数位，
// 前端做右对齐和对账都会难受。
func FormatMoney(d decimal.Decimal) string     { return d.StringFixed(MoneyScale) }
func FormatQuantity(d decimal.Decimal) string  { return d.StringFixed(QuantityScale) }
func FormatPrice(d decimal.Decimal) string     { return d.StringFixed(PriceScale) }
func FormatRatio(d decimal.Decimal) string     { return d.StringFixed(RatioScale) }
func FormatPercent(d decimal.Decimal) string   { return d.StringFixed(PercentScale) }
func FormatIndicator(d decimal.Decimal) string { return d.StringFixed(IndicatorScale) }

// FormatPtr 渲染可空数值。nil 渲染成空串而不是 "0"：
// 「没有这个数」和「这个数是零」在行情里是两件事——停牌日没有换手率，
// 与换手率为 0，对下游的判断完全不同。
func FormatPtr(d *decimal.Decimal, scale int32) string {
	if d == nil {
		return ""
	}
	return d.StringFixed(scale)
}

// 解析：入参一律收成 string 而不是数字类型。
//
// 这条约束是整条链路的入口闸门：接口层一旦用 float64 绑定 JSON 数字，
// 精度在进入领域层之前就已经丢了，后面再怎么用 decimal 也补不回来。
func ParseMoney(raw, subject string) (decimal.Decimal, error) {
	return Parse(raw, subject, MoneyScale)
}

func ParseQuantity(raw, subject string) (decimal.Decimal, error) {
	return Parse(raw, subject, QuantityScale)
}

func ParsePrice(raw, subject string) (decimal.Decimal, error) {
	return Parse(raw, subject, PriceScale)
}

func ParseRatio(raw, subject string) (decimal.Decimal, error) {
	return Parse(raw, subject, RatioScale)
}

func ParsePercent(raw, subject string) (decimal.Decimal, error) {
	return Parse(raw, subject, PercentScale)
}

// Parse 解析并立刻收敛到给定精度。
// 多出来的小数位是调用方的输入噪声，让它流进聚合只会让
// 「内存值 != 落库值」这个分叉从入口就开始。
func Parse(raw, subject string, scale int32) (decimal.Decimal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return decimal.Zero, custom_errors.Invalid("%s不能为空", subject)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, custom_errors.Invalid("%s不是合法的数字: %s", subject, raw)
	}
	return d.Round(scale), nil
}

// ParseOptional 解析可空数值：空串返回 nil 而不是零值。
// 用于 PE、PB、换手率这类「这一天确实没有」的字段。
func ParseOptional(raw, subject string, scale int32) (*decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	d, err := Parse(raw, subject, scale)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// MustParse 解析字面量常量，失败即 panic。
// 只用于代码里写死的常量（默认手续费率、阈值），不要用在任何外部输入上。
func MustParse(raw string) decimal.Decimal {
	return decimal.RequireFromString(raw)
}

// FromString 解析并忽略错误，非法输入得到零值。
// 只用在「上游已保证格式、拿不到也只是少一个展示字段」的场合。
func FromString(raw string) decimal.Decimal {
	d, _ := decimal.NewFromString(strings.TrimSpace(raw))
	return d
}

// 符号判断：让调用点读起来是业务语言而不是 Cmp 比较。
func IsPositive(d decimal.Decimal) bool { return d.GreaterThan(decimal.Zero) }
func IsNegative(d decimal.Decimal) bool { return d.LessThan(decimal.Zero) }

// Ptr 取地址。decimal.Decimal 是值类型，可空字段需要指针。
func Ptr(d decimal.Decimal) *decimal.Decimal { return &d }

// Deref 解引用，nil 得到零值。
func Deref(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}

// Sqrt 返回 d 的平方根，保留 scale 位小数。
//
// shopspring/decimal v1.4.0 不提供 Sqrt，而布林带的标准差绕不开它。
// 这里不走牛顿迭代、也不借道 math.Sqrt 取初值，而是直接用 big.Int 的整数
// 平方根：把 d 左移 2*scale 位后取整数平方根，等价于 sqrt(d) 左移 scale 位。
// 这么做的好处是结果是确定的（不依赖迭代次数与收敛判据），
// 且全程不经过任何浮点——否则「用 decimal 存、用 float 算」只是把精度损失
// 从存储挪到了计算，并没有消除。
//
// 负数返回零：调用点是方差开根，方差为负只可能是上游算错，
// 让它变成 NaN 往下游传比返回零更难排查。
func Sqrt(d decimal.Decimal, scale int32) decimal.Decimal {
	if d.Sign() <= 0 {
		return decimal.Zero
	}
	// 左移 2*scale 后截断成整数。截断丢掉的是低于目标精度的部分，
	// 对开根结果的影响不超过末位。
	shifted := d.Shift(2 * scale).BigInt()
	root := new(big.Int).Sqrt(shifted)
	return decimal.NewFromBigInt(root, -scale)
}

// Sum 求和。空切片返回零而不是报错：连加的单位元就是零。
func Sum(xs []decimal.Decimal) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	return decimal.Sum(xs[0], xs[1:]...)
}

// Mean 算术平均。空切片返回零，由调用方自行判断是否有意义。
func Mean(xs []decimal.Decimal, scale int32) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	return Sum(xs).DivRound(decimal.NewFromInt(int64(len(xs))), scale)
}

// PopulationStdDev 总体标准差（除以 N，不是 N-1）。
//
// 用总体而非样本标准差是布林带的行业惯例：窗口内的 N 根 K 线就是全部总体，
// 不是从更大母体里抽的样本。换成 N-1 会让带宽系统性变宽，与各家行情软件对不上。
func PopulationStdDev(xs []decimal.Decimal, mean decimal.Decimal, scale int32) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	acc := decimal.Zero
	for _, v := range xs {
		d := v.Sub(mean)
		acc = acc.Add(d.Mul(d))
	}
	// 先除后开根，且中间量多留几位：方差本身不落库，过早取整会让
	// 开根后的末位失真。
	variance := acc.DivRound(decimal.NewFromInt(int64(len(xs))), scale+4)
	return Sqrt(variance, scale)
}

// Max 返回切片最大值，空切片返回零。
func Max(xs []decimal.Decimal) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	return decimal.Max(xs[0], xs[1:]...)
}

// Min 返回切片最小值，空切片返回零。
func Min(xs []decimal.Decimal) decimal.Decimal {
	if len(xs) == 0 {
		return decimal.Zero
	}
	return decimal.Min(xs[0], xs[1:]...)
}

// PercentChange 计算涨跌幅（百分比）。base 为零时返回零而不是除零panic：
// 新股上市首日没有前收盘价，这是正常分支而不是故障。
func PercentChange(current, base decimal.Decimal) decimal.Decimal {
	if base.IsZero() {
		return decimal.Zero
	}
	return RoundPercent(current.Sub(base).Div(base).Mul(decimal.NewFromInt(100)))
}

// RatioOf 计算 part/total 的比率（0~1）。total 为零时返回零。
func RatioOf(part, total decimal.Decimal) decimal.Decimal {
	if total.IsZero() {
		return decimal.Zero
	}
	return RoundRatio(part.DivRound(total, RatioScale+2))
}
