package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
)

// Criterion 是模板里的一条筛选条件，**子实体**，不是聚合根。
//
// ===========================================================================
// 本文件遵循 watchlist/entities/watchlist_item.go 立下的「聚合根 ↔ 子实体」范式
// ===========================================================================
//
// # 它为什么是实体而不是值对象
//
// 因为它在模板里有身份：用户把「pe < 20」改成「pe < 15」之后，它还是**那一条**
// 条件——排序位置不变，创建时间不变，前端表单里的那一行也不会跳走。
// 对比 value_objects.Criterion：那个是规则本身，重新填一次就是全新的一份事实，
// 没有「同一条规则变了」这种说法。
//
// 于是本类型 = 身份（ID、TemplateID、SortOrder、时间戳）+ 规则（Spec）。
// 规则的**全部校验都在 VO 里**，本类型一条都不重复判——那是同一条规则的
// 第二个定义点，改一处忘一处只是时间问题。
//
// # 它为什么没有自己的仓储
//
// 因为它不是聚合根。条件的不变式（同一模板内 (字段, 比较符) 不重复、
// 条数上限、至少一条）全都是**跨若干条条件**才成立的规则，
// 只有看得见全部兄弟节点的 ScreeningTemplate 才有资格判定。
// 给子实体配仓储等于给调用方一条绕过根直接写单条记录的路，上面三条会全部失效。
//
// 于是本上下文只有一个仓储：ScreeningTemplateRepository。
//
// # 为什么没有嵌 EventRecorder
//
// 事件是聚合对外的声明，只有根有资格发。「某条条件被改了」在语义上永远是
// 「某个模板被改了」。
type Criterion struct {
	// ID 为 0 表示尚未落库。仓储的子实体 diff 正是靠它区分 INSERT 与 UPDATE。
	ID         uint64
	TemplateID uint64

	// Spec 是这条条件表达的规则。它是值对象，整体替换而不是逐字段修改——
	// 「字段、比较符、值」三者之间有跨字段不变式，逐字段改必然经过非法的中间态。
	Spec value_objects.Criterion

	// SortOrder 由根统一维护，恒为 0..n-1 的稠密连续序列。
	// 它决定前端表单里条件的排列顺序，也决定筛选结果表格的列顺序，
	// 因此不能是「用户随便填的数字」。
	SortOrder int

	CreatedAt time.Time
	UpdatedAt time.Time

	// attached 是「这个子实体确实由根创建」的凭据，刻意不导出。
	//
	// 包外可以写出 &entities.Criterion{...} 并 append 进 t.Criteria，编译器拦不住；
	// 但它无论如何也设不了这个字段（不导出的字段在包外既不能用字面量赋值，
	// 也不能赋值访问）。于是 Validate() 能在仓储写库之前把这种绕过根
	// 偷偷塞进来的条件揪出来——这正是「条件只能经由根产生」这条约定的运行期防线。
	attached bool
}

// newCriterion 是子实体唯一的构造函数，且**不导出**。
//
// 包外没有任何办法调用它——这就是「条件只能经由根产生」在编译期的表达。
// 包内它只有两个调用点，都在 screening_template.go 里：
//   - (*ScreeningTemplate).AddCriterion      领域路径，执行全部不变式
//   - (*ScreeningTemplate).RehydrateCriterion 持久化重建路径，跳过校验（既成事实）
func newCriterion(
	templateID uint64,
	spec value_objects.Criterion,
	sortOrder int,
	now time.Time,
) *Criterion {
	return &Criterion{
		TemplateID: templateID,
		Spec:       spec,
		SortOrder:  sortOrder,
		CreatedAt:  now,
		UpdatedAt:  now,
		attached:   true,
	}
}

// Field / Operator / Key / Describe 是对 Spec 的转发访问器。
//
// 有了它们，调用方不必到处写 c.Spec.Field()；更重要的是它们都是**只读**的，
// 而 Spec 本身是值对象——包外拿到一份拷贝也改不动实体里的那一份。
func (c *Criterion) Field() value_objects.FieldName          { return c.Spec.Field() }
func (c *Criterion) Operator() value_objects.CompareOperator { return c.Spec.Operator() }
func (c *Criterion) Key() string                             { return c.Spec.Key() }
func (c *Criterion) Describe() string                        { return c.Spec.Describe() }

// ---------------------------------------------------------------------------
// 以下修改方法全部不导出：只有同包的聚合根能调用它们。
// domain_services 与 application 拿到 *Criterion 也只能读，不能改。
// ---------------------------------------------------------------------------

func (c *Criterion) setSortOrder(n int, now time.Time) {
	if c.SortOrder == n {
		// 没变就不动 UpdatedAt：仓储据此判断这一行要不要发 UPDATE，
		// 调整一条条件的位置不该把整个模板的条件行全部重写一遍。
		return
	}
	c.SortOrder = n
	c.UpdatedAt = now
}

func (c *Criterion) setSpec(spec value_objects.Criterion, now time.Time) {
	if c.Spec.Equal(spec) {
		return
	}
	c.Spec = spec
	c.UpdatedAt = now
}

func (c *Criterion) bindTemplate(templateID uint64) { c.TemplateID = templateID }

func (c *Criterion) isAttached() bool { return c.attached }
