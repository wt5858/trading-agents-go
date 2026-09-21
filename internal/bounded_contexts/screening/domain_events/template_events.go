// Package domain_events 定义选股筛选上下文对外广播的领域事件。
//
// 三个事件全部由聚合根 ScreeningTemplate 抛出，没有任何一个由子实体 Criterion 抛出——
// 子实体不是根，它没有对外发声的资格，「某条筛选条件被改了」在语义上永远是
// 「某个选股模板被改了」。这也是为什么 Criterion 不嵌 EventRecorder。
//
// # 为什么「执行了一次筛选」不在这里
//
// 筛选执行不是领域决策，它不改变任何聚合的状态，只是一次查询。
// 为查询发领域事件会让事件总线被读流量淹没，而消费方从中也拿不到任何
// 「系统做了什么决定」的信息。执行次数这类统计属于可观测性，走日志与指标。
package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnTemplateCreatedEventName           = "screening.template_created"
	OnTemplateCriteriaChangedEventName   = "screening.template_criteria_changed"
	OnTemplateVisibilityChangedEventName = "screening.template_visibility_changed"
)

// OnTemplateCreated 在新模板创建时抛出。
//
// TemplateID 刻意不在这里：模板 ID 是自增主键，创建事件在聚合内部产生时
// 这个 ID 还不存在（要等 INSERT 回填）。硬塞一个 0 进去比不带更糟——
// 消费方会拿着 0 去查库。需要 ID 的消费方应当订阅落库之后的路径。
type OnTemplateCreated struct {
	domain_event.BaseDomainEvent
	UserID       uint64 `json:"userId"`
	TemplateName string `json:"templateName"`
}

func NewOnTemplateCreated(userID uint64, name string) *OnTemplateCreated {
	return &OnTemplateCreated{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		TemplateName:    name,
	}
}

func (e *OnTemplateCreated) Name() string { return OnTemplateCreatedEventName }

func (e *OnTemplateCreated) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTemplateCriteriaChanged 在模板的条件集合发生增删改时抛出。
//
// 载荷只有标识与条数，不带条件本身：领域事件跨上下文传播，携带子实体
// 等于把它们的指针交到了根的控制范围之外。下游真正关心的是
// 「这个模板的口径变了，之前基于它缓存的选股结果要作废」，那只需要一个 ID。
type OnTemplateCriteriaChanged struct {
	domain_event.BaseDomainEvent
	UserID        uint64 `json:"userId"`
	TemplateID    uint64 `json:"templateId"`
	TemplateName  string `json:"templateName"`
	CriteriaCount int    `json:"criteriaCount"`
}

func NewOnTemplateCriteriaChanged(userID, templateID uint64, name string, count int) *OnTemplateCriteriaChanged {
	return &OnTemplateCriteriaChanged{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		TemplateID:      templateID,
		TemplateName:    name,
		CriteriaCount:   count,
	}
}

func (e *OnTemplateCriteriaChanged) Name() string { return OnTemplateCriteriaChangedEventName }

func (e *OnTemplateCriteriaChanged) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTemplateVisibilityChanged 在模板被公开或收回时抛出。
//
// 它单独成一个事件而不是并进 CriteriaChanged：可见性变化是一次**授权**变更，
// 消费方（审计、内容审核）关心的是它，而不关心条件调了几个数。
// 把两件事混在一个事件里，消费方只能靠比对载荷猜自己要不要处理。
type OnTemplateVisibilityChanged struct {
	domain_event.BaseDomainEvent
	UserID     uint64 `json:"userId"`
	TemplateID uint64 `json:"templateId"`
	IsPublic   bool   `json:"isPublic"`
}

func NewOnTemplateVisibilityChanged(userID, templateID uint64, isPublic bool) *OnTemplateVisibilityChanged {
	return &OnTemplateVisibilityChanged{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		UserID:          userID,
		TemplateID:      templateID,
		IsPublic:        isPublic,
	}
}

func (e *OnTemplateVisibilityChanged) Name() string { return OnTemplateVisibilityChangedEventName }

func (e *OnTemplateVisibilityChanged) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
