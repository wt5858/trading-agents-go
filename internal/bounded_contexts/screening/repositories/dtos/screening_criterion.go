package dtos

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
)

// ScreeningCriterionDto 对应 screening_criteria 表，是**子实体**的持久化形态。
//
// 注意它没有导出的 ToDomain，理由见本包的包注释。
type ScreeningCriterionDto struct {
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// (template_id, field, operator) 唯一索引是「同一模板内条件不重复」的数据库兜底。
	//
	// 聚合根的 AddCriterion 已经判过一次，为什么还要这一道：内存判断只在
	// **同一个聚合实例**内成立。同一个用户开两个标签页同时编辑同一个模板，
	// 会加载出两个各自认为自己没重复的聚合实例。唯一索引是唯一挡得住那一幕的东西。
	TemplateID uint64 `gorm:"column:template_id;not null;uniqueIndex:uk_screening_criteria_template_field,priority:1"`
	Field      string `gorm:"column:field;type:varchar(32);not null;uniqueIndex:uk_screening_criteria_template_field,priority:2"`
	Operator   string `gorm:"column:operator;type:varchar(16);not null;uniqueIndex:uk_screening_criteria_template_field,priority:3"`

	// Values 存 JSON 数组，因为取值个数随比较符变化（eq 一个、between 两个、in 若干）。
	//
	// 为什么不拆成 value_min / value_max / value_list 三列：那会让「个数与比较符
	// 相符」这条不变式在表结构里表达成一组互相排斥的 NULL 约束，
	// 而它在领域层已经由 CompareOperator.Arity 判过了。
	// 为什么不存成逗号分隔串：行业名里就带逗号，而 JSON 的转义是现成的。
	//
	// 全部存成字符串数组、不区分数值与文本：数值的解析口径归属
	// value_objects.NewCriterion（它按字段类型决定怎么解析），
	// 库里存两套类型只会让那个口径出现第二个定义点。
	Values []byte `gorm:"column:values_json;type:json"`

	// SortOrder 上**没有**唯一约束，这是刻意的：重排的中间态必然出现两行同号
	// （交换两条条件的位置时），加了唯一约束就得引入临时负数之类的把戏。
	// 稠密连续由聚合根保证，数据库只负责按它排序。
	SortOrder int       `gorm:"column:sort_order;not null;default:0;index:idx_screening_criteria_template_sort,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (ScreeningCriterionDto) TableName() string { return "screening_criteria" }

// attachTo 把一行条件挂回它的聚合根。**不导出**，因为包外不该有任何办法
// 从一行数据造出一条游离的筛选条件——它在领域里不是一个合法的东西。
//
// 最终落到 t.RehydrateCriterion 上：无论是领域路径（AddCriterion）还是重建路径，
// 子实体的产生都必须经过根，这一点在读写两条路上是一致的。
//
// 这里走的是 value_objects.RehydrateCriterion，而它**仍然跑完整校验**——
// 与本项目其它重建路径相反。理由只有一条：这个值会变成一条查询语句，
// 而「库里的数据可信」这个前提在安全边界上不成立。详见 RehydrateFieldName 的注释。
// 校验失败时得到零值条件，模板照样列得出来，只是执行时会明确报错。
func (dto ScreeningCriterionDto) attachTo(t *entities.ScreeningTemplate) {
	t.RehydrateCriterion(
		dto.ID,
		value_objects.RehydrateCriterion(dto.Field, dto.Operator, dto.decodeValues()),
		dto.SortOrder,
		dto.CreatedAt,
		dto.UpdatedAt,
	)
}

// decodeValues 把 JSON 列还原成字符串切片。
//
// 解析失败返回空切片而不是 panic：一行坏 JSON 会让这条条件在重建时变成零值，
// 由聚合与查询构造器分别拦下，而不是打挂整个模板列表接口。
func (dto ScreeningCriterionDto) decodeValues() []string {
	if len(dto.Values) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(dto.Values, &out); err != nil {
		return nil
	}
	return out
}

// FromDomainCriterion 把子实体投影成 DTO。
//
// templateID 由调用方显式传入，而不是读 c.TemplateID：新建模板时子实体身上的
// template_id 还是 0（自增主键要等根 INSERT 之后才知道）。由仓储在拿到主键之后
// 统一传入，比让每个调用点自己记得先回填要可靠。
func FromDomainCriterion(templateID uint64, c *entities.Criterion) *ScreeningCriterionDto {
	values, err := json.Marshal(c.Spec.Values())
	if err != nil {
		// 入参是一个 []string，marshal 不可能失败；真失败了也只能是不可恢复的
		// 编程错误。退化成 NULL 列好过让整次保存失败——读路径会把它还原成零值条件。
		values = nil
	}
	return &ScreeningCriterionDto{
		ID:         c.ID,
		TemplateID: templateID,
		Field:      c.Field().String(),
		Operator:   c.Operator().String(),
		Values:     values,
		SortOrder:  c.SortOrder,
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
	}
}

// FromDomainCriteria 批量投影，供仓储一次性构造批量写入的入参。
func FromDomainCriteria(templateID uint64, criteria []*entities.Criterion) []*ScreeningCriterionDto {
	out := make([]*ScreeningCriterionDto, 0, len(criteria))
	for _, c := range criteria {
		out = append(out, FromDomainCriterion(templateID, c))
	}
	return out
}

// SameAs 判断一行条件是否与库里那一行完全一致，供仓储的子实体 diff 使用。
//
// 只比**可变列**：id / template_id / field / operator / created_at 一旦写入就不再变化
// （field 与 operator 是判重键，改它们等价于删一条加一条，聚合根不允许就地改），
// 把它们纳入比较不会多发现任何差异，只会让「改了取值」这类修改因为
// created_at 的时区往返误差而被误判成有变化，白白多发一条 UPDATE。
//
// values 用字节比较：两边都由同一个 json.Marshal 生成，同样的字符串切片
// 必然产生同样的字节序列，不需要反序列化回去再逐个比。
func (dto ScreeningCriterionDto) SameAs(other ScreeningCriterionDto) bool {
	if dto.SortOrder != other.SortOrder {
		return false
	}
	if len(dto.Values) != len(other.Values) {
		return false
	}
	for i := range dto.Values {
		if dto.Values[i] != other.Values[i] {
			return false
		}
	}
	return true
}
