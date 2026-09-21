// Package dtos 承载选股筛选上下文的持久化对象与领域映射。
//
// 映射函数（ToDomain / FromDomainXxx）与 DTO 定义在同一文件，本项目没有 mapper 包：
// 表结构与它的映射规则是同一件事的两面，拆开只会让改一列要开两个文件。
//
// 本包的类型绝不允许越过 repositories/ 向上泄漏。带 gorm 标签的结构一旦出现在
// domain_services 或 handler 的签名里，换存储就要改业务代码。
//
// # 本包如何体现「子实体只能经由根产生」
//
// ScreeningCriterionDto **没有**导出的 ToDomain：一条脱离模板的筛选条件在领域里
// 不是一个合法的东西，给它一个能单独造出实体的函数，就等于在持久化层
// 开了一扇绕过聚合根的后门。它只有一个不导出的 attachTo，由
// ScreeningTemplateDto.ToDomain 调用，最终落到根的 RehydrateCriterion 上。
package dtos

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
)

// ScreeningTemplateDto 对应 screening_templates 表。
//
// 通用约定与其它上下文一致：时间统一 datetime(3)、关掉 GORM 的自动时间戳
// （时间由聚合根维护，否则「导入存量模板」会被静默改写成导入时刻）。
type ScreeningTemplateDto struct {
	ID     uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	UserID uint64 `gorm:"column:user_id;not null;uniqueIndex:uk_screening_templates_user_name,priority:1"`
	// (user_id, name) 唯一索引是「同一用户下模板名唯一」这条不变式的**唯一**保证。
	// 聚合里没有对应的内存判断，因为那条规则跨聚合实例，内存看不见兄弟模板；
	// 而「先查有没有重名再插」是 TOCTOU，两个并发请求会双双查到「没有」然后都插进去。
	Name        string `gorm:"column:name;type:varchar(128);not null;uniqueIndex:uk_screening_templates_user_name,priority:2"`
	Description string `gorm:"column:description;type:varchar(600);not null;default:''"`

	// 排序规则拆成字段名 + 方向两列，而不是存一个 "total_mv desc" 的串。
	//
	// 理由和注入边界是同一回事：存成一个串，读回来就得解析它，
	// 而解析出来的那半截会被当成列名用。两列分开存，字段名读回来仍然要过
	// RehydrateSortSpec 的白名单，方向则只有两个合法取值。
	SortField     string `gorm:"column:sort_field;type:varchar(32);not null;default:''"`
	SortDirection string `gorm:"column:sort_direction;type:varchar(4);not null;default:'desc'"`
	// 列名不叫 limit：limit 是 MySQL 保留字，建表和每一条查询都要加反引号，
	// 迟早会有人在手写 SQL 里漏掉。
	ResultLimit int  `gorm:"column:result_limit;not null;default:50"`
	IsPublic    bool `gorm:"column:is_public;not null;default:false;index:idx_screening_templates_public"`

	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (ScreeningTemplateDto) TableName() string { return "screening_templates" }

// ToDomain 把一行模板连同它的子实体行重建成完整聚合。
//
// 签名里带着 criteria 是刻意的：这个聚合**没有**「只有根、没有条件」的合法形态
// （Validate 会拒绝零条件的模板）。允许单独重建一个空壳根，调用方迟早会拿着它
// 去 Save，而 Save 会把「条件为空」如实理解为「用户清空了这个模板」，
// 一次读取就变成了一次破坏。要重建就得给全，这个约束由签名本身表达。
func (dto ScreeningTemplateDto) ToDomain(criteria []ScreeningCriterionDto) *entities.ScreeningTemplate {
	t := entities.RehydrateScreeningTemplate(
		dto.ID,
		dto.UserID,
		// 不走 NewTemplateName：落库的行是既成事实，读路径再校验一次，
		// 一条规则收紧前存下的历史数据就能把整个列表接口打挂。
		value_objects.RehydrateTemplateName(dto.Name),
		dto.Description,
		// 排序字段**仍然**要过白名单，这是与模板名的区别所在：
		// 它会进 ORDER BY。无法识别时退化成零值，执行时回落到默认排序。
		value_objects.RehydrateSortSpec(dto.SortField, dto.SortDirection),
		dto.ResultLimit,
		dto.IsPublic,
		dto.CreatedAt,
		dto.UpdatedAt,
		len(criteria),
	)
	for i := range criteria {
		criteria[i].attachTo(t)
	}
	return t
}

func FromDomainTemplate(t *entities.ScreeningTemplate) *ScreeningTemplateDto {
	sort := t.Sort.OrDefault()
	return &ScreeningTemplateDto{
		ID:            t.ID,
		UserID:        t.UserID,
		Name:          t.Name.String(),
		Description:   t.Description,
		SortField:     sort.Field().String(),
		SortDirection: sort.Direction().String(),
		ResultLimit:   t.Limit,
		IsPublic:      t.IsPublic,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
}

// ToDomainTemplates 把「一批模板行 + 它们全部的条件行」缝成若干个完整聚合。
//
// 缝合放在本包而不是仓储里，是为了让仓储的 ListByUser 读起来就是
// 「两条查询 + 一次缝合」这三行。
//
// 实现上先按 template_id 归拢成一个 map 再分发，整体 O(模板数 + 条件数)。
// 绝不能写成「对每个模板遍历一遍全部条件」——那是 O(模板数 × 条件数)，
// 也绝不能写成「对每个模板查一次库」——那是 N+1，正是本方法存在的理由。
//
// criteriaRows 必须已经按 (template_id, sort_order) 排好序：排序是数据库的活，
// 在这里再排一次是重复劳动，而且会让「排序口径」出现第二个定义点。
func ToDomainTemplates(
	templateRows []ScreeningTemplateDto,
	criteriaRows []ScreeningCriterionDto,
) []*entities.ScreeningTemplate {
	byTemplate := make(map[uint64][]ScreeningCriterionDto, len(templateRows))
	for _, row := range criteriaRows {
		byTemplate[row.TemplateID] = append(byTemplate[row.TemplateID], row)
	}
	out := make([]*entities.ScreeningTemplate, 0, len(templateRows))
	for _, t := range templateRows {
		out = append(out, t.ToDomain(byTemplate[t.ID]))
	}
	return out
}
