package value_objects

import (
	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// FieldValue 是「某只股票在某个筛选字段上的取值」。
//
// 数值与文本用两个字段加一个标志位承载，而不是 any：
// any 会把类型判断推到每一个渲染点，前端拿到的 JSON 也会在同一个字段上
// 时而是数字时而是字符串。Present 为 false 表示该字段在这只票上没有数据
// （停牌、新股、财报未披露），渲染成 null 而不是 0——
// 0 会被前端画成一个具体的值，而「没有数据」是另一回事。
type FieldValue struct {
	Field   FieldName
	Number  decimal.Decimal
	Text    string
	Present bool
}

// NumberValue / TextValue 是两个构造器，命名刻意带类型，
// 让调用点一眼看得出这一列是数值还是文本。
func NumberValue(field FieldName, n decimal.Decimal) FieldValue {
	return FieldValue{Field: field, Number: n, Present: true}
}

func TextValue(field FieldName, s string) FieldValue {
	return FieldValue{Field: field, Text: s, Present: true}
}

// MissingValue 表示该字段在这只票上无数据。
func MissingValue(field FieldName) FieldValue {
	return FieldValue{Field: field, Present: false}
}

// IsNumeric 直接问字段自己，不额外存一份类型标记：
// 存两份迟早会不一致，而字段类型的唯一真相在 fieldRegistry 里。
func (v FieldValue) IsNumeric() bool { return v.Field.IsNumeric() }

// ScreeningResult 是筛选命中的一只股票，读路径值对象。
//
// # 它为什么不是 stock.Stock
//
// 跨上下文只传标识与数据，不传实体。持有 stock.Stock 会让选股上下文在编译期
// 依赖股票上下文的聚合，也会让一次筛选顺带把股票主数据的全部字段和它的
// 领域方法拖进来——而筛选结果需要的只有「代码、名称，以及用户筛选所依据的那几个数」。
//
// # Fields 为什么是切片而不是 map
//
// 顺序有意义：它就是前端表格的列顺序，而列顺序必须与用户填写筛选条件的顺序一致，
// 否则用户会在一张列名乱跳的表里找自己刚填的那个指标。map 给不出稳定顺序。
type ScreeningResult struct {
	Code shared_vo.StockCode
	Name string
	// Fields 依次是：用户筛选所依据的字段，加上排序字段（若不在筛选条件里）。
	// 只带这些而不是把所有可筛选字段都塞进来：一次筛选要为每个字段多访问一个存储，
	// 用户没问的指标不值得为它多一次跨库查询。
	Fields []FieldValue
}

func (r ScreeningResult) Symbol() string { return r.Code.Symbol }

func (r ScreeningResult) Market() shared_vo.Market { return r.Code.Market }

// Value 按字段取值。命中的字段个数是个位数，线性扫比维护一个 map 更划算，
// 也让这个值对象保持「纯数据」的形状。
func (r ScreeningResult) Value(field FieldName) (FieldValue, bool) {
	for _, v := range r.Fields {
		if v.Field.Equal(field) {
			return v, true
		}
	}
	return FieldValue{}, false
}

// ScreeningResultSet 是一次筛选的完整结果。
//
// Total 与 len(Results) 是两个不同的数：前者是符合条件的股票总数，
// 后者是被 limit 截到的那一页。前端要靠 Total 才能说出「共命中 312 只，展示前 50 只」，
// 只给一页数据会让用户以为全市场只有 50 只票符合他的条件。
type ScreeningResultSet struct {
	Results []ScreeningResult
	Total   int64
	// AsOf 是本次筛选所依据的行情交易日。
	//
	// 必须返回给用户：筛选是对某一天横截面的查询，盘后与次日早盘执行同一个模板
	// 结果不同是正常的，但用户看不到日期就只会觉得系统不稳定。
	// 没有任何行情条件参与时为零值。
	AsOf shared_vo.TradeDate
	// Truncated 表示候选集在跨存储求交时触到了上限，结果是全集的一个子集。
	//
	// 如实上报而不是悄悄返回一份不完整的清单：选股结果的价值完全建立在
	// 「符合条件的都在这里」这个承诺上，破坏了它却不说，比返回错误更糟。
	Truncated bool
}

// EmptyResultSet 是「没有任何股票命中」的规范返回。
//
// Results 用非 nil 空切片：让「没命中」和「还没查」在 JSON 里都是 []，
// 前端不必区分 null 与 []。
func EmptyResultSet() ScreeningResultSet {
	return ScreeningResultSet{Results: []ScreeningResult{}}
}
