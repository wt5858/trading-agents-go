// Package http_handlers 把分析上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
package http_handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：分析上下文不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

// sseKeepAlive 是 SSE 心跳间隔。
// 没有心跳的话，一个长时间没有进度推进的任务会让中间的反向代理认为连接已死并掐断。
const sseKeepAlive = 20 * time.Second

type AnalysisHandler struct {
	analysisService *domain_services.AnalysisService
	batchService    *domain_services.BatchService
	resolve         OperatorResolver
}

func NewAnalysisHandler(
	analysisService *domain_services.AnalysisService,
	batchService *domain_services.BatchService,
	resolve OperatorResolver,
) *AnalysisHandler {
	return &AnalysisHandler{
		analysisService: analysisService,
		batchService:    batchService,
		resolve:         resolve,
	}
}

func (h *AnalysisHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/analysis", authRequired)
	g.POST("/tasks", h.Submit)
	g.GET("/tasks", h.ListMine)
	g.GET("/tasks/:id", h.GetTask)
	g.GET("/tasks/:id/result", h.GetResult)
	g.POST("/tasks/:id/cancel", h.Cancel)
	g.GET("/tasks/:id/progress", h.StreamProgress)
	g.POST("/batches", h.SubmitBatch)
	g.GET("/batches/:id", h.GetBatch)
}

// ---------------------------------------------------------------------------
// 提交
// ---------------------------------------------------------------------------

// submitRequest 提交单只股票的分析。
//
// 除 Code 外都可省略：市场能从代码推断，其余字段由领域层回落到默认值。
type submitRequest struct {
	Code      string   `json:"code" binding:"required" example:"600519"`
	Market    string   `json:"market" example:"CN"`
	TradeDate string   `json:"tradeDate" example:"2026-09-17"`
	Depth     int      `json:"depth" example:"3"`
	Analysts  []string `json:"analysts"`
	LLMModel  string   `json:"llmModel"`
} // @name analysis.SubmitRequest

// Submit 提交一次分析。
//
// @Summary  提交分析任务
// @Tags     分析任务
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     submitRequest true "分析请求，depth 取 1/3/5 分别对应快速、标准、深度"
// @Success  200  {object} response.Envelope{data=taskView}
// @Failure  400  {object} response.Envelope "请求参数不合法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  429  {object} response.Envelope "并发分析数已达上限"
// @Failure  500  {object} response.Envelope "任务落库或入队失败"
// @Router   /analysis/tasks [post]
func (h *AnalysisHandler) Submit(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	task, err := h.analysisService.Submit(c.Request.Context(), op, domain_services.SubmitInput{
		Code:      req.Code,
		Market:    req.Market,
		TradeDate: req.TradeDate,
		Depth:     req.Depth,
		Analysts:  req.Analysts,
		LLMModel:  req.LLMModel,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTaskView(task))
}

// submitBatchRequest 批量提交分析。Codes 之外的字段对批内所有任务生效。
type submitBatchRequest struct {
	Codes     []string `json:"codes" binding:"required" example:"600519,000001"`
	Market    string   `json:"market" example:"CN"`
	TradeDate string   `json:"tradeDate" example:"2026-09-17"`
	Depth     int      `json:"depth" example:"3"`
	Analysts  []string `json:"analysts"`
	LLMModel  string   `json:"llmModel"`
} // @name analysis.SubmitBatchRequest

// SubmitBatch 批量提交分析。
//
// @Summary  批量提交分析任务
// @Tags     分析任务
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     submitBatchRequest true "批量分析请求，单批次股票数量有上限"
// @Success  200  {object} response.Envelope{data=submitBatchView}
// @Failure  400  {object} response.Envelope "请求参数不合法或超出单批次上限"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  429  {object} response.Envelope "并发分析数已达上限"
// @Failure  500  {object} response.Envelope "批次落库或入队失败"
// @Router   /analysis/batches [post]
func (h *AnalysisHandler) SubmitBatch(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req submitBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	out, err := h.batchService.SubmitBatch(c.Request.Context(), op, domain_services.SubmitBatchInput{
		Codes:     req.Codes,
		Market:    req.Market,
		TradeDate: req.TradeDate,
		Depth:     req.Depth,
		Analysts:  req.Analysts,
		LLMModel:  req.LLMModel,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, submitBatchView{
		Batch: toBatchView(out.Batch),
		Tasks: toTaskViews(out.Tasks),
	})
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// GetTask 查询单个分析任务。
//
// @Summary  查询分析任务
// @Tags     分析任务
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "任务 ID"
// @Success  200 {object} response.Envelope{data=taskView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Router   /analysis/tasks/{id} [get]
func (h *AnalysisHandler) GetTask(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	task, err := h.analysisService.GetTask(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toTaskView(task))
}

// GetResult 查询分析任务的结论。
//
// 任务还没跑完时同样是 404：结果尚未产出与任务不存在在接口上不做区分，
// 避免任务 ID 空间被探测。
//
// @Summary  查询分析结果
// @Tags     分析任务
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "任务 ID"
// @Success  200 {object} response.Envelope{data=resultView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "任务不存在或尚未产出结果"
// @Router   /analysis/tasks/{id}/result [get]
func (h *AnalysisHandler) GetResult(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	result, err := h.analysisService.GetResult(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toResultView(result))
}

// ListMine 分页列出当前用户的分析任务。
//
// @Summary  分析任务列表
// @Tags     分析任务
// @Produce  json
// @Security BearerAuth
// @Param    userId   query    integer false "目标用户 ID，仅管理员可传，缺省为自己"
// @Param    status   query    string  false "任务状态过滤，留空表示不限" Enums(queued, running, completed, failed, canceled)
// @Param    page     query    integer false "页码，默认 1"
// @Param    pageSize query    integer false "每页条数，默认 20"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]taskView}}
// @Failure  400      {object} response.Envelope "状态取值非法"
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Failure  403      {object} response.Envelope "无权查看其他用户的分析任务"
// @Router   /analysis/tasks [get]
func (h *AnalysisHandler) ListMine(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	// userId 只有管理员传得动；服务层会判权限，这里不重复判。
	targetUserID, _ := strconv.ParseUint(c.Query("userId"), 10, 64)
	tasks, total, err := h.analysisService.ListByUser(
		c.Request.Context(), op, targetUserID, c.Query("status"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toTaskViews(tasks), total, page.Number, page.Size)
}

// GetBatch 查询批次进度。
//
// @Summary  查询分析批次
// @Tags     分析任务
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "批次 ID"
// @Success  200 {object} response.Envelope{data=batchView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "批次不存在"
// @Router   /analysis/batches/{id} [get]
func (h *AnalysisHandler) GetBatch(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	batch, err := h.batchService.GetBatch(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toBatchView(batch))
}

// Cancel 取消尚未终结的分析任务。
//
// @Summary  取消分析任务
// @Tags     分析任务
// @Produce  json
// @Security BearerAuth
// @Param    id path string true "任务 ID"
// @Success  200 {object} response.Envelope{data=cancelView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Failure  409 {object} response.Envelope "任务已处于终态或被并发改写"
// @Router   /analysis/tasks/{id}/cancel [post]
func (h *AnalysisHandler) Cancel(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.analysisService.Cancel(c.Request.Context(), op, c.Param("id")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, cancelView{Canceled: true})
}

// ---------------------------------------------------------------------------
// SSE 进度流
// ---------------------------------------------------------------------------

// StreamProgress 以 SSE 推送任务进度。
//
// # 为什么这里没有 go func()
//
// 领域服务把「当前快照」和「后续增量」分开返回，因此本方法可以在自己的协程里
// 先写首帧、再同步地一帧一帧拉。整条链路不需要任何额外协程，也就不存在
// 「客户端断开后有个协程还在跑」这种泄漏。
//
// 每一帧的负载仍然是统一信封：SSE 只是传输方式，不该成为绕开响应约定的借口。
//
// @Summary     订阅任务进度（SSE）
// @Description 以 Server-Sent Events 推送任务进度，直到任务终结或客户端断开。
// @Description 正常帧为 `event: progress`，data 是统一信封且 data 字段为 progressView；
// @Description 出错帧为 `event: error`，信封的 data 为 null。
// @Description 长时间没有新进度时服务端发一行 `: keep-alive` 注释保活，客户端应忽略。
// @Tags        分析任务
// @Produce     text/event-stream
// @Security    BearerAuth
// @Param       id  path     string true "任务 ID"
// @Success     200 {string} string "SSE 事件流，每条形如 data: {...}\n\n"
// @Failure     401 {object} response.Envelope "未登录或令牌无效"
// @Failure     404 {object} response.Envelope "任务不存在"
// @Failure     500 {object} response.Envelope "订阅进度流失败"
// @Router      /analysis/tasks/{id}/progress [get]
func (h *AnalysisHandler) StreamProgress(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	taskID := c.Param("id")

	sub, err := h.analysisService.SubscribeProgress(ctx, op, taskID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer func() { _ = sub.Stream.Close() }()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	// 关掉 nginx 的缓冲，否则进度会被攒着一次性吐出来，实时性全无。
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	if !writeProgressEvent(c, sub.Snapshot) {
		return
	}
	// 任务已经收场（完成 / 失败 / 取消）就不必挂着连接等一个永远不会来的更新。
	if sub.Terminal || progressFinished(sub.Snapshot) {
		return
	}

	for {
		// Next 会一直阻塞在 Redis 订阅上。用一个带超时的派生 ctx 把它切成
		// 不超过心跳周期的片段，这样「长时间没有新进度」也能定期发一个保活注释行，
		// 而不需要再起一个协程专门管心跳。
		frameCtx, cancelFrame := context.WithTimeout(ctx, sseKeepAlive)
		prog, alive, err := sub.Stream.Next(frameCtx)
		timedOut := frameCtx.Err() != nil
		cancelFrame()

		if err != nil {
			writeErrorEvent(c, err)
			return
		}
		// 客户端断开或服务停机。
		if ctx.Err() != nil {
			return
		}
		if !alive {
			if !timedOut {
				// 订阅本身断了（Redis 连接关闭），没什么可等的了。
				return
			}
			if !writeKeepAlive(c) {
				return
			}
			continue
		}
		if !writeProgressEvent(c, prog) {
			return
		}
		if progressFinished(prog) {
			return
		}
	}
}

func progressFinished(p value_objects.Progress) bool {
	return p.TotalSteps > 0 && p.DoneSteps >= p.TotalSteps
}

func writeProgressEvent(c *gin.Context, p value_objects.Progress) bool {
	payload, err := json.Marshal(response.Envelope{
		Code:    response.CodeOK,
		Message: "ok",
		Data:    toProgressView(p),
	})
	if err != nil {
		return false
	}
	return writeSSE(c, "progress", payload)
}

func writeErrorEvent(c *gin.Context, err error) {
	payload, mErr := json.Marshal(response.Envelope{
		Code:    response.CodeInternal,
		Message: custom_errors.MessageOf(err),
		Data:    nil,
	})
	if mErr != nil {
		return
	}
	writeSSE(c, "error", payload)
}

func writeSSE(c *gin.Context, event string, payload []byte) bool {
	if _, err := c.Writer.WriteString("event: " + event + "\ndata: "); err != nil {
		return false
	}
	if _, err := c.Writer.Write(payload); err != nil {
		return false
	}
	if _, err := c.Writer.WriteString("\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// writeKeepAlive 发一个 SSE 注释行。客户端会忽略它，中间代理却据此认为连接仍活着。
func writeKeepAlive(c *gin.Context) bool {
	if _, err := c.Writer.WriteString(": keep-alive\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type taskView struct {
	ID         string        `json:"id" example:"task_20260917_a1b2c3d4e5f6"`
	UserID     uint64        `json:"userId"`
	BatchID    string        `json:"batchId,omitempty"`
	Symbol     string        `json:"symbol" example:"600519.SH"`
	Market     string        `json:"market" example:"CN"`
	TradeDate  string        `json:"tradeDate" example:"2026-09-17"`
	Depth      int           `json:"depth" example:"3"`
	Analysts   []string      `json:"analysts"`
	Status     string        `json:"status" example:"running"`
	StatusText string        `json:"statusText"`
	Progress   *progressView `json:"progress,omitempty"`
	Error      string        `json:"error,omitempty"`
	Attempts   int           `json:"attempts"`
	CreatedAt  time.Time     `json:"createdAt"`
	StartedAt  *time.Time    `json:"startedAt,omitempty"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
} // @name analysis.TaskView

func toTaskView(t *entities.Task) *taskView {
	if t == nil {
		return nil
	}
	return &taskView{
		ID:         t.ID,
		UserID:     t.UserID,
		BatchID:    t.BatchID,
		Symbol:     t.Request.Code.FullSymbol(),
		Market:     string(t.Request.Code.Market),
		TradeDate:  t.Request.TradeDate.String(),
		Depth:      t.Request.Depth.Int(),
		Analysts:   t.Request.AnalystIDs(),
		Status:     t.Status.String(),
		StatusText: t.Status.DisplayName(),
		Progress:   toProgressView(t.Progress),
		Error:      t.ErrMsg,
		Attempts:   t.Attempts,
		CreatedAt:  t.CreatedAt,
		StartedAt:  t.StartedAt,
		FinishedAt: t.FinishedAt,
	}
}

func toTaskViews(tasks []*entities.Task) []*taskView {
	out := make([]*taskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toTaskView(t))
	}
	return out
}

// progressView 直接照搬聚合里固化好的派生量。
//
// 这里刻意不写 float64(done)/float64(total)*100，也不写 time.Since(startedAt)：
// 百分比与剩余秒数是写入当时算好并落库的事实，接口层重算会让同一条记录
// 每刷新一次给出不同的数字，与 SSE 推送的值也会对不上。
type progressView struct {
	Steps      []value_objects.Step `json:"steps"`
	CurrentIdx int                  `json:"currentIndex"`
	DoneSteps  int                  `json:"doneSteps"`
	TotalSteps int                  `json:"totalSteps"`
	Percent    string               `json:"percent" example:"42.50"`
	ETASeconds int64                `json:"etaSeconds"`
	Message    string               `json:"message"`
	UpdatedAt  time.Time            `json:"updatedAt"`
} // @name analysis.ProgressView

func toProgressView(p value_objects.Progress) *progressView {
	if p.IsZero() {
		return nil
	}
	return &progressView{
		Steps:      p.Steps,
		CurrentIdx: p.CurrentIdx,
		DoneSteps:  p.DoneSteps,
		TotalSteps: p.TotalSteps,
		Percent:    decimalx.FormatPercent(p.Percent),
		ETASeconds: p.ETASeconds,
		Message:    p.Message,
		UpdatedAt:  p.UpdatedAt,
	}
}

type batchView struct {
	ID        string    `json:"id" example:"batch_20260917_a1b2c3d4e5f6"`
	UserID    uint64    `json:"userId"`
	TaskIDs   []string  `json:"taskIds"`
	Total     int       `json:"total"`
	Completed int       `json:"completed"`
	Failed    int       `json:"failed"`
	Percent   string    `json:"percent" example:"42.50"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
} // @name analysis.BatchView

// submitBatchView 批量提交的产出：批次本身 + 展开的子任务。
//
// 本可以直接 gin.H 拼一个两字段的 map，但那样这个响应体在 OpenAPI 里就是
// 一个没有形状的 object，调用方只能靠读源码对接——文档与代码迟早分家。
type submitBatchView struct {
	Batch *batchView  `json:"batch"`
	Tasks []*taskView `json:"tasks"`
} // @name analysis.SubmitBatchView

// cancelView 取消结果。同样为了让这个响应体在文档里有形状而存在。
type cancelView struct {
	Canceled bool `json:"canceled" example:"true"`
} // @name analysis.CancelView

func toBatchView(b *entities.Batch) *batchView {
	if b == nil {
		return nil
	}
	return &batchView{
		ID:        b.ID,
		UserID:    b.UserID,
		TaskIDs:   b.TaskIDs,
		Total:     b.Total,
		Completed: b.Completed,
		Failed:    b.Failed,
		// 同样直接读固化值，不用 (completed+failed)/total 重算。
		Percent:   decimalx.FormatPercent(b.Percent),
		Done:      b.Done(),
		CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt,
	}
}

type resultView struct {
	Symbol    string                       `json:"symbol" example:"600519.SH"`
	TradeDate string                       `json:"tradeDate" example:"2026-09-17"`
	Decision  value_objects.Decision       `json:"decision"`
	Action    string                       `json:"actionText"`
	Reports   map[string]string            `json:"reports"`
	Usage     value_objects.TokenUsage     `json:"usage"`
	Phases    []value_objects.PhaseOutcome `json:"phases"`
	CreatedAt time.Time                    `json:"createdAt"`
} // @name analysis.ResultView

func toResultView(r *value_objects.Result) *resultView {
	if r == nil {
		return nil
	}
	return &resultView{
		Symbol:    r.Code.FullSymbol(),
		TradeDate: r.TradeDate.String(),
		Decision:  r.Decision,
		Action:    r.Decision.Action.DisplayName(),
		Reports:   r.Reports,
		Usage:     r.Usage,
		Phases:    r.Phases,
		CreatedAt: r.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *AnalysisHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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
