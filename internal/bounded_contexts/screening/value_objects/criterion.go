package value_objects

import (
	"strings"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MaxCriterionValues 是单个条件允许携带的值个数上限，只对 in / not_in 有意义。
//
// 它防的不是内存，是查询计划：一个带上千个元素的 IN 列表会让 MySQL 放弃索引
// 转而全表扫，在 Mongo 那边则会把 $in 退化成逐个查找。真要按上千个代码筛选，
// 那不是筛选而是「批量查询」，应该走 stock 上下文的 FindByCodes。
const MaxCriterionValues = 200

// Criterion 是一条筛选条件：字段 + 比较符 + 值。**不可变值对象**。
//
// # 为什么三个字段都不导出
//
// 这三者之间存在跨字段的不变式（比较符要与字段类型相容、值的个数要与比较符的
// 元数相符），导出字段等于允许包外造出一个 {field: industry, op: between}
// 这样通过不了任何校验、却能被查询构造器消费的组合。只留 NewCriterion 一条入口，
// 「构造出来的就一定是合法的」才成立，下游（entities、查询构造器）
// 才有资格不再重复判定。
//
// # 与 entities.Criterion 同名不是笔误
//
// 它们描述同一个概念的两个侧面：本类型是那条**规则**本身（不可变、可比较、
// 可以脱离任何模板独立存在，临时筛选就只用它）；entities.Criterion 是那条规则
// 在某个模板里的**那一条记录**（有主键、有排序位置、有生命周期）。
// 实体持有本类型作为自己的 Spec 字段，规则的校验只写在这里一处。
type Criterion struct {
	field  FieldName
	op     CompareOperator
	values []string
	// numbers 是 values 在数值字段下的解析结果，与 values 一一对应。
	// 预先解析而不是在查询构造器里现解析：现解析意味着一个「pe > 抱歉」
	// 这样的条件要等到打到数据库那一刻才报错，而那时错误信息里已经没有
	// 足够的上下文说清是哪一条条件坏了。类别字段下这个切片为 nil。
	numbers []decimal.Decimal
}

// NewCriterion 是筛选条件的唯一构造入口，四道校验按依赖顺序排列。
//
// 顺序是刻意的：字段先于比较符（要先知道字段类型才能判相容性），
// 相容性先于元数（一个类别字段配 between 本身就是错的，
// 再去数它带了几个值只会给出一句不着边际的提示），元数先于值解析
// （个数都不对，解析哪一个都没意义）。
func NewCriterion(rawField, rawOp string, rawValues []string) (Criterion, error) {
	field, err := NewFieldName(rawField)
	if err != nil {
		return Criterion{}, err
	}
	op, err := NewCompareOperator(rawOp)
	if err != nil {
		return Criterion{}, err
	}
	return newCriterionFrom(field, op, rawValues)
}

// NewCriterionOf 是已经持有值对象时的构造入口，语义与 NewCriterion 完全一致。
// 它存在是为了让「改一条已有条件的值」不必把字段名降级回字符串再解析一遍。
func NewCriterionOf(field FieldName, op CompareOperator, rawValues []string) (Criterion, error) {
	if field.IsZero() {
		return Criterion{}, custom_errors.Invalid("筛选字段不能为空")
	}
	if _, ok := field.Spec(); !ok {
		// 防的是「用零值 FieldName 之外的方式绕过白名单」。包外造不出这种值，
		// 但 RehydrateFieldName 的失败路径会产生零值，这里是它的兜底。
		return Criterion{}, custom_errors.Invalid("不支持的筛选字段: %s", field.String())
	}
	return newCriterionFrom(field, op, rawValues)
}

func newCriterionFrom(field FieldName, op CompareOperator, rawValues []string) (Criterion, error) {
	if err := op.CompatibleWith(field.Kind()); err != nil {
		// 把字段名补进消息里：光说「between 不能用于类别字段」，
		// 用户面对一个十条件的模板还是不知道该改哪一条。
		return Criterion{}, custom_errors.Invalid(
			"筛选条件「%s」不合法：%s", field.Label(), custom_errors.MessageOf(err))
	}

	values, err := normalizeValues(field, op, rawValues)
	if err != nil {
		return Criterion{}, err
	}

	c := Criterion{field: field, op: op, values: values}
	if field.IsNumeric() {
		nums := make([]decimal.Decimal, 0, len(values))
		for _, v := range values {
			n, err := decimal.NewFromString(strings.TrimSpace(v))
			if err != nil {
				return Criterion{}, custom_errors.Invalid(
					"字段「%s」要求数值，无法解析: %s", field.Label(), v)
			}
			nums = append(nums, n)
		}
		// between 的两端必须有序。不排序也不报错的话，BETWEEN 20 AND 10
		// 在 SQL 里恒为假——用户会得到一份空清单而没有任何解释。
		// 这里选择报错而不是静默交换：交换等于替用户猜他的意图，
		// 而「上下限写反了」多半意味着他把两个输入框填错了位置。
		if op == OpBetween && nums[0].GreaterThan(nums[1]) {
			return Criterion{}, custom_errors.Invalid(
				"字段「%s」的区间下限不能大于上限: %s ~ %s", field.Label(), values[0], values[1])
		}
		c.numbers = nums
	}
	return c, nil
}

// normalizeValues 做去空白、去空值与元数校验。
//
// 去空值放在计数之前：前端的多值输入框常常会尾随一个空项，
// 把它算进个数会让一个「in 一个行业」的条件被当成「in 两个」通过校验，
// 然后在查询里多出一个空串的比较项。
func normalizeValues(field FieldName, op CompareOperator, raw []string) ([]string, error) {
	values := make([]string, 0, len(raw))
	for _, v := range raw {
		if t := strings.TrimSpace(v); t != "" {
			values = append(values, t)
		}
	}

	min, max := op.Arity()
	switch {
	case len(values) < min:
		return nil, custom_errors.Invalid(
			"比较符「%s」至少需要 %d 个值，实际 %d 个", op.Label(), min, len(values))
	case max > 0 && len(values) > max:
		return nil, custom_errors.Invalid(
			"比较符「%s」只接受 %d 个值，实际 %d 个", op.Label(), max, len(values))
	case len(values) > MaxCriterionValues:
		return nil, custom_errors.Invalid(
			"字段「%s」的取值个数不能超过 %d 个，实际 %d 个",
			field.Label(), MaxCriterionValues, len(values))
	}
	return values, nil
}

// RehydrateCriterion 从持久化数据重建条件，**不跳过校验**。
//
// 理由与 RehydrateFieldName 相同：这个值会变成一条查询，库里的数据在安全边界上
// 不算可信输入。重建失败时返回零值条件，由聚合的 Validate 与查询构造器
// 分别拦下——模板仍然列得出来，只是执行时会明确报错。
func RehydrateCriterion(rawField, rawOp string, rawValues []string) Criterion {
	c, err := NewCriterion(rawField, rawOp, rawValues)
	if err != nil {
		return Criterion{}
	}
	return c
}

func (c Criterion) Field() FieldName          { return c.field }
func (c Criterion) Operator() CompareOperator { return c.op }
func (c Criterion) Source() FieldSource       { return c.field.Source() }

func (c Criterion) IsZero() bool { return c.field.IsZero() || c.op == "" }

// Values 返回值的拷贝。返回拷贝而不是内部切片：值对象的不可变性在 Go 里
// 只能靠「不交出可写引用」来实现，直接返回 c.values 的话调用方一次
// values[0] = "x" 就改掉了一个本该不可变的对象。
func (c Criterion) Values() []string {
	out := make([]string, len(c.values))
	copy(out, c.values)
	return out
}

// Numbers 返回数值解析结果的拷贝；类别字段返回空切片。
func (c Criterion) Numbers() []decimal.Decimal {
	out := make([]decimal.Decimal, len(c.numbers))
	copy(out, c.numbers)
	return out
}

// Args 返回要绑定到查询占位符上的值，数值字段给 decimal.Decimal，类别字段给 string。
//
// # 它为什么必须在这里，而不是让查询构造器自己判断类型
//
// 类型选错不会报错，只会安静地改变查询语义：把 20 当成字符串 "20" 绑进去，
// MySQL 会做隐式转换从而**放弃索引**，Mongo 那边则直接匹配不上任何文档
// （BSON 里 20 与 "20" 是两个不同的值），用户得到一份空清单而毫无提示。
// 类型归属字段定义，所以这个判断只该有一处，就是这里。
//
// # 数值为什么必须是 decimal 而不是 float64
//
// 这不是风格问题，是筛选结果对不对的问题。行情列现在以 BSON Decimal128 落库，
// 而用 float64 绑定的 $gte 会以 BSON double 进入查询。Mongo 跨类型比较时
// 用的是 double 的**精确二进制值**：查询写 0.1，实际比的是
// 0.1000000000000000055511151231257827，于是库里那条正好等于 0.1 的记录
// 会被 $gte 判为小于而漏掉。用 decimal 绑定则两侧都是十进制，边界值不会丢。
// MySQL 侧同理：decimal.Decimal 实现了 driver.Valuer，
// 绑进去与 DECIMAL 列是同源比较，不会触发隐式类型转换而放弃索引。
//
// 注意返回的永远是**值**，永远不是标识符。标识符（列名）来自
// FieldName.Column() 那张写死的注册表，与用户输入无关——
// 这条分界就是本上下文防注入的全部要义：值走占位符，标识符走白名单。
func (c Criterion) Args() []any {
	if c.field.IsNumeric() {
		out := make([]any, len(c.numbers))
		for i, n := range c.numbers {
			out[i] = n
		}
		return out
	}
	out := make([]any, len(c.values))
	for i, v := range c.values {
		out[i] = v
	}
	return out
}

// Key 是条件在模板内的判重键：(字段, 比较符)。
//
// 值不进键是刻意的：同一字段上「pe > 10」和「pe > 20」是同一条规则的两个版本，
// 而不是两条可以并存的规则——并存时后者恒覆盖前者，前者纯属噪音。
// 而「pe > 10」与「pe < 30」比较符不同，是一个合法的区间表达，必须允许并存。
func (c Criterion) Key() string { return c.field.String() + ":" + c.op.String() }

// Equal 比较两条条件是否完全一致，供模板的「条件有没有真的变过」判定使用。
func (c Criterion) Equal(o Criterion) bool {
	if !c.field.Equal(o.field) || c.op != o.op || len(c.values) != len(o.values) {
		return false
	}
	for i := range c.values {
		if c.values[i] != o.values[i] {
			return false
		}
	}
	return true
}

// Describe 渲染成一句人话，用于错误提示与模板列表的摘要展示。
func (c Criterion) Describe() string {
	if c.IsZero() {
		return "(无效条件)"
	}
	return c.field.Label() + " " + c.op.Label() + " " + strings.Join(c.values, ", ")
}
