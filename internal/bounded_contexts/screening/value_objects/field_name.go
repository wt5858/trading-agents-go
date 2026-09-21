// Package value_objects 提供选股筛选上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不出现 gorm / bson 标签：持久化格式的演进属于 repositories/，由 DTO 承载。
// 本包也不引用 entities：值对象是实体的构件，反向依赖会绕成一个环。
//
// # 本包为什么是本上下文的安全边界
//
// 选股筛选的输入形状是「字段 + 比较符 + 值」，其中**字段名最终会变成一条
// SQL 的列名或一个 Mongo 文档的键**。如果字段名是一个开放的 string，
// 那么 `internal/...` 里就存在一条从 HTTP 请求体直达查询语句的路径——
// 那是注入。占位符能挡住「值」，挡不住「标识符」：SQL 不允许把列名参数化，
// 拼接是唯一的写法，所以唯一的防线是**让非法的列名根本构造不出来**。
//
// FieldName 因此被做成一个封闭枚举：包外无法用类型转换伪造
// （底层字段不导出），唯一的构造入口 NewFieldName 只认白名单里的键，
// 而真正进入语句的物理列名来自白名单表里写死的常量，不是用户给的那个串。
package value_objects

import (
	"sort"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// FieldKind 是筛选字段的数据类型，决定它能接受哪些比较符。
//
// 只分两类（数值 / 类别）而不是照搬 SQL 类型系统：筛选器真正关心的只有
// 「这个字段有没有全序」——有全序才谈得上 > < between，没有就只能谈相等与集合。
type FieldKind string

const (
	// FieldKindNumeric 是有序数值字段：市盈率、市值、涨跌幅……
	FieldKindNumeric FieldKind = "numeric"
	// FieldKindCategorical 是无序类别字段：行业、市场、地区。
	// 对它做 between 在业务上没有意义（「行业介于『银行』和『白酒』之间」不是一句话），
	// 这条判定落在 CompareOperator.CompatibleWith 里。
	FieldKindCategorical FieldKind = "categorical"
)

func (k FieldKind) String() string { return string(k) }

// FieldSource 标记一个字段的数据落在哪个存储里。
//
// 它不是元数据装饰，而是查询规划的输入：跨存储的筛选条件无法用一条语句完成，
// 必须先在各自的存储里筛出候选代码再做交集（见 repositories/stock_screener.go）。
// 把来源钉在字段定义上，规划器就不必靠字段名去猜。
type FieldSource string

const (
	// FieldSourceStock 是 MySQL 的 stocks 表：股票主数据（行业、市场、市值）。
	FieldSourceStock FieldSource = "stock"
	// FieldSourceQuote 是 MongoDB 的 quotes 集合：最新交易日的行情横截面。
	FieldSourceQuote FieldSource = "quote"
	// FieldSourceFinancial 是 MongoDB 的 financials 集合：最近一期财报。
	FieldSourceFinancial FieldSource = "financial"
)

func (s FieldSource) String() string { return string(s) }

// FieldName 是筛选字段名，一个**封闭枚举**。
//
// 它是 struct 而不是 `type FieldName string`，这个选择是刻意的：
// 后者允许包外写出 FieldName("pe = 1 OR 1=1 -- ")，一次类型转换就绕开了全部校验，
// 而这个值随后会被当成列名拼进 SQL。struct + 不导出字段让那行代码压根编译不过——
// 包外得到一个 FieldName 的**唯一**办法是调用 NewFieldName，而它只认白名单。
//
// 对比本项目其它 VO（GroupName、ItemNote）：那些用 struct 是为了收敛规范化规则，
// 伪造一个非法值的后果只是一条难看的数据。这里的后果是任意 SQL 执行，
// 所以同一个手法在这里是安全机制，不是风格。
type FieldName struct{ v string }

// FieldSpec 是一个可筛选字段的完整定义，也是前端构建筛选表单所需的全部信息。
//
// Column 是**写死在本文件里的物理列名 / BSON 键**，与用户输入无关。
// 查询构造器只读这一列，绝不会把 FieldName 的字符串直接拼进语句——
// 这是「封闭枚举挡住注入」的后半场：前半场保证枚举值合法，
// 后半场保证进入语句的是我们自己写下的常量。
type FieldSpec struct {
	Name   FieldName
	Kind   FieldKind
	Source FieldSource
	// Column 是该字段在其存储里的物理标识：MySQL 列名或 Mongo 文档键。
	// 它与 Name 常常同名，但那是巧合不是约定——两者一旦需要分道扬镳
	// （比如库里改列名），改这一处即可，对外的字段名保持稳定。
	Column string
	// Label 是中文显示名，供筛选表单直接渲染，免得前端再维护一份字典。
	Label string
	// Unit 是量纲提示（%、万元），前端用它渲染输入框后缀。
	// 没有量纲的字段留空。
	Unit string
}

// 字段名常量。它们是对外契约的一部分（前端按这些串构造请求），
// 所以集中声明在一处，改名即是接口变更。
const (
	FieldPE          = "pe"
	FieldPB          = "pb"
	FieldROE         = "roe"
	FieldNetMargin   = "net_margin"
	FieldGrossMargin = "gross_margin"
	FieldDebtRatio   = "debt_ratio"
	FieldEPS         = "eps"
	FieldTotalMV     = "total_mv"
	FieldCircMV      = "circ_mv"
	FieldClose       = "close"
	FieldOpen        = "open"
	FieldHigh        = "high"
	FieldLow         = "low"
	FieldChangePct   = "change_pct"
	FieldTurnover    = "turnover"
	FieldVolume      = "volume"
	FieldAmount      = "amount"
	FieldIndustry    = "industry"
	FieldMarket      = "market"
	FieldArea        = "area"
)

// fieldRegistry 是白名单本身，也是本上下文唯一的字段真相来源。
//
// 加字段只能改这里。这个「只有一处能改」正是安全性的来源：审计这段代码
// 就等于审计了全部可能出现在查询语句里的标识符。
//
// 为什么用 map 而不是 switch：注册表还要被 AllFieldSpecs 遍历出去喂给前端表单，
// switch 给不出「全部字段」这个列表，迟早会在别处出现第二份手抄的清单。
var fieldRegistry = map[string]FieldSpec{
	// --- 估值与财务：quotes 集合直接带了 pe/pb，取它比取财报快一步 ---
	FieldPE:  {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "pe", Label: "市盈率"},
	FieldPB:  {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "pb", Label: "市净率"},
	FieldEPS: {Kind: FieldKindNumeric, Source: FieldSourceFinancial, Column: "eps", Label: "每股收益"},
	// ROE / 净利率 / 毛利率 / 资产负债率都是**数据源直接给出、随财报一起落库**的派生量。
	// 这里读的是存量列，绝不在筛选时用「净利润 ÷ 营收」现算：
	// 现算的口径和源给的对不上，而 Tushare 的 fina_indicator 连稳定的绝对值字段都没有，
	// 现算只会得到一列 0。口径见 stock/value_objects/financial.go 的长注释。
	FieldROE:         {Kind: FieldKindNumeric, Source: FieldSourceFinancial, Column: "roe", Label: "净资产收益率", Unit: "%"},
	FieldNetMargin:   {Kind: FieldKindNumeric, Source: FieldSourceFinancial, Column: "net_margin", Label: "销售净利率", Unit: "%"},
	FieldGrossMargin: {Kind: FieldKindNumeric, Source: FieldSourceFinancial, Column: "gross_margin", Label: "销售毛利率", Unit: "%"},
	FieldDebtRatio:   {Kind: FieldKindNumeric, Source: FieldSourceFinancial, Column: "debt_ratio", Label: "资产负债率", Unit: "%"},

	// --- 市值：落在 stocks 主表，同样是数据源给出的存量值，不用股价乘股本反推 ---
	FieldTotalMV: {Kind: FieldKindNumeric, Source: FieldSourceStock, Column: "total_mv", Label: "总市值", Unit: "万元"},
	FieldCircMV:  {Kind: FieldKindNumeric, Source: FieldSourceStock, Column: "circ_mv", Label: "流通市值", Unit: "万元"},

	// --- 量价：最新交易日的行情横截面 ---
	FieldClose:     {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "close", Label: "最新价"},
	FieldOpen:      {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "open", Label: "开盘价"},
	FieldHigh:      {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "high", Label: "最高价"},
	FieldLow:       {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "low", Label: "最低价"},
	FieldChangePct: {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "change_pct", Label: "涨跌幅", Unit: "%"},
	FieldTurnover:  {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "turnover", Label: "换手率", Unit: "%"},
	FieldVolume:    {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "volume", Label: "成交量"},
	FieldAmount:    {Kind: FieldKindNumeric, Source: FieldSourceQuote, Column: "amount", Label: "成交额"},

	// --- 类别字段：只支持相等与集合运算 ---
	FieldIndustry: {Kind: FieldKindCategorical, Source: FieldSourceStock, Column: "industry", Label: "所属行业"},
	FieldMarket:   {Kind: FieldKindCategorical, Source: FieldSourceStock, Column: "market", Label: "所属市场"},
	FieldArea:     {Kind: FieldKindCategorical, Source: FieldSourceStock, Column: "area", Label: "所属地区"},
}

// init 把 map 的键回填进每条 spec 的 Name，省掉在字面量里把字段名写两遍。
// 写两遍迟早会出现键与 Name 不一致的那一行，而那一行会让前端拿到一个
// 提交回来就被拒绝的字段。
func init() {
	for key, spec := range fieldRegistry {
		spec.Name = FieldName{v: key}
		fieldRegistry[key] = spec
	}
}

// NewFieldName 是包外获得 FieldName 的**唯一**入口，也是注入防线本身。
//
// 未知字段一律报错，绝不「宽容地忽略」：忽略一个筛选条件会让筛选结果
// 凭空变大（少了一个过滤器），用户看到的是一份不符合他条件的清单却毫无提示。
// 而在安全语境下，「无法识别就放行」本身就是漏洞的定义。
func NewFieldName(s string) (FieldName, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	if key == "" {
		return FieldName{}, custom_errors.Invalid("筛选字段不能为空")
	}
	if _, ok := fieldRegistry[key]; !ok {
		return FieldName{}, custom_errors.Invalid("不支持的筛选字段: %s", s)
	}
	return FieldName{v: key}, nil
}

// RehydrateFieldName 从持久化数据重建字段名。
//
// # 为什么它和本项目其它 Rehydrate 不一样：它**仍然查白名单**
//
// 别处的 Rehydrate（RehydrateGroupName、RehydrateUsername）刻意跳过校验，
// 因为落库的行是既成事实，读路径重新校验会让历史数据把整个列表接口打挂。
// 这里不能照搬，理由只有一条：那些值最终只是被显示出来，而这个值最终会被
// 当成列名拼进查询语句。一旦库里因为任何原因（迁移脚本、误操作、更早的
// 漏洞）存了一个非白名单的串，无校验的重建会把它直接送进 SQL。
// 「库里的数据可信」这个前提在安全边界上是不成立的。
//
// 折中之处在于失败方式：未知字段返回**零值** FieldName 而不是 error。
// 这样一条坏行只会让它所属的模板在执行时报错（查询构造器拒绝零值字段，
// 见 stock_screener.go），而不会让「列出我的模板」整个接口挂掉——
// 用户至少还能看见并删掉那个坏模板。
func RehydrateFieldName(s string) FieldName {
	key := strings.ToLower(strings.TrimSpace(s))
	if _, ok := fieldRegistry[key]; !ok {
		return FieldName{}
	}
	return FieldName{v: key}
}

// String 返回对外的字段名。注意它**不是**给查询构造器用的——
// 构造器要的是 Column()，那才是写死在注册表里的物理标识。
func (f FieldName) String() string { return f.v }

func (f FieldName) IsZero() bool { return f.v == "" }

func (f FieldName) Equal(o FieldName) bool { return f.v == o.v }

// Spec 返回字段定义。零值字段返回零值 spec 与 false。
func (f FieldName) Spec() (FieldSpec, bool) {
	spec, ok := fieldRegistry[f.v]
	return spec, ok
}

// Kind 返回数据类型。零值字段归为类别型：这是保守的选择，
// 类别型能用的比较符是数值型的真子集。
func (f FieldName) Kind() FieldKind {
	if spec, ok := fieldRegistry[f.v]; ok {
		return spec.Kind
	}
	return FieldKindCategorical
}

func (f FieldName) Source() FieldSource {
	if spec, ok := fieldRegistry[f.v]; ok {
		return spec.Source
	}
	return ""
}

// Column 返回进入查询语句的物理标识。
//
// 零值（未知字段）返回空串，调用方**必须**据此拒绝构造查询。
// 返回空串而不是 panic：坏数据不该让一次 HTTP 请求打挂进程。
func (f FieldName) Column() string {
	if spec, ok := fieldRegistry[f.v]; ok {
		return spec.Column
	}
	return ""
}

func (f FieldName) Label() string {
	if spec, ok := fieldRegistry[f.v]; ok {
		return spec.Label
	}
	return f.v
}

func (f FieldName) IsNumeric() bool { return f.Kind() == FieldKindNumeric }

// AllFieldSpecs 返回全部可筛选字段，按字段名排序。
//
// 它服务于「前端自动生成筛选表单」这条路径：前端拿到字段、类型、
// 以及该类型支持的比较符，就能自己把表单画出来，不必维护第二份清单。
// 排序是为了让接口响应稳定——map 遍历顺序随机会让前端的字段列表每次刷新都在跳。
func AllFieldSpecs() []FieldSpec {
	out := make([]FieldSpec, 0, len(fieldRegistry))
	for _, spec := range fieldRegistry {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.v < out[j].Name.v })
	return out
}
