package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// CompareOperator 是筛选条件的比较符。
//
// # 它为什么可以是 string 类型，而 FieldName 不行
//
// 两者都会影响最终的查询语句，但影响方式不同：FieldName 决定**标识符**，
// 而标识符在 SQL 里不能参数化，只能拼接，所以它必须是伪造不出来的封闭类型。
// CompareOperator 决定的是「走哪条 switch 分支」——分支里的 `>` `BETWEEN`
// 是写死在代码里的常量，操作符的值本身永远不会进入语句文本。
// 伪造一个 CompareOperator("evil") 的后果只是所有 switch 都不命中、
// 查询构造器返回「不支持的比较符」，没有任何可利用的面。
//
// 于是这里用最直白的 string 常量枚举，换取 switch 的可读性。
type CompareOperator string

const (
	OpGT      CompareOperator = "gt"      // >
	OpGTE     CompareOperator = "gte"     // >=
	OpLT      CompareOperator = "lt"      // <
	OpLTE     CompareOperator = "lte"     // <=
	OpEQ      CompareOperator = "eq"      // =
	OpNE      CompareOperator = "ne"      // <>
	OpBetween CompareOperator = "between" // BETWEEN a AND b，闭区间
	OpIn      CompareOperator = "in"      // IN (...)
	OpNotIn   CompareOperator = "not_in"  // NOT IN (...)
)

// allOperators 的顺序即接口返回给前端的顺序：先序关系、再相等、最后集合，
// 与筛选表单下拉框的习惯排列一致。
var allOperators = []CompareOperator{
	OpGT, OpGTE, OpLT, OpLTE, OpEQ, OpNE, OpBetween, OpIn, OpNotIn,
}

// operatorLabels 是中文显示名，与 FieldSpec.Label 同样是为了让前端不必维护字典。
var operatorLabels = map[CompareOperator]string{
	OpGT:      "大于",
	OpGTE:     "大于等于",
	OpLT:      "小于",
	OpLTE:     "小于等于",
	OpEQ:      "等于",
	OpNE:      "不等于",
	OpBetween: "介于",
	OpIn:      "属于",
	OpNotIn:   "不属于",
}

// NewCompareOperator 解析并校验比较符。
func NewCompareOperator(s string) (CompareOperator, error) {
	op := CompareOperator(strings.ToLower(strings.TrimSpace(s)))
	if !op.Valid() {
		return "", custom_errors.Invalid("不支持的比较符: %s", s)
	}
	return op, nil
}

func (o CompareOperator) Valid() bool {
	switch o {
	case OpGT, OpGTE, OpLT, OpLTE, OpEQ, OpNE, OpBetween, OpIn, OpNotIn:
		return true
	}
	return false
}

func (o CompareOperator) String() string { return string(o) }

func (o CompareOperator) Label() string {
	if l, ok := operatorLabels[o]; ok {
		return l
	}
	return string(o)
}

// IsOrdering 判定是否为序关系比较符。
//
// 「有没有全序」是本上下文里区分两类字段的唯一判据，所以这个判定被提出来
// 单独命名，而不是在 CompatibleWith 里写一串 case——它在 Arity 与
// 兼容性判定两处都要用，写两遍就会出现改一处忘一处的那天。
func (o CompareOperator) IsOrdering() bool {
	switch o {
	case OpGT, OpGTE, OpLT, OpLTE, OpBetween:
		return true
	}
	return false
}

// IsSet 判定是否为集合运算比较符（值的个数可变）。
func (o CompareOperator) IsSet() bool { return o == OpIn || o == OpNotIn }

// CompatibleWith 判定该比较符能否作用于某类字段。
//
// 规则只有一条，而且是从数据类型本身推出来的，不是拍脑袋列的表：
// **序关系比较符要求字段有全序，因此只适用于数值字段。**
//
// 「市盈率介于 10 和 20 之间」成立，「行业介于『银行』和『白酒』之间」不成立——
// 后者依赖的是中文串的字典序，那是一个和业务毫无关系的顺序，
// 它会安静地返回一批莫名其妙的结果，而不是报错。这正是必须在构造点拦掉的那种
// 「语法合法、语义荒谬」的输入。
//
// 反过来相等与集合运算对两类字段都成立：「市盈率等于 0」（未盈利标的的常见标记）
// 和「行业属于（银行, 保险）」都是真实需求。
func (o CompareOperator) CompatibleWith(kind FieldKind) error {
	if !o.Valid() {
		return custom_errors.Invalid("不支持的比较符: %s", o)
	}
	if o.IsOrdering() && kind != FieldKindNumeric {
		return custom_errors.Invalid(
			"比较符「%s」只能用于数值字段，不能用于%s字段", o.Label(), kindLabel(kind))
	}
	return nil
}

// Arity 返回该比较符要求的值个数。
//
//	min == max        个数固定（between 恰好 2，其余恰好 1）
//	max == 0          个数不设上限下界只要求至少 min 个（in / not_in）
//
// 元数校验必须发生在构造点：一个 between 只带 1 个值的条件，
// 到了查询构造器那里要么 panic（切片越界），要么被悄悄降级成 >=，
// 两种都比「构造时就拒绝」糟糕得多。
func (o CompareOperator) Arity() (min, max int) {
	switch o {
	case OpBetween:
		return 2, 2
	case OpIn, OpNotIn:
		return 1, 0
	default:
		return 1, 1
	}
}

// AllOperators 返回全部比较符，供前端渲染下拉框。
func AllOperators() []CompareOperator {
	out := make([]CompareOperator, len(allOperators))
	copy(out, allOperators)
	return out
}

// OperatorsFor 返回某类字段可用的比较符，是前端「选完字段之后该给哪些比较符」
// 这个交互的数据来源。它与 CompatibleWith 共用同一条规则，不存在第二份定义。
func OperatorsFor(kind FieldKind) []CompareOperator {
	out := make([]CompareOperator, 0, len(allOperators))
	for _, op := range allOperators {
		if op.CompatibleWith(kind) == nil {
			out = append(out, op)
		}
	}
	return out
}

func kindLabel(kind FieldKind) string {
	if kind == FieldKindNumeric {
		return "数值"
	}
	return "类别"
}
