// Package http_handlers 把报告上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：报告上下文不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type ReportHandler struct {
	reportService *domain_services.ReportService
	resolve       OperatorResolver
}

func NewReportHandler(reportService *domain_services.ReportService, resolve OperatorResolver) *ReportHandler {
	return &ReportHandler{reportService: reportService, resolve: resolve}
}

func (h *ReportHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/reports", authRequired)
	g.GET("", h.ListMine)
	g.GET("/:id", h.Get)
	g.GET("/:id/sections/:key", h.GetSection)
	g.DELETE("/:id", h.Delete)
}

// 报告没有创建接口：它由 OnTaskCompleted 事件生成，不接受外部直接构造。
// 开一个 POST /reports 等于让调用方绕过分析结论自己编一份报告出来。

// ListMine 列出我的报告。
//
// @Summary  我的报告列表
// @Tags     分析报告
// @Produce  json
// @Security BearerAuth
// @Param    userId   query    integer false "只有管理员传得动：查看指定用户的报告，留空即查自己"
// @Param    page     query    integer false "页码，默认 1"
// @Param    pageSize query    integer false "每页条数，默认 20"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]reportSummaryView}}
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Failure  403      {object} response.Envelope "无权查看其他用户的分析报告"
// @Router   /reports [get]
func (h *ReportHandler) ListMine(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	// userId 只有管理员传得动；服务层会判权限，这里不重复判。
	targetUserID, _ := strconv.ParseUint(c.Query("userId"), 10, 64)

	reports, total, err := h.reportService.ListByUser(c.Request.Context(), op, targetUserID, page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toReportSummaryViews(reports), total, page.Number, page.Size)
}

// Get 取单份报告的完整内容。
//
// 文档里没有 403，这不是漏写：读到别人的报告时服务层返回的是 NotFound 而不是
// Forbidden（见 domain_services.requireOwnership），否则状态码差异本身就会变成
// 一条可以枚举报告 ID 的信道。文档必须和这个决定保持一致，不然等于把它写回去。
//
// @Summary  报告详情
// @Tags     分析报告
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "报告 ID"
// @Success  200 {object} response.Envelope{data=reportView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "报告不存在或不属于当前用户"
// @Router   /reports/{id} [get]
func (h *ReportHandler) Get(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	report, err := h.reportService.Get(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toReportView(report))
}

// GetSection 单独取一节，供前端按需加载长报告的某一段。
//
// @Summary  报告章节
// @Tags     分析报告
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "报告 ID"
// @Param    key path     string true "章节标识" example(conclusion)
// @Success  200 {object} response.Envelope{data=sectionView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "报告不存在、不属于当前用户，或该章节不存在"
// @Router   /reports/{id}/sections/{key} [get]
func (h *ReportHandler) GetSection(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	key := value_objects.SectionKey(c.Param("key"))
	section, err := h.reportService.GetSection(c.Request.Context(), op, c.Param("id"), key)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toSectionView(section))
}

// Delete 删除一份报告。
//
// @Summary  删除报告
// @Tags     分析报告
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "报告 ID"
// @Success  200 {object} response.Envelope{data=deleteView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "报告不存在或不属于当前用户"
// @Router   /reports/{id} [delete]
func (h *ReportHandler) Delete(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.reportService.Delete(c.Request.Context(), op, c.Param("id")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deleteView{Deleted: true})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// deleteView 删除结果。
//
// 一个布尔值本可以用 gin.H 写完，但那样它就不在 OpenAPI 里——
// 响应体的形状只要没有类型，文档就只能靠人手写，而手写的那份迟早和代码分家。
type deleteView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name report.DeleteView

type sectionView struct {
	Key     string `json:"key" example:"conclusion"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Order   int    `json:"order"`
} // @name report.SectionView

// toSectionView 照抄落库时固化的 Order，不按数组下标现编。
// 下标会随「某一节内容为空被丢弃」而错位，固化值才是报告真正的排布。
func toSectionView(s value_objects.Section) sectionView {
	return sectionView{
		Key:     s.Key.String(),
		Title:   s.Title,
		Content: s.Content,
		Order:   s.Order,
	}
}

// sectionOutlineView 是不带正文的章节目录项，用于列表页。
type sectionOutlineView struct {
	Key   string `json:"key" example:"conclusion"`
	Title string `json:"title"`
	Order int    `json:"order"`
} // @name report.SectionOutlineView

type reportView struct {
	ID         string        `json:"id" example:"rpt_20260917_a1b2c3d4e5f6"`
	UserID     uint64        `json:"userId"`
	TaskID     string        `json:"taskId" example:"task_20260917_a1b2c3d4e5f6"`
	Symbol     string        `json:"symbol" example:"600519.SH"`
	Market     string        `json:"market" example:"CN"`
	TradeDate  string        `json:"tradeDate" example:"2026-09-17"`
	Title      string        `json:"title"`
	Summary    string        `json:"summary"`
	Sections   []sectionView `json:"sections"`
	Action     string        `json:"action" enums:"buy,hold,sell,reduce,increase,undecided"`
	ActionText string        `json:"actionText"`
	// 下面这组直接读聚合里固化的数字。这里刻意不写 Confidence*100 之类的换算，
	// 也不按 TargetPrice/StopLoss 现算盈亏比：派生量是生成当时算好并落库的事实，
	// 接口层重算会让同一份报告每刷新一次都可能给出不同的数字。
	Confidence  string    `json:"confidence"`
	RiskScore   string    `json:"riskScore"`
	TargetPrice string    `json:"targetPrice"`
	StopLoss    string    `json:"stopLoss"`
	Position    string    `json:"position"`
	CreatedAt   time.Time `json:"createdAt"`
} // @name report.ReportView

func toReportView(r *entities.Report) *reportView {
	if r == nil {
		return nil
	}
	sections := make([]sectionView, 0, len(r.Sections))
	for _, s := range r.Sections {
		sections = append(sections, toSectionView(s))
	}
	return &reportView{
		ID:          r.ID,
		UserID:      r.UserID,
		TaskID:      r.TaskID,
		Symbol:      r.Symbol.FullSymbol(),
		Market:      string(r.Symbol.Market),
		TradeDate:   r.TradeDate.String(),
		Title:       r.Title,
		Summary:     r.Summary,
		Sections:    sections,
		Action:      r.Action.String(),
		ActionText:  r.Action.DisplayName(),
		Confidence:  decimalx.FormatRatio(r.Confidence),
		RiskScore:   decimalx.FormatPercent(r.RiskScore),
		TargetPrice: decimalx.FormatPrice(r.TargetPrice),
		StopLoss:    decimalx.FormatPrice(r.StopLoss),
		Position:    decimalx.FormatPercent(r.Position),
		CreatedAt:   r.CreatedAt,
	}
}

// reportSummaryView 是列表项：只给目录，不给正文。
//
// 一份报告十几段 markdown，列表页把正文一并吐出去，一页二十条就是几兆的响应体，
// 而列表根本不展示正文。这是视图裁剪，不是另一套读模型——数据仍然来自同一个聚合。
type reportSummaryView struct {
	ID         string               `json:"id" example:"rpt_20260917_a1b2c3d4e5f6"`
	UserID     uint64               `json:"userId"`
	TaskID     string               `json:"taskId" example:"task_20260917_a1b2c3d4e5f6"`
	Symbol     string               `json:"symbol" example:"600519.SH"`
	Market     string               `json:"market" example:"CN"`
	TradeDate  string               `json:"tradeDate" example:"2026-09-17"`
	Title      string               `json:"title"`
	Summary    string               `json:"summary"`
	Outline    []sectionOutlineView `json:"outline"`
	Action     string               `json:"action" enums:"buy,hold,sell,reduce,increase,undecided"`
	ActionText string               `json:"actionText"`
	Confidence string               `json:"confidence"`
	RiskScore  string               `json:"riskScore"`
	CreatedAt  time.Time            `json:"createdAt"`
} // @name report.ReportSummaryView

func toReportSummaryView(r *entities.Report) *reportSummaryView {
	if r == nil {
		return nil
	}
	outline := make([]sectionOutlineView, 0, len(r.Sections))
	for _, s := range r.Sections {
		outline = append(outline, sectionOutlineView{
			Key:   s.Key.String(),
			Title: s.Title,
			Order: s.Order,
		})
	}
	return &reportSummaryView{
		ID:         r.ID,
		UserID:     r.UserID,
		TaskID:     r.TaskID,
		Symbol:     r.Symbol.FullSymbol(),
		Market:     string(r.Symbol.Market),
		TradeDate:  r.TradeDate.String(),
		Title:      r.Title,
		Summary:    r.Summary,
		Outline:    outline,
		Action:     r.Action.String(),
		ActionText: r.Action.DisplayName(),
		Confidence: decimalx.FormatRatio(r.Confidence),
		RiskScore:  decimalx.FormatPercent(r.RiskScore),
		CreatedAt:  r.CreatedAt,
	}
}

func toReportSummaryViews(reports []*entities.Report) []*reportSummaryView {
	out := make([]*reportSummaryView, 0, len(reports))
	for _, r := range reports {
		out = append(out, toReportSummaryView(r))
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *ReportHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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

// parsePage 容忍垃圾查询参数，回落到默认值：
// 一个写错的页码不值得让一次读请求整体失败。
func parsePage(c *gin.Context) shared_vo.Page {
	num, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return shared_vo.NewPage(num, size)
}
