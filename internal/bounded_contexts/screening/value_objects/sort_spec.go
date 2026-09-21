package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SortDirection 是排序方向。
type SortDirection string

const (
	SortAsc  SortDirection = "asc"
	SortDesc SortDirection = "desc"
)

func (d SortDirection) String() string { return string(d) }

// SortSpec 是筛选结果的排序规则。
//
// # 它为什么复用 FieldName 而不是收一个裸字段名
//
// 排序字段和筛选字段一样会变成语句里的标识符（ORDER BY `col`），
// 注入面完全相同。复用同一个封闭枚举意味着这条路径不需要第二套白名单，
// 也就不会出现「筛选那边补了字段、排序这边忘了补」的不一致。
//
// # 它为什么同时决定查询规划
//
// 排序字段所在的存储决定了哪个存储能吃下 ORDER BY + LIMIT。
// 把排序放在有条件的那个存储上，数据库只需返回 limit 条；放错了，
// 就得把候选全集拉进内存再排——那正是「把 5000 只票加载进内存」的另一种写法。
// 规划逻辑见 repositories/stock_screener.go，这里只负责让字段与方向合法。
type SortSpec struct {
	field FieldName
	dir   SortDirection
}

// NewSortSpec 构造排序规则。
//
// 字段名为空时返回零值 SortSpec 而不是报错：「不指定排序」是合法输入，
// 由执行层回落到默认排序（见 DefaultSortSpec）。
func NewSortSpec(rawField, rawDir string) (SortSpec, error) {
	if strings.TrimSpace(rawField) == "" {
		return SortSpec{}, nil
	}
	field, err := NewFieldName(rawField)
	if err != nil {
		return SortSpec{}, err
	}
	dir := SortDirection(strings.ToLower(strings.TrimSpace(rawDir)))
	switch dir {
	case SortAsc, SortDesc:
	case "":
		// 缺省按降序：选股场景里排序字段几乎总是「越大越好」的因子
		// （市值、ROE、涨幅），默认升序会让用户第一屏看到的是最差的一批。
		dir = SortDesc
	default:
		return SortSpec{}, custom_errors.Invalid("排序方向只能是 asc 或 desc: %s", rawDir)
	}
	return SortSpec{field: field, dir: dir}, nil
}

// RehydrateSortSpec 从持久化数据重建，与 RehydrateFieldName 同样仍查白名单。
func RehydrateSortSpec(rawField, rawDir string) SortSpec {
	s, err := NewSortSpec(rawField, rawDir)
	if err != nil {
		return SortSpec{}
	}
	return s
}

// DefaultSortSpec 是未指定排序时的兜底：总市值从大到小。
//
// 选它而不是「按代码升序」有两层考虑：一是选股结果被 limit 截断，
// 按代码排序意味着用户永远只看得到代码最小的那几十只，那是一份没有信息量的清单；
// 二是 total_mv 落在 MySQL 的 stocks 表上，而那张表在任何一次筛选里都要被访问
// （结果需要股票名称），拿它做排序锚点不会多引入一个存储。
func DefaultSortSpec() SortSpec {
	return SortSpec{field: FieldName{v: FieldTotalMV}, dir: SortDesc}
}

func (s SortSpec) IsZero() bool { return s.field.IsZero() }

// OrDefault 在未指定时回落到默认排序。
func (s SortSpec) OrDefault() SortSpec {
	if s.IsZero() {
		return DefaultSortSpec()
	}
	return s
}

func (s SortSpec) Field() FieldName         { return s.field }
func (s SortSpec) Direction() SortDirection { return s.dir }
func (s SortSpec) Descending() bool         { return s.dir == SortDesc }
func (s SortSpec) Source() FieldSource      { return s.field.Source() }
