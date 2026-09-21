// Package value_objects 提供模拟交易上下文的值对象：构造即校验、不可变、无生命周期。
//
// # 本包最重要的一条约定：金额与数量一律用 shopspring/decimal，永远不用 float64
//
// float64 表示金额是缺陷而不是风格问题：0.1 + 0.2 != 0.3，二进制浮点根本表示不了
// 十进制小数。一个模拟账本一旦对不平——买入扣的现金和卖出收的现金加起来
// 不等于「初始资金 + 已实现盈亏」——它就一文不值，因为使用者再也无法分辨
// 「我的策略亏了 0.03」和「浮点误差累计了 0.03」。decimal 是十进制定点，
// 加减乘除的结果与手算、与数据库里的 decimal 列完全一致。
//
// # 第二条约定：乘除派生量「算一次、落库、读路径只读存量」
//
// 成交金额、手续费、成本基数、已实现盈亏、平均成本都是乘除结果。
// 它们必须在发生的那一刻算好并随记录落库；渲染时绝不用 数量 × 价格 重算。
// 理由很直接：真正从现金里划走的是**当时算出并写进库的那个数**，
// 读路径重算一遍，只要取整口径、字段精度、或者后来改过的公式有任何一点偏差，
// 展示出来的成交额就和账户余额的变化对不上，而这种分叉无处收敛。
//
// 唯一的例外是浮动盈亏：它依赖实时报价，本质上没有「当时的事实」可落库，
// 详见 portfolio.go 的说明。
//
// # 这两条约定现在是全仓库的
//
// 它们最初只约束模拟交易，现在行情、指标、选股、研报走的是同一套：
// 定义统一上提到 internal/helpers/decimalx，本文件只保留转发别名，
// 让本上下文既有的调用点继续读作领域语言（ParseMoney 而不是 decimalx.ParseMoney）。
// 精度常量与取整口径只有 decimalx 一处定义——两处定义迟早会对不齐，
// 而对不齐的那天是在对账时才发现的。
package value_objects

import (
	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	// MoneyScale 是金额精度，与迁移里的 decimal(20,4) 一致。
	MoneyScale = decimalx.MoneyScale
	// QuantityScale 是数量精度，与迁移里的 decimal(20,8) 一致。
	QuantityScale = decimalx.QuantityScale
)

// RoundMoney 把金额收敛到落库精度。
// 每一个乘除结果都要过一次它，保证内存里的值就是将来库里的值。
func RoundMoney(d decimal.Decimal) decimal.Decimal { return decimalx.RoundMoney(d) }

// RoundQuantity 把数量收敛到落库精度。
func RoundQuantity(d decimal.Decimal) decimal.Decimal { return decimalx.RoundQuantity(d) }

// FormatMoney 渲染金额。定长小数位而不是 String()：
// String() 会把 100.0000 输出成 100，同一列在不同行呈现出不同的小数位，
// 前端做右对齐和对账都会难受。
func FormatMoney(d decimal.Decimal) string { return decimalx.FormatMoney(d) }

// FormatQuantity 渲染数量，理由同上。
func FormatQuantity(d decimal.Decimal) string { return decimalx.FormatQuantity(d) }

// ParseMoney 把接口层传来的字符串解析成金额。
//
// 入参刻意收成 string 而不是数字类型：一旦接口层用 float64 绑定 JSON 数字，
// 精度在进入领域层之前就已经丢了，这里再怎么用 decimal 也补不回来。
func ParseMoney(raw, subject string) (decimal.Decimal, error) {
	return decimalx.ParseMoney(raw, subject)
}

// ParseQuantity 把接口层传来的字符串解析成数量。
func ParseQuantity(raw, subject string) (decimal.Decimal, error) {
	return decimalx.ParseQuantity(raw, subject)
}

// IsPositive / IsNegative 只是为了让调用点读起来是业务语言而不是 Cmp 比较。
func IsPositive(d decimal.Decimal) bool { return decimalx.IsPositive(d) }

func IsNegative(d decimal.Decimal) bool { return decimalx.IsNegative(d) }
