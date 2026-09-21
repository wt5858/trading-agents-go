package value_objects

import (
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// MaxCriteria 是一次筛选允许携带的条件数上限。
	//
	// 它同时是模板的条件数上限（entities.MaxCriteriaPerTemplate 转引本常量）：
	// 临时筛选和保存成模板的筛选是同一件事的两种保存方式，两边给不同的上限，
	// 用户会遇到「能筛出来却存不下」这种无从解释的状况。
	//
	// 20 这个数是从执行代价推出来的，不是拍的：条件跨三个存储时，
	// 每个存储一条查询，条件再多也只是让单条语句的谓词变长，代价可控；
	// 真正不可控的是用户拿筛选器当批量查询用（几百个条件）。
	// 20 个条件已经能表达任何一个真实的选股策略。
	MaxCriteria = 20

	// DefaultResultLimit 是未指定条数时返回的股票数。
	DefaultResultLimit = 50
	// MaxResultLimit 是硬上限。筛选结果直连前端表格，必须有个天花板兜住 OOM；
	// 真要拿全市场数据的场景应该走导出，不是筛选接口。
	MaxResultLimit = 500
)

// ScreenQuery 是一次筛选执行的完整入参：条件 + 排序 + 条数。
//
// # 它为什么是一个值对象而不是三个参数
//
// 三者之间存在约束（条件不能为空、排序字段可能不在条件里、条数要收敛到区间内），
// 散成三个参数意味着每个调用点都要各自记得校验一遍，而调用点有两个
// （临时筛选与执行模板）。做成 VO 之后「构造出来的就是可执行的」，
// 查询规划器因此可以不做任何入参防御。
type ScreenQuery struct {
	criteria []Criterion
	sort     SortSpec
	limit    int
}

// NewScreenQuery 构造并收敛筛选入参。
//
// 空条件直接报错而不是「返回全市场」：一次没有任何条件的筛选会扫出整个股票池，
// 那既不是用户的意图（他一定是漏填了什么），也是这个接口最容易被误用成
// 全表导出的路径。
func NewScreenQuery(criteria []Criterion, sort SortSpec, limit int) (ScreenQuery, error) {
	if len(criteria) == 0 {
		return ScreenQuery{}, custom_errors.Invalid("至少需要一个筛选条件")
	}
	if len(criteria) > MaxCriteria {
		return ScreenQuery{}, custom_errors.Invalid(
			"筛选条件最多 %d 条，当前 %d 条", MaxCriteria, len(criteria))
	}

	seen := make(map[string]struct{}, len(criteria))
	cleaned := make([]Criterion, 0, len(criteria))
	for _, c := range criteria {
		if c.IsZero() {
			// 零值条件只可能来自一条坏的持久化记录（见 RehydrateCriterion）。
			// 静默丢弃会让筛选结果凭空变大且毫无提示，所以必须显式失败。
			return ScreenQuery{}, custom_errors.Invalid("筛选条件中存在无法识别的字段，请检查模板")
		}
		// 同一个 (字段, 比较符) 出现两次时后者恒覆盖前者，前者是纯噪音。
		// 这条判重与聚合根里的那条是同一条规则，只是这里还要覆盖临时筛选
		// （它根本不经过聚合）。
		if _, dup := seen[c.Key()]; dup {
			return ScreenQuery{}, custom_errors.Invalid("筛选条件重复: %s", c.Describe())
		}
		seen[c.Key()] = struct{}{}
		cleaned = append(cleaned, c)
	}

	return ScreenQuery{
		criteria: cleaned,
		sort:     sort.OrDefault(),
		limit:    ClampLimit(limit),
	}, nil
}

// ClampLimit 把条数收敛到 [1, MaxResultLimit]。
// 导出是因为模板实体在设置 Limit 时用的是同一套边界，
// 而同一个边界在两处各写一遍迟早会分叉。
func ClampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultResultLimit
	case limit > MaxResultLimit:
		return MaxResultLimit
	default:
		return limit
	}
}

// Criteria 返回条件的拷贝，理由与 Criterion.Values 相同：不交出可写引用。
func (q ScreenQuery) Criteria() []Criterion {
	out := make([]Criterion, len(q.criteria))
	copy(out, q.criteria)
	return out
}

func (q ScreenQuery) Sort() SortSpec { return q.sort }
func (q ScreenQuery) Limit() int     { return q.limit }
func (q ScreenQuery) IsZero() bool   { return len(q.criteria) == 0 }

// CriteriaBySource 按存储把条件分组，是跨存储查询规划的第一步。
//
// 分组放在值对象里而不是仓储里：这是一次纯粹的、不碰任何 IO 的数据整理，
// 放在这里可以被直接单测，也让仓储那边的规划逻辑只剩下「怎么查」这一件事。
func (q ScreenQuery) CriteriaBySource() map[FieldSource][]Criterion {
	out := make(map[FieldSource][]Criterion, 3)
	for _, c := range q.criteria {
		src := c.Source()
		out[src] = append(out[src], c)
	}
	return out
}

// OutputFields 是结果集里要带回的字段：用户筛选所依据的字段，
// 再加上排序字段（排序字段常常不在条件里——「PE 小于 20 的票按市值排」）。
//
// 去重并保持顺序：条件顺序就是前端表格的列顺序，见 ScreeningResult 的注释。
func (q ScreenQuery) OutputFields() []FieldName {
	out := make([]FieldName, 0, len(q.criteria)+1)
	seen := make(map[string]struct{}, len(q.criteria)+1)
	add := func(f FieldName) {
		if f.IsZero() {
			return
		}
		if _, dup := seen[f.String()]; dup {
			return
		}
		seen[f.String()] = struct{}{}
		out = append(out, f)
	}
	for _, c := range q.criteria {
		add(c.Field())
	}
	add(q.sort.Field())
	return out
}
