// Package http_handlers 把选股筛选上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
// 本包也绝不直接构造或修改子实体：编译器不允许（子实体的构造与修改方法都不导出），
// 这正是聚合设计想要的结果。
//
// 特别地，本包**不做字段白名单校验**。看起来「在入口挡掉非法字段」是接口层的活，
// 但那会造成两个白名单：一个在这里，一个在 value_objects.fieldRegistry。
// 两份清单迟早分叉，而分叉的方向一定是接口层这份更宽松（谁也不会记得同步收紧），
// 于是防线形同虚设。正确的做法是让唯一的白名单守在值对象的构造点上，
// 任何入口——HTTP、将来的 gRPC、批量导入——都绕不过它。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：选股上下文不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type ScreeningHandler struct {
	screeningService *domain_services.ScreeningService
	resolve          OperatorResolver
}

func NewScreeningHandler(
	screeningService *domain_services.ScreeningService,
	resolve OperatorResolver,
) *ScreeningHandler {
	return &ScreeningHandler{screeningService: screeningService, resolve: resolve}
}

// Register 挂载路由。
//
// 路径形状让「条件是模板的下级资源」在 URL 上就不成立——这里**没有**
// /templates/:id/criteria/:criterionId 这样的接口。那样的路径会诱导出一个
// 「按条件 ID 直接改」的端点，而那正是绕过聚合根的那条路。
// 改条件的唯一入口是 PUT /templates/:id，整份提交。
func (h *ScreeningHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/screening", authRequired)

	// 字段字典要放在 :id 路由之前注册。gin 的路由树里静态段优先于参数段，
	// 顺序其实无关紧要，但把它排在最前也符合「前端第一步就要拿它」的使用顺序。
	g.GET("/fields", h.ListFields)

	g.POST("/run", h.Run)

	g.GET("/templates", h.ListTemplates)
	g.GET("/templates/public", h.ListPublicTemplates)
	g.POST("/templates", h.CreateTemplate)
	g.GET("/templates/:id", h.GetTemplate)
	g.PUT("/templates/:id", h.UpdateTemplate)
	g.DELETE("/templates/:id", h.DeleteTemplate)
	g.POST("/templates/:id/run", h.RunTemplate)
}

// ---------------------------------------------------------------------------
// 字段字典
// ---------------------------------------------------------------------------

// fieldDictView 字段字典。
//
// 本可以直接回 gin.H{"fields": ...}，但那样这份字典就不在 OpenAPI 里，
// 而它恰恰是前端接入本上下文的第一份文档——没有类型，前端就只能照着
// 某次抓包的结果猜字段名，而抓包的那一份永远不会跟着后端一起改。
// 外层多包一个对象而不是直接返回数组，是为了将来加 version / updatedAt
// 这类字典级元信息时不必改变响应的顶层形状。
type fieldDictView struct {
	Fields []fieldView `json:"fields"`
} // @name screening.FieldDictView

// ListFields 返回全部可筛选字段及各自可用的比较符。
//
// 它是前端自动生成筛选表单的数据源：拿到字段名、中文标签、类型、量纲，
// 以及该类型允许的比较符，表单就能自己画出来。没有它，前端就得手抄一份字段清单，
// 而那份清单与后端白名单的每一次分叉，都会表现为用户填完表单被拒绝。
//
// 不声明 4xx（401 除外）：字段白名单是编译期常量，这条路径不读任何存储，
// 也没有可传错的入参。
//
// @Summary  查询可筛选字段字典
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=fieldDictView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /screening/fields [get]
func (h *ScreeningHandler) ListFields(c *gin.Context) {
	if _, ok := h.operator(c); !ok {
		return
	}
	specs := h.screeningService.AvailableFields()
	views := make([]fieldView, 0, len(specs))
	for _, spec := range specs {
		views = append(views, toFieldView(spec))
	}
	response.OK(c, fieldDictView{Fields: views})
}

// ---------------------------------------------------------------------------
// 执行筛选
// ---------------------------------------------------------------------------

// criterionRequest 一条筛选条件。field 的取值来自 GET /screening/fields，
// operator 必须在该字段 kind 允许的集合里——两者的相容性由值对象判定。
type criterionRequest struct {
	Field    string `json:"field" binding:"required" example:"pe"`
	Operator string `json:"operator" binding:"required" enums:"gt,gte,lt,lte,eq,ne,between,in,not_in" example:"lt"`
	// Values 一律以字符串传，个数由比较符的元数决定：between 恰好 2 个，
	// in/not_in 至少 1 个，其余恰好 1 个。
	Values []string `json:"values" binding:"required" example:"20"`
} // @name screening.CriterionRequest

type runRequest struct {
	Criteria      []criterionRequest `json:"criteria" binding:"required"`
	SortField     string             `json:"sortField" example:"total_mv"`
	SortDirection string             `json:"sortDirection" enums:"asc,desc" example:"desc"`
	// Limit 留空按 50 条，上限 500；超出部分由值对象直接收敛，不报错。
	Limit int `json:"limit" example:"50"`
} // @name screening.RunRequest

// Run 执行一次临时筛选（不保存）。
//
// 用 POST 而不是带 query 参数的 GET：筛选条件是一个可变长度的嵌套结构，
// 塞进 query string 要么超长度限制，要么得自己发明一套编码格式——
// 而那套格式会成为第二个需要校验的输入面。
//
// 条件重复与超过 20 条在这条路径上是 400 而不是 409/429：临时筛选不经过聚合根，
// 判定全部落在 NewScreenQuery 里，它一律抛 Invalid。同样的输入存成模板会是
// 409/429（见 CreateTemplate），差异来自判定发生在哪一层，不是笔误。
//
// @Summary  执行临时筛选
// @Tags     选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     runRequest true "筛选条件、排序与条数"
// @Success  200  {object} response.Envelope{data=resultSetView}
// @Failure  400  {object} response.Envelope "条件为空、超过 20 条、重复，或字段/比较符/排序不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  503  {object} response.Envelope "行情或财务存储未配置，无法执行筛选"
// @Router   /screening/run [post]
func (h *ScreeningHandler) Run(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req runRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	result, err := h.screeningService.Execute(c.Request.Context(), op, domain_services.ScreenInput{
		Criteria:      toCriterionInputs(req.Criteria),
		SortField:     req.SortField,
		SortDirection: req.SortDirection,
		Limit:         req.Limit,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toResultSetView(result))
}

// RunTemplate 执行一个已保存的模板。
//
// 可见性判据是 ReadableBy：自己的模板与广场上的公开模板都能执行，
// 执行时一律用模板自带的排序与条数，不接受临时覆盖。
//
// 400 在这里有一条不那么显然的来源：模板里存着一条字段已不在白名单里的条件
// （白名单收紧过，或是更早的脏数据）。NewScreenQuery 明确报错而不是跳过那一条。
//
// @Summary  执行已保存的选股模板
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "模板 ID"
// @Success  200 {object} response.Envelope{data=resultSetView}
// @Failure  400 {object} response.Envelope "模板 ID 非正整数，或模板内含无法识别的字段"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "模板不存在，或既非本人所有也未公开"
// @Failure  503 {object} response.Envelope "行情或财务存储未配置，无法执行筛选"
// @Router   /screening/templates/{id}/run [post]
func (h *ScreeningHandler) RunTemplate(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	templateID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	result, err := h.screeningService.ExecuteTemplate(c.Request.Context(), op, templateID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toResultSetView(result))
}

// ---------------------------------------------------------------------------
// 模板 CRUD
// ---------------------------------------------------------------------------

// templateRequest 新建与更新共用同一份入参。
//
// 更新语义是「按字段缺省跳过」：name 为空表示不改名，criteria 为空表示不动条件，
// sortField 为空表示不改排序，limit <= 0 表示不改条数。isPublic 例外——布尔量
// 没有「未填」这个状态，每次提交都会整体生效。
type templateRequest struct {
	Name        string `json:"name" example:"低估值蓝筹"`
	Description string `json:"description"`
	// Criteria 整份替换，没有「增量加一条」的形状：编辑表单一次提交整份列表，
	// 增量语义会让「用户删掉了一条」无法表达。最多 20 条。
	Criteria      []criterionRequest `json:"criteria"`
	SortField     string             `json:"sortField" example:"total_mv"`
	SortDirection string             `json:"sortDirection" enums:"asc,desc" example:"desc"`
	Limit         int                `json:"limit" example:"50"`
	// IsPublic 置真即上架「选股策略广场」：任何登录用户都能查看并执行，但改不动。
	IsPublic bool `json:"isPublic"`
} // @name screening.TemplateRequest

// CreateTemplate 新建选股模板。
//
// 409 有两个来源：与本人已有模板重名（(user_id, name) 唯一索引），
// 以及同一份提交里出现了 (字段, 比较符) 相同的两条条件。
//
// @Summary  新建选股模板
// @Tags     选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     templateRequest true "模板名、筛选条件、排序与条数"
// @Success  200  {object} response.Envelope{data=templateView}
// @Failure  400  {object} response.Envelope "模板名为空、条件为空，或字段/比较符/排序不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  409  {object} response.Envelope "已存在同名模板，或提交中含重复的筛选条件"
// @Failure  429  {object} response.Envelope "筛选条件超过 20 条上限"
// @Router   /screening/templates [post]
func (h *ScreeningHandler) CreateTemplate(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req templateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	t, err := h.screeningService.CreateTemplate(c.Request.Context(), op, req.toInput())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTemplateView(t))
}

// UpdateTemplate 整份更新模板。
//
// 只有属主能改：公开只意味着别人能读，不意味着别人能改。非属主拿到的是 404
// 而不是 403（理由见 GetTemplate）。
//
// 409 除了重名与条件重复，还有一个并发来源：在本次加载与保存之间，
// 模板或它的某条条件被另一个请求删掉了，此时要求调用方重新加载后重试。
//
// @Summary  更新选股模板
// @Tags     选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int             true "模板 ID"
// @Param    body body     templateRequest true "待更新的字段，留空表示不改"
// @Success  200  {object} response.Envelope{data=templateView}
// @Failure  400  {object} response.Envelope "模板 ID 非正整数，或字段/比较符/排序不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "模板不存在或不属于当前用户"
// @Failure  409  {object} response.Envelope "重名、条件重复，或模板已被并发修改"
// @Failure  429  {object} response.Envelope "筛选条件超过 20 条上限"
// @Router   /screening/templates/{id} [put]
func (h *ScreeningHandler) UpdateTemplate(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	templateID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req templateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	t, err := h.screeningService.UpdateTemplate(c.Request.Context(), op, templateID, req.toInput())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTemplateView(t))
}

// GetTemplate 读取单个模板。
//
// 读路径的可见性比写路径宽：自己的模板，加上广场上的公开模板，都能读到。
//
// 不可见时返回 404 而非 403，与本仓库其它上下文一致：403 等于确认「这个 id 存在」，
// 会让模板 ID 空间变成可探测的信道——遍历 id 就能数出别人有几个模板，
// 甚至靠 403/404 的差异推断某个模板是否公开。
//
// @Summary  查询选股模板详情
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "模板 ID"
// @Success  200 {object} response.Envelope{data=templateView}
// @Failure  400 {object} response.Envelope "模板 ID 非正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "模板不存在，或既非本人所有也未公开"
// @Router   /screening/templates/{id} [get]
func (h *ScreeningHandler) GetTemplate(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	templateID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	t, err := h.screeningService.GetTemplate(c.Request.Context(), op, templateID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTemplateView(t))
}

// deleteResultView 删除结果。
//
// 与 fieldDictView 同理：gin.H 写起来省一个类型，代价是响应体的形状不进 OpenAPI，
// 只能靠人手写文档，而手写的那份迟早和代码分家。
type deleteResultView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name screening.DeleteResultView

// DeleteTemplate 删除模板及其全部条件。
//
// 只有属主能删；非属主与不存在同样都是 404，理由见 GetTemplate。
//
// @Summary  删除选股模板
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "模板 ID"
// @Success  200 {object} response.Envelope{data=deleteResultView}
// @Failure  400 {object} response.Envelope "模板 ID 非正整数"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "模板不存在或不属于当前用户"
// @Router   /screening/templates/{id} [delete]
func (h *ScreeningHandler) DeleteTemplate(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	templateID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.screeningService.DeleteTemplate(c.Request.Context(), op, templateID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deleteResultView{Deleted: true})
}

// ListTemplates 列出当前用户的全部模板。
//
// 不分页也不含别人的模板：个人模板数量有 20 条条件的天花板兜着，规模有限；
// 管理员在这里同样只看得到自己的——选股模板是个人策略，不是可运维的对象。
// 要看别人公开出来的策略走 /screening/templates/public。
//
// @Summary  列出我的选股模板
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=[]templateView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /screening/templates [get]
func (h *ScreeningHandler) ListTemplates(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	templates, err := h.screeningService.ListTemplates(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTemplateViews(templates))
}

// ListPublicTemplates 列出全站公开模板（选股策略广场），分页。
//
// 与 /screening/templates 的三点区别：取的是全站 isPublic 为真的模板而非本人的、
// 结果分页（广场的规模不设上限）、返回的模板只能读与执行，改不动——
// 对它们发 PUT/DELETE 会拿到 404。
//
// 页码与每页条数解析不出来一律退回默认值，由 shared_vo.NewPage 收敛，
// 所以这条路径没有 400：一个写错的页码不值得让读请求失败。
//
// @Summary  列出公开选股模板
// @Tags     选股
// @Produce  json
// @Security BearerAuth
// @Param    page     query    int false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int false "每页条数，默认与上限由分页值对象收敛"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]templateView}}
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Router   /screening/templates/public [get]
func (h *ScreeningHandler) ListPublicTemplates(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := shared_vo.NewPage(queryInt(c, "page"), queryInt(c, "pageSize"))
	templates, total, err := h.screeningService.ListPublicTemplates(c.Request.Context(), op, page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toTemplateViews(templates), total, page.Number, page.Size)
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// fieldView 是一个可筛选字段的完整描述，也是筛选请求里 field 的唯一合法取值来源。
type fieldView struct {
	// Name 就是提交 criterionRequest.field 时要填的串。
	Name  string `json:"name" example:"roe"`
	Label string `json:"label" example:"净资产收益率"`
	// Kind 决定该字段能接受哪些比较符：numeric 有全序，九个比较符全可用；
	// categorical 无序，只剩相等与集合运算。Operators 已按此过滤，
	// 前端不必自己再推一遍。
	Kind string `json:"kind" enums:"numeric,categorical" example:"numeric"`
	// Unit 是量纲提示（% / 万元），供输入框渲染后缀；无量纲字段不下发该键。
	Unit      string         `json:"unit,omitempty" example:"%"`
	Operators []operatorView `json:"operators"`
} // @name screening.FieldView

type operatorView struct {
	Value string `json:"value" enums:"gt,gte,lt,lte,eq,ne,between,in,not_in" example:"between"`
	Label string `json:"label" example:"介于"`
	// ValueCount 告诉前端这个比较符要渲染几个输入框：
	// 1 个（大于）、2 个（介于）、或者一个可增删的多值控件（属于，min=1 max=0）。
	MinValues int `json:"minValues" example:"2"`
	MaxValues int `json:"maxValues" example:"2"`
} // @name screening.OperatorView

// toFieldView 渲染一个字段。
//
// 注意这里**不暴露** FieldSpec.Source（数据落在哪个存储）：
// 那是查询规划的内部信息，前端知道了既用不上，又会让「某天把 pe 从行情挪到财务」
// 变成一次接口变更。
func toFieldView(spec value_objects.FieldSpec) fieldView {
	ops := value_objects.OperatorsFor(spec.Kind)
	views := make([]operatorView, 0, len(ops))
	for _, op := range ops {
		min, max := op.Arity()
		views = append(views, operatorView{
			Value: op.String(), Label: op.Label(), MinValues: min, MaxValues: max,
		})
	}
	return fieldView{
		Name:      spec.Name.String(),
		Label:     spec.Label,
		Kind:      spec.Kind.String(),
		Unit:      spec.Unit,
		Operators: views,
	}
}

type criterionView struct {
	Field    string   `json:"field" example:"pe"`
	Label    string   `json:"label" example:"市盈率"`
	Operator string   `json:"operator" enums:"gt,gte,lt,lte,eq,ne,between,in,not_in" example:"lt"`
	Values   []string `json:"values" example:"20"`
	Sort     int      `json:"sortOrder"`
} // @name screening.CriterionView

// toCriterionView 渲染一条条件。
//
// 入参是 *entities.Criterion（子实体指针），但这里只读它——
// 本包连一个能改动它的方法都调不到，因为那些方法全部不导出。
func toCriterionView(c *entities.Criterion) criterionView {
	return criterionView{
		Field:    c.Field().String(),
		Label:    c.Field().Label(),
		Operator: c.Operator().String(),
		Values:   c.Spec.Values(),
		Sort:     c.SortOrder,
	}
}

type templateView struct {
	ID          uint64          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Criteria    []criterionView `json:"criteria"`
	// SortField / SortDirection 恒为已展开的有效值：模板未指定排序时，
	// 这里回的是兜底的 total_mv desc，而不是空串。
	SortField     string    `json:"sortField" example:"total_mv"`
	SortDirection string    `json:"sortDirection" enums:"asc,desc" example:"desc"`
	Limit         int       `json:"limit" example:"50"`
	IsPublic      bool      `json:"isPublic"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
} // @name screening.TemplateView

func toTemplateView(t *entities.ScreeningTemplate) *templateView {
	if t == nil {
		return nil
	}
	criteria := make([]criterionView, 0, t.CriterionCount())
	for _, c := range t.Criteria {
		criteria = append(criteria, toCriterionView(c))
	}
	sort := t.Sort.OrDefault()
	return &templateView{
		ID:            t.ID,
		Name:          t.Name.String(),
		Description:   t.Description,
		Criteria:      criteria,
		SortField:     sort.Field().String(),
		SortDirection: sort.Direction().String(),
		Limit:         t.Limit,
		IsPublic:      t.IsPublic,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
}

func toTemplateViews(templates []*entities.ScreeningTemplate) []*templateView {
	out := make([]*templateView, 0, len(templates))
	for _, t := range templates {
		out = append(out, toTemplateView(t))
	}
	return out
}

// resultRowView 是筛选结果的一行。
//
// Values 用 map[字段名]可空值，而 Columns 单独给出顺序：
// JSON 对象的键顺序在各语言的解析器里都不保证，前端照着 Columns 渲染表头
// 才能拿到与用户填写条件一致的列序（见 ScreeningResult 的注释）。
type resultRowView struct {
	Symbol string `json:"symbol" example:"600519.SH"`
	Market string `json:"market" example:"CN"`
	Name   string `json:"name" example:"贵州茅台"`
	// Values 的键是字段名，值为字符串（数值字段）、字符串（类别字段）或 null（缺失）。
	// 数值以字符串输出，理由同其它上下文：JSON 数字在前端就是 float64。
	Values map[string]any `json:"values"`
} // @name screening.ResultRowView

type resultSetView struct {
	Items   []resultRowView `json:"items"`
	Columns []columnView    `json:"columns"`
	// Total 是命中总数，可能大于 len(items)——超出条数上限的部分被截断。
	Total int64 `json:"total" example:"128"`
	// AsOf 必须回给前端：筛选是对某一天横截面的查询，盘后与次日早盘
	// 执行同一个模板结果不同是正常的，但用户看不到日期就只会觉得系统不稳定。
	AsOf string `json:"asOf,omitempty" example:"2024-05-31"`
	// Truncated 为真表示 total 超过了本次的条数上限，页面应提示用户收紧条件。
	Truncated bool `json:"truncated"`
} // @name screening.ResultSetView

type columnView struct {
	Field string `json:"field" example:"pe"`
	Label string `json:"label" example:"市盈率"`
	Unit  string `json:"unit,omitempty" example:"%"`
} // @name screening.ColumnView

func toResultSetView(rs value_objects.ScreeningResultSet) resultSetView {
	out := resultSetView{
		Items:     make([]resultRowView, 0, len(rs.Results)),
		Columns:   []columnView{},
		Total:     rs.Total,
		Truncated: rs.Truncated,
	}
	if !rs.AsOf.IsZero() {
		out.AsOf = rs.AsOf.String()
	}
	// 列顺序取第一行的字段顺序——每一行的字段集合与顺序都由
	// ScreenQuery.OutputFields 统一决定，所以任取一行都一样。
	if len(rs.Results) > 0 {
		for _, fv := range rs.Results[0].Fields {
			spec, _ := fv.Field.Spec()
			out.Columns = append(out.Columns, columnView{
				Field: fv.Field.String(), Label: fv.Field.Label(), Unit: spec.Unit,
			})
		}
	}
	for _, r := range rs.Results {
		row := resultRowView{
			Symbol: r.Code.FullSymbol(),
			Market: r.Market().String(),
			Name:   r.Name,
			Values: make(map[string]any, len(r.Fields)),
		}
		for _, fv := range r.Fields {
			// 缺失渲染成 null 而不是 0：一只刚上市、财报还没披露的票，
			// ROE 是「没有」，不是「0%」。口径判定在值对象里，这里只负责渲染。
			switch {
			case !fv.Present:
				row.Values[fv.Field.String()] = nil
			case fv.IsNumeric():
				// 以字符串输出，理由同其它上下文：JSON 数字在前端就是 float64。
				row.Values[fv.Field.String()] = fv.Number.String()
			default:
				row.Values[fv.Field.String()] = fv.Text
			}
		}
		out.Items = append(out.Items, row)
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (r templateRequest) toInput() domain_services.TemplateInput {
	return domain_services.TemplateInput{
		Name:          r.Name,
		Description:   r.Description,
		Criteria:      toCriterionInputs(r.Criteria),
		SortField:     r.SortField,
		SortDirection: r.SortDirection,
		Limit:         r.Limit,
		IsPublic:      r.IsPublic,
	}
}

func toCriterionInputs(reqs []criterionRequest) []domain_services.CriterionInput {
	out := make([]domain_services.CriterionInput, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, domain_services.CriterionInput{
			Field: r.Field, Operator: r.Operator, Values: r.Values,
		})
	}
	return out
}

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *ScreeningHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
	if h.resolve == nil {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return domain_services.Operator{}, false
	}
	op, ok := h.resolve(c)
	if !ok || op.UserID == 0 {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return domain_services.Operator{}, false
	}
	return op, true
}

func parseUintParam(c *gin.Context, name string) (uint64, error) {
	raw := c.Param(name)
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, custom_errors.Invalid("参数 %s 必须是正整数: %s", name, raw)
	}
	return v, nil
}

// queryInt 解析可选的整型查询参数；缺失或非法一律回 0，
// 由 shared_vo.NewPage 去收敛成合法分页。
func queryInt(c *gin.Context, name string) int {
	v, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return 0
	}
	return v
}
