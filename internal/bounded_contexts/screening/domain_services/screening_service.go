package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ScreeningService 编排选股模板与筛选执行的全部用例。
//
// 依赖仓储的具体类型而不是接口：仓储在本项目里只有一个实现，
// 多声明一层接口既不能换实现，又让「改一个方法要动三个文件」。
// 而 StockScreener 是接口，因为它是本层声明、由持久化层实现的消费端口，
// 测试里要能换成桩。
type ScreeningService struct {
	templates *repositories.ScreeningTemplateRepository
	screener  StockScreener
	publisher domain_event.Publisher
}

func NewScreeningService(
	templates *repositories.ScreeningTemplateRepository,
	screener StockScreener,
	publisher domain_event.Publisher,
) *ScreeningService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &ScreeningService{templates: templates, screener: screener, publisher: publisher}
}

// ---------------------------------------------------------------------------
// 入参形状
// ---------------------------------------------------------------------------

// CriterionInput 是接口层传来的一条筛选条件的裸形态。
//
// 它收字符串而不是值对象：HTTP 边界拿到的本来就是字符串，
// 让 handler 去构造值对象等于把「怎么解析一条条件」这件事挪到了接口层，
// 而那是业务规则的一部分（字段白名单、比较符相容性、元数），必须在领域侧。
type CriterionInput struct {
	Field    string
	Operator string
	Values   []string
}

// ScreenInput 是一次临时筛选的入参。
type ScreenInput struct {
	Criteria      []CriterionInput
	SortField     string
	SortDirection string
	Limit         int
}

// TemplateInput 是新建/更新模板的入参。
//
// 更新时 Criteria 为空表示「不动条件」，非空表示「整份替换」。
// 没有「增量加一条」的入参形状，是刻意的：编辑模板的表单一次提交整份列表，
// 增量语义会让「用户删掉了一条」变得无法表达（缺席究竟是删除还是没改？）。
type TemplateInput struct {
	Name          string
	Description   string
	Criteria      []CriterionInput
	SortField     string
	SortDirection string
	Limit         int
	IsPublic      bool
}

// ---------------------------------------------------------------------------
// 模板 CRUD
// ---------------------------------------------------------------------------

// CreateTemplate 新建选股模板。
//
// 没有「先查有没有重名」这一步：那是 TOCTOU，两个并发请求会双双查到「不重名」
// 然后双双插入。重名由 (user_id, name) 唯一索引挡住，仓储把 1062 翻译成
// AlreadyExists。少一次查询，还多一层真正的保证。
func (s *ScreeningService) CreateTemplate(
	ctx context.Context, op Operator, in TemplateInput,
) (*entities.ScreeningTemplate, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	name, err := value_objects.NewTemplateName(in.Name)
	if err != nil {
		return nil, err
	}
	specs, err := parseCriteria(in.Criteria)
	if err != nil {
		return nil, err
	}
	sort, err := value_objects.NewSortSpec(in.SortField, in.SortDirection)
	if err != nil {
		return nil, err
	}

	t, err := entities.NewScreeningTemplate(op.UserID, name, in.Description)
	if err != nil {
		return nil, err
	}
	// 条件整份交给聚合根：重复、上限、排序全部由它判定，本层一条都不重复判。
	// 子实体也正是在这里、且只可能在这里诞生。
	if err := t.ReplaceCriteria(specs); err != nil {
		return nil, err
	}
	t.SetSort(sort)
	t.SetLimit(in.Limit)
	t.SetPublic(in.IsPublic)

	// 交出去的是整个聚合根，根与条件在仓储的同一个事务里落库。
	if err := s.templates.Save(ctx, t); err != nil {
		return nil, err
	}
	// 事件在领域决策与落库都完成之后才发布，保证消费方不会读到尚未落库的聚合。
	s.publish(ctx, t)
	return t, nil
}

// UpdateTemplate 更新模板。
//
// 全部改动先在内存里的聚合上完成，最后一次 Save 落库。绝不是「改名发一条
// UPDATE、改条件再发一条」——那样中途失败会留下一个改了一半的模板，
// 而聚合的意义正是让这一组改动要么全成要么全不成。
func (s *ScreeningService) UpdateTemplate(
	ctx context.Context, op Operator, templateID uint64, in TemplateInput,
) (*entities.ScreeningTemplate, error) {
	t, err := s.loadOwned(ctx, op, templateID)
	if err != nil {
		return nil, err
	}

	if in.Name != "" {
		name, err := value_objects.NewTemplateName(in.Name)
		if err != nil {
			return nil, err
		}
		if err := t.Rename(name); err != nil {
			return nil, err
		}
	}
	t.UpdateDescription(in.Description)

	if len(in.Criteria) > 0 {
		specs, err := parseCriteria(in.Criteria)
		if err != nil {
			return nil, err
		}
		// 整份替换而不是逐条增删：理由见 ReplaceCriteria 的注释——
		// 逐条增删的中间态会反复经过「零条件」「超上限」这些非法状态。
		if err := t.ReplaceCriteria(specs); err != nil {
			return nil, err
		}
	}

	if in.SortField != "" {
		sort, err := value_objects.NewSortSpec(in.SortField, in.SortDirection)
		if err != nil {
			return nil, err
		}
		t.SetSort(sort)
	}
	if in.Limit > 0 {
		t.SetLimit(in.Limit)
	}
	t.SetPublic(in.IsPublic)

	if err := s.templates.Save(ctx, t); err != nil {
		return nil, err
	}
	s.publish(ctx, t)
	return t, nil
}

// DeleteTemplate 删除模板及其全部条件。
//
// 先加载再删，是为了让「不是你的模板」和「模板不存在」走同一条 NotFound 路径；
// 真正的并发保证在仓储的 DELETE 谓词里（user_id 进了 WHERE），
// 本层的这次加载只是为了给出可读的错误，不是安全边界。
func (s *ScreeningService) DeleteTemplate(ctx context.Context, op Operator, templateID uint64) error {
	t, err := s.loadOwned(ctx, op, templateID)
	if err != nil {
		return err
	}
	// 条件的删除不在这里：调用方交出的是根的标识，条件怎么跟着消失
	// 是仓储在同一个事务里的实现细节。本层连 screening_criteria 这个名字都不该知道。
	return s.templates.Delete(ctx, t.ID, op.UserID)
}

// GetTemplate 读取单个模板。
//
// 可见性判定用 ReadableBy 而不是 OwnedBy：公开模板（选股策略广场）
// 任何登录用户都能查看并执行，但只有属主能改。
func (s *ScreeningService) GetTemplate(
	ctx context.Context, op Operator, templateID uint64,
) (*entities.ScreeningTemplate, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	if templateID == 0 {
		return nil, custom_errors.Invalid("模板 ID 不能为空")
	}
	t, err := s.templates.FindByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if !t.ReadableBy(op.UserID) {
		return nil, custom_errors.NotFound("选股模板(id=%d) 不存在", templateID)
	}
	return t, nil
}

// ListTemplates 列出当前用户的全部模板（含各自的条件）。
//
// 管理员也只看自己的模板：选股模板是个人策略，不是可运维的对象。
// 这是本上下文与 analysis 的区别——那边管理员需要排查别人的任务。
func (s *ScreeningService) ListTemplates(ctx context.Context, op Operator) ([]*entities.ScreeningTemplate, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	return s.templates.ListByUser(ctx, op.UserID)
}

// ListPublicTemplates 列出公开模板，分页。
func (s *ScreeningService) ListPublicTemplates(
	ctx context.Context, op Operator, page shared_vo.Page,
) ([]*entities.ScreeningTemplate, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	return s.templates.ListPublic(ctx, page)
}

// ---------------------------------------------------------------------------
// 执行筛选
// ---------------------------------------------------------------------------

// Execute 执行一次临时筛选。
//
// ===========================================================================
// 本方法的全部工作是「构造一个合法的 ScreenQuery 然后交出去」
// ===========================================================================
//
// 它不遍历股票，不取行情，不在内存里比较任何一个数——那些全都发生在
// StockScreener 的实现里，而实现把它们变成了数据库的 WHERE / $match。
// 本层甚至不知道数据落在哪个存储里。
//
// 这不是「把活推给下游」，而是接口形状决定的必然结果：StockScreener 上
// 没有任何一个能拿回股票列表的方法（见 ports.go 的长注释），
// 所以「先捞 5000 只票再 for 一遍」这种写法在这里根本写不出来。
//
// 排序与条数一并交下去，同样是为了让数据库执行它们：
// 在这里排序意味着先把全部命中行拉回内存，limit 就成了一次浪费之后的装饰。
func (s *ScreeningService) Execute(
	ctx context.Context, op Operator, in ScreenInput,
) (value_objects.ScreeningResultSet, error) {
	if err := requireLogin(op); err != nil {
		return value_objects.EmptyResultSet(), err
	}
	specs, err := parseCriteria(in.Criteria)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	sort, err := value_objects.NewSortSpec(in.SortField, in.SortDirection)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	// 值对象的构造器在这里承担了全部入参不变式：非空、不超上限、不重复、
	// 条数收敛。查询实现因此可以不做任何入参防御。
	query, err := value_objects.NewScreenQuery(specs, sort, in.Limit)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	return s.run(ctx, query)
}

// ExecuteTemplate 执行一个已保存的模板。
//
// 模板的排序与条数一并生效：它们是模板的一部分（「低估值蓝筹，按市值取前 30」
// 是一个完整的策略），执行时另给一套会让保存下来的策略名不副实。
func (s *ScreeningService) ExecuteTemplate(
	ctx context.Context, op Operator, templateID uint64,
) (value_objects.ScreeningResultSet, error) {
	t, err := s.GetTemplate(ctx, op, templateID)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	// Specs() 交出的是一组不可变的规则拷贝，执行器拿到它既改不动模板，
	// 也不需要知道「条件」在模板里还有主键和排序位置这回事。
	query, err := value_objects.NewScreenQuery(t.Specs(), t.Sort, t.Limit)
	if err != nil {
		// 这条路径上最可能的失败是模板里存着一条字段无法识别的条件
		// （白名单收紧过，或者是更早的脏数据）。NewScreenQuery 会明确报错而不是
		// 跳过那一条——跳过会让筛选结果凭空变大，用户拿到一份不符合他策略的清单。
		return value_objects.EmptyResultSet(), err
	}
	return s.run(ctx, query)
}

// AvailableFields 返回可筛选字段及各自可用的比较符，供前端自动生成筛选表单。
//
// 它不碰任何存储：字段白名单是编译期常量（value_objects.fieldRegistry），
// 为它查一次库既没有数据来源，也会让「加一个字段」变成一次数据迁移。
//
// 它同时是前端与后端之间那份契约的**唯一**发布点：前端照着这份清单画表单，
// 提交回来的字段必然在白名单里，于是正常用户永远不会撞上「不支持的筛选字段」。
func (s *ScreeningService) AvailableFields() []value_objects.FieldSpec {
	return value_objects.AllFieldSpecs()
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

func (s *ScreeningService) run(ctx context.Context, query value_objects.ScreenQuery) (value_objects.ScreeningResultSet, error) {
	if s.screener == nil {
		return value_objects.EmptyResultSet(), custom_errors.Unavailable("选股执行器未配置")
	}
	return s.screener.Screen(ctx, query)
}

// loadOwned 加载模板并校验归属。
//
// 归属不符时返回 NotFound 而不是 Forbidden：Forbidden 等于确认「这个 id 存在」，
// 会让模板 ID 空间变成可探测的信道——攻击者可以靠遍历 id 数出别人有几个模板，
// 甚至通过报错差异推断某个模板是否公开。权限判定属于本层（谁能调用），
// 不属于 entities/（业务规则）。
//
// 注意这里用 OwnedBy 而不是 ReadableBy：本方法服务于写路径，
// 公开只意味着别人能读，不意味着别人能改。
func (s *ScreeningService) loadOwned(ctx context.Context, op Operator, templateID uint64) (*entities.ScreeningTemplate, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	if templateID == 0 {
		return nil, custom_errors.Invalid("模板 ID 不能为空")
	}
	t, err := s.templates.FindByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if !t.OwnedBy(op.UserID) {
		return nil, custom_errors.NotFound("选股模板(id=%d) 不存在", templateID)
	}
	return t, nil
}

// publish 取出聚合累积的事件并发布。取出即清空，保证同一事件不会被重复发布。
func (s *ScreeningService) publish(ctx context.Context, t *entities.ScreeningTemplate) {
	if evts := t.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// parseCriteria 把接口层传来的裸字符串解析成值对象。
//
// 这是「形状校验」的归属地：字段是否在白名单里、比较符与字段类型是否相容、
// 值的个数是否与比较符的元数相符——全部由 value_objects.NewCriterion 判定。
// 而「同一模板内不能重复」那种要看聚合状态的判定属于 entities/。
//
// 遇到坏条件立刻失败，不做「跳过这一条继续」：少一个过滤器会让筛选结果
// 凭空变大，用户看到的是一份不符合他条件的清单却没有任何提示。
func parseCriteria(inputs []CriterionInput) ([]value_objects.Criterion, error) {
	if len(inputs) == 0 {
		return nil, custom_errors.Invalid("至少需要一个筛选条件")
	}
	out := make([]value_objects.Criterion, 0, len(inputs))
	for i, in := range inputs {
		c, err := value_objects.NewCriterion(in.Field, in.Operator, in.Values)
		if err != nil {
			return nil, custom_errors.Invalid(
				"第 %d 个筛选条件不合法：%s", i+1, custom_errors.MessageOf(err))
		}
		out = append(out, c)
	}
	return out, nil
}
