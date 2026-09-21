package http_handlers

import (
	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// SyncOperatorResolver 从请求上下文取出调用者身份。
// 注入而非直接读 identity 的 Claims：stock 上下文不该在编译期依赖身份上下文。
type SyncOperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type SyncHandler struct {
	syncService *domain_services.SyncService
	resolve     SyncOperatorResolver
}

func NewSyncHandler(syncService *domain_services.SyncService, resolve SyncOperatorResolver) *SyncHandler {
	return &SyncHandler{syncService: syncService, resolve: resolve}
}

func (h *SyncHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/market-sync", authRequired)
	g.POST("/runs", h.Trigger)
	g.GET("/runs", h.History)
	g.GET("/runs/:id", h.GetRun)
	g.GET("/latest", h.Latest)
}

type triggerSyncRequest struct {
	Kind   string `json:"kind" binding:"required" example:"quotes"`
	Market string `json:"market" binding:"required" example:"CN"`
} // @name stock.TriggerSyncRequest

// Trigger 手动触发一次同步。
//
// 同步是同步执行的（可能几十分钟），因此这个接口只适合小范围类型；
// 常规全量同步应当由定时任务驱动，接口层不做后台化——
// 在 handler 里起后台任务会绕过队列与并发配额，且进程重启即丢失。
//
// @Summary  手动触发同步
// @Tags     数据同步
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     triggerSyncRequest true "同步类型与市场"
// @Success  200  {object} response.Envelope{data=syncRunView}
// @Failure  400  {object} response.Envelope "请求体不合法，或同步类型、市场未知"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "需要管理员权限"
// @Failure  404  {object} response.Envelope "该市场没有可同步的标的，请先同步股票列表"
// @Failure  409  {object} response.Envelope "同类型同市场已有同步任务在运行"
// @Failure  503  {object} response.Envelope "没有数据源支持该市场，或所有数据源取数失败"
// @Router   /market-sync/runs [post]
func (h *SyncHandler) Trigger(c *gin.Context) {
	op, ok := h.resolve(c)
	if !ok {
		response.Fail(c, custom_errors.Unauthorized("未登录"))
		return
	}
	var req triggerSyncRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	run, err := h.syncService.Trigger(c.Request.Context(), &op, req.Kind, req.Market)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toSyncRunView(run))
}

// History 分页查询同步历史。kind 与 market 都是可选过滤条件。
//
// @Summary  查询同步历史
// @Tags     数据同步
// @Produce  json
// @Security BearerAuth
// @Param    kind     query    string false "同步类型 stock_list/quotes/klines/financials/news，留空表示不限"
// @Param    market   query    string false "市场（CN/HK/US），留空表示不限"
// @Param    page     query    int    false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int    false "每页条数，默认 20，上限 200"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]syncRunView}}
// @Failure  400      {object} response.Envelope "同步类型或市场非法"
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Router   /market-sync/runs [get]
func (h *SyncHandler) History(c *gin.Context) {
	page := parsePage(c)
	runs, total, err := h.syncService.History(c.Request.Context(), c.Query("kind"), c.Query("market"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]*syncRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, toSyncRunView(r))
	}
	response.OKPage(c, views, total, page.Number, page.Size)
}

// GetRun 按 ID 查单次同步记录，用于轮询进度与断点。
//
// @Summary  查询单次同步记录
// @Tags     数据同步
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "同步记录 ID"
// @Success  200 {object} response.Envelope{data=syncRunView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "同步记录不存在"
// @Router   /market-sync/runs/{id} [get]
func (h *SyncHandler) GetRun(c *gin.Context) {
	run, err := h.syncService.GetRun(c.Request.Context(), c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toSyncRunView(run))
}

// Latest 取某类型某市场最近一次同步，供运维面板判断数据新鲜度。
//
// 与 History 不同，这里的 kind 与 market 都是必填：值对象不接受空串，
// 缺任一个都会在领域服务里变成 400 而不是「不限」。
//
// @Summary  查询最近一次同步
// @Tags     数据同步
// @Produce  json
// @Security BearerAuth
// @Param    kind   query    string true "同步类型 stock_list/quotes/klines/financials/news"
// @Param    market query    string true "市场（CN/HK/US）"
// @Success  200    {object} response.Envelope{data=syncRunView}
// @Failure  400    {object} response.Envelope "同步类型或市场缺失、非法"
// @Failure  401    {object} response.Envelope "未登录或令牌无效"
// @Failure  404    {object} response.Envelope "尚无该类型该市场的同步记录"
// @Router   /market-sync/latest [get]
func (h *SyncHandler) Latest(c *gin.Context) {
	run, err := h.syncService.LatestOf(c.Request.Context(), c.Query("kind"), c.Query("market"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toSyncRunView(run))
}

type syncRunView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" example:"quotes"`
	KindName string `json:"kindName" example:"行情快照"`
	Market   string `json:"market" example:"CN"`
	Status   string `json:"status" example:"succeeded"`
	Total    int    `json:"total"`
	// successRate 与 quote 里的比率一样是字符串：成功率是 decimal 算出来的，
	// 走 JSON 数字就会在前端退化成 float64。
	Succeeded   int    `json:"succeeded"`
	Failed      int    `json:"failed"`
	Skipped     int    `json:"skipped"`
	SuccessRate string `json:"successRate"`
	// cursor 是断点续传的游标，记录最后一个处理完的标的代码；
	// resumable 为 true 时可以从这里接着跑。
	Cursor      string `json:"cursor,omitempty"`
	Resumable   bool   `json:"resumable"`
	TriggeredBy string `json:"triggeredBy" example:"user:1"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt,omitempty"`
	DurationMS  int64  `json:"durationMs"`
	Error       string `json:"error,omitempty"`
} // @name stock.SyncRunView

func toSyncRunView(r *entities.SyncRun) *syncRunView {
	if r == nil {
		return nil
	}
	v := &syncRunView{
		ID:       r.ID,
		Kind:     r.Kind.String(),
		KindName: r.Kind.DisplayName(),
		Market:   r.Market.String(),
		Status:   r.Status.String(),
		Total:    r.Stats.Total,
		// 成功率直接读存量，不在这里用 Succeeded/Total 重算——
		// 乘除派生值以落库那一刻为准。
		Succeeded:   r.Stats.Succeeded,
		Failed:      r.Stats.Failed,
		Skipped:     r.Stats.Skipped,
		SuccessRate: decimalx.FormatPercent(r.Stats.SuccessRate),
		Cursor:      r.Cursor,
		Resumable:   r.Resumable(),
		TriggeredBy: r.TriggeredBy,
		StartedAt:   r.StartedAt.Format("2006-01-02 15:04:05"),
		DurationMS:  r.DurationMS,
		Error:       r.Error,
	}
	if r.FinishedAt != nil {
		v.FinishedAt = r.FinishedAt.Format("2006-01-02 15:04:05")
	}
	return v
}
