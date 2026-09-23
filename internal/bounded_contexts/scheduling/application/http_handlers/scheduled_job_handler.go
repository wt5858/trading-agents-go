// Package http_handlers 把定时任务上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
// 也没有任何权限判定：谁能做什么由 domain_services 的 requireAdmin 统一裁决，
// 在两个地方各判一次，迟早会出现两处规则不一致。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：定时任务上下文不该在
// 编译期依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

// defaultPreviewCount 是 cron 预览的默认条数。
const defaultPreviewCount = 5

type ScheduledJobHandler struct {
	scheduler *domain_services.SchedulerService
	resolve   OperatorResolver
}

func NewScheduledJobHandler(
	scheduler *domain_services.SchedulerService,
	resolve OperatorResolver,
) *ScheduledJobHandler {
	return &ScheduledJobHandler{scheduler: scheduler, resolve: resolve}
}

func (h *ScheduledJobHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/scheduling", authRequired)
	g.GET("/jobs", h.List)
	g.POST("/jobs", h.Create)
	// preview-cron 放在 /jobs/:id 之前注册无妨：gin 的路由树里静态段优先于参数段，
	// 不会被 :id 吃掉。
	g.POST("/jobs/preview-cron", h.PreviewCron)
	g.GET("/jobs/:id", h.Get)
	g.PUT("/jobs/:id", h.Update)
	g.DELETE("/jobs/:id", h.Delete)
	g.POST("/jobs/:id/pause", h.Pause)
	g.POST("/jobs/:id/resume", h.Resume)
	g.POST("/jobs/:id/trigger", h.TriggerNow)
	g.GET("/jobs/:id/executions", h.ListExecutions)
}

// ---------------------------------------------------------------------------
// 任务管理
// ---------------------------------------------------------------------------

// List 分页查询定时任务，登录即可。
//
// @Summary  查询定时任务列表
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    kind     query    string false "任务种类，留空表示不限" Enums(market_sync, scheduled_analysis, data_cleanup)
// @Param    status   query    string false "任务状态，留空表示不限" Enums(enabled, paused, disabled)
// @Param    page     query    int    false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int    false "每页条数，默认 20"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]jobView}}
// @Failure  400      {object} response.Envelope "任务种类或状态取值非法"
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Router   /scheduling/jobs [get]
func (h *ScheduledJobHandler) List(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	jobs, total, err := h.scheduler.ListJobs(
		c.Request.Context(), op, c.Query("kind"), c.Query("status"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toJobViews(jobs), total, page.Number, page.Size)
}

// Get 取单条定时任务，登录即可。
//
// @Summary  查询单条定时任务
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "任务 ID"
// @Success  200 {object} response.Envelope{data=jobView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Router   /scheduling/jobs/{id} [get]
func (h *ScheduledJobHandler) Get(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	job, err := h.scheduler.GetJob(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toJobView(job))
}

type createJobRequest struct {
	Name string `json:"name" binding:"required"`
	Kind string `json:"kind" binding:"required" enums:"market_sync,scheduled_analysis,data_cleanup"`
	// Cron 是 5 段标准表达式（也接受 @daily 这类描述符），不是 6 段带秒的写法——
	// 例子里多一段秒，是这个接口最常见的 400。
	Cron string `json:"cron" binding:"required" example:"30 9 * * *"`
	// Payload 原样透传给运行器，接口层不解释它的结构——那是运行器的语义。
	Payload                map[string]any `json:"payload"`
	TimeoutSeconds         int            `json:"timeoutSeconds"`
	MaxConsecutiveFailures int            `json:"maxConsecutiveFailures"`
} // @name scheduling.CreateJobRequest

// Create 新建一条定时任务，仅管理员。
//
// @Summary  创建定时任务
// @Tags     定时任务
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     createJobRequest true "任务定义"
// @Success  200  {object} response.Envelope{data=jobView}
// @Failure  400  {object} response.Envelope "参数不合法，含非法的 cron 表达式、任务种类或超时上限"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  409  {object} response.Envelope "同名任务已存在"
// @Router   /scheduling/jobs [post]
func (h *ScheduledJobHandler) Create(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req createJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailBind(c, err)
		return
	}
	job, err := h.scheduler.CreateJob(c.Request.Context(), op, domain_services.CreateJobInput{
		Name:                   req.Name,
		Kind:                   req.Kind,
		Cron:                   req.Cron,
		Payload:                req.Payload,
		TimeoutSeconds:         req.TimeoutSeconds,
		MaxConsecutiveFailures: req.MaxConsecutiveFailures,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toJobView(job))
}

// updateJobRequest 用指针接 Payload，以区分「没传这一项」和「传了一个空对象」。
// 用值类型的话，把参数清空这个操作根本表达不出来。
type updateJobRequest struct {
	Name                   string          `json:"name"`
	Cron                   string          `json:"cron" example:"30 9 * * *"`
	Payload                *map[string]any `json:"payload"`
	TimeoutSeconds         int             `json:"timeoutSeconds"`
	MaxConsecutiveFailures int             `json:"maxConsecutiveFailures"`
} // @name scheduling.UpdateJobRequest

// Update 修改任务定义与调度，仅管理员。字段留空表示不改这一项。
//
// @Summary  修改定时任务
// @Tags     定时任务
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     string           true "任务 ID"
// @Param    body body     updateJobRequest true "要修改的字段，留空的字段保持不变"
// @Success  200  {object} response.Envelope{data=jobView}
// @Failure  400  {object} response.Envelope "参数不合法，含非法的 cron 表达式或超出上限的超时"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  403  {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  404  {object} response.Envelope "任务不存在"
// @Failure  409  {object} response.Envelope "任务已停用，不允许修改调度或配置"
// @Router   /scheduling/jobs/{id} [put]
func (h *ScheduledJobHandler) Update(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req updateJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailBind(c, err)
		return
	}
	in := domain_services.UpdateJobInput{
		Name:                   req.Name,
		Cron:                   req.Cron,
		TimeoutSeconds:         req.TimeoutSeconds,
		MaxConsecutiveFailures: req.MaxConsecutiveFailures,
	}
	if req.Payload != nil {
		in.Payload = *req.Payload
		in.HasPayload = true
	}
	job, err := h.scheduler.UpdateJob(c.Request.Context(), op, c.Param("id"), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toJobView(job))
}

// Delete 删除任务，仅管理员。执行历史保留，见仓储层注释。
//
// @Summary  删除定时任务
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "任务 ID"
// @Success  200 {object} response.Envelope{data=deleteJobView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Router   /scheduling/jobs/{id} [delete]
func (h *ScheduledJobHandler) Delete(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.scheduler.DeleteJob(c.Request.Context(), op, c.Param("id")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deleteJobView{Deleted: true})
}

// Pause 暂停任务，仅管理员。
//
// @Summary  暂停定时任务
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "任务 ID"
// @Success  200 {object} response.Envelope{data=pauseJobView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Failure  409 {object} response.Envelope "任务已处于暂停或停用状态"
// @Router   /scheduling/jobs/{id}/pause [post]
func (h *ScheduledJobHandler) Pause(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.scheduler.PauseJob(c.Request.Context(), op, c.Param("id")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pauseJobView{Paused: true})
}

// Resume 恢复任务，仅管理员。恢复会清零连续失败计数并按 cron 重算下次执行时间。
//
// @Summary  恢复定时任务
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "任务 ID"
// @Success  200 {object} response.Envelope{data=resumeJobView}
// @Failure  400 {object} response.Envelope "任务的 cron 表达式已不可用，无法恢复调度"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Failure  409 {object} response.Envelope "任务已处于启用状态，或已停用需先重新启用"
// @Router   /scheduling/jobs/{id}/resume [post]
func (h *ScheduledJobHandler) Resume(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	if err := h.scheduler.ResumeJob(c.Request.Context(), op, c.Param("id")); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, resumeJobView{Resumed: true})
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// TriggerNow 把这次触发排进队列后立刻返回，不等执行结束。
//
// 返回的是一条 queued 状态的执行记录，客户端拿它的 id 去
// GET /jobs/:id/executions 里看结局。
//
// # 为什么不再同步等待
//
// 因为执行已经搬到消费端了，这里根本没有可等的东西——真要等，就得轮询数据库
// 直到那条记录变成终态，而那只是把轮询从客户端挪到了服务端，还额外占着一条连接。
//
// 就算能等也不该等：一次行情同步跑十几分钟，同步返回意味着 HTTP 请求要挂那么久，
// 中间任何一次网关超时都会让管理员以为任务失败了，而它其实跑得好好的。
// 「让我看看它能不能跑通」这个需求，由执行历史里那条记录如实回答，
// 而且它连失败原因都记着——比一个超时的请求能给的信息多得多。
//
// @Summary  立即触发一次执行
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "任务 ID"
// @Success  200 {object} response.Envelope{data=executionView} "已排队的执行记录，status 为 queued"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  403 {object} response.Envelope "定时任务管理仅限管理员"
// @Failure  404 {object} response.Envelope "任务不存在"
// @Failure  409 {object} response.Envelope "任务已停用，不可手动触发"
// @Failure  503 {object} response.Envelope "执行记录已写下，但到期消息投递失败，等待恢复巡检补投"
// @Router   /scheduling/jobs/{id}/trigger [post]
func (h *ScheduledJobHandler) TriggerNow(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	exec, err := h.scheduler.TriggerNow(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toExecutionView(exec))
}

// ListExecutions 分页查询某任务的执行历史，登录即可。
//
// 不声明 404：执行历史按 job_id 查，任务删除后历史仍然保留（它是审计记录），
// 一个查不到任何记录的 id 只会得到空列表。
//
// @Summary  查询任务执行历史
// @Tags     定时任务
// @Produce  json
// @Security BearerAuth
// @Param    id       path     string true  "任务 ID"
// @Param    page     query    int    false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int    false "每页条数，默认 20"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]executionView}}
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Router   /scheduling/jobs/{id}/executions [get]
func (h *ScheduledJobHandler) ListExecutions(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	execs, total, err := h.scheduler.ListExecutions(c.Request.Context(), op, c.Param("id"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toExecutionViews(execs), total, page.Number, page.Size)
}

type previewCronRequest struct {
	// Cron 与创建接口同一套规则：5 段标准表达式，或 @daily 这类描述符。
	Cron string `json:"cron" binding:"required" example:"30 9 * * *"`
	// Count 是推演条数，不传或非正数按 5 条；上限 50 由值对象收敛。
	Count int `json:"count" example:"5"`
} // @name scheduling.PreviewCronRequest

// PreviewCron 让人在保存之前先看到真实的触发序列。
// 用 POST 而不是 GET：cron 表达式里全是 * 和空格，塞进 query string 需要转义，
// 而转义出错的表现是「预览结果和实际不符」，比报错更危险。
//
// 它不读库、不改任何状态，因此登录即可，不要求管理员——文档里没有 403 是有意的。
//
// @Summary  预览 cron 表达式的触发时刻
// @Tags     定时任务
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     previewCronRequest true "待推演的 cron 表达式与条数"
// @Success  200  {object} response.Envelope{data=cronPreviewView}
// @Failure  400  {object} response.Envelope "表达式为空、语法非法，或在未来不会再触发"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Router   /scheduling/jobs/preview-cron [post]
func (h *ScheduledJobHandler) PreviewCron(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req previewCronRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailBind(c, err)
		return
	}
	if req.Count <= 0 {
		req.Count = defaultPreviewCount
	}
	next, err := h.scheduler.PreviewCron(op, req.Cron, req.Count)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, cronPreviewView{Cron: req.Cron, Next: next})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type jobView struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	KindText   string         `json:"kindText"`
	Cron       string         `json:"cron" example:"30 9 * * *"`
	CronValid  bool           `json:"cronValid"`
	Payload    map[string]any `json:"payload"`
	Status     string         `json:"status"`
	StatusText string         `json:"statusText"`

	NextRunAt      time.Time  `json:"nextRunAt"`
	LastRunAt      *time.Time `json:"lastRunAt,omitempty"`
	LastStatus     string     `json:"lastStatus,omitempty"`
	LastStatusText string     `json:"lastStatusText,omitempty"`

	ConsecutiveFailures    int `json:"consecutiveFailures"`
	MaxConsecutiveFailures int `json:"maxConsecutiveFailures"`
	TimeoutSeconds         int `json:"timeoutSeconds"`

	TotalRuns   int64 `json:"totalRuns"`
	SuccessRuns int64 `json:"successRuns"`
	// SuccessRate 直接照搬聚合里固化好的派生量。
	// 这里刻意不写 float64(success)/float64(total)*100：成功率是写入当时算好并落库的
	// 事实，接口层重算会让列表接口和详情接口在并发写入时给出互相矛盾的数字。
	SuccessRate string `json:"successRate"`

	CreatedBy uint64    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
} // @name scheduling.JobView

func toJobView(j *entities.ScheduledJob) *jobView {
	if j == nil {
		return nil
	}
	return &jobView{
		ID:       j.ID,
		Name:     j.Name,
		Kind:     j.Kind.String(),
		KindText: j.Kind.DisplayName(),
		Cron:     j.Cron.String(),
		// cronValid 如实暴露「这条历史记录的表达式已经解析不出来了」。
		// 藏起来的话，运维只会看到一条状态正常但永远不执行的任务。
		CronValid:              j.Cron.Valid(),
		Payload:                j.Payload.Map(),
		Status:                 j.Status.String(),
		StatusText:             j.Status.DisplayName(),
		NextRunAt:              j.NextRunAt,
		LastRunAt:              j.LastRunAt,
		LastStatus:             j.LastStatus.String(),
		LastStatusText:         lastStatusText(j),
		ConsecutiveFailures:    j.ConsecutiveFailures,
		MaxConsecutiveFailures: j.MaxConsecutiveFailures,
		TimeoutSeconds:         int(j.EffectiveTimeout() / time.Second),
		TotalRuns:              j.TotalRuns,
		SuccessRuns:            j.SuccessRuns,
		SuccessRate:            decimalx.FormatPercent(j.SuccessRate),
		CreatedBy:              j.CreatedBy,
		CreatedAt:              j.CreatedAt,
		UpdatedAt:              j.UpdatedAt,
	}
}

func lastStatusText(j *entities.ScheduledJob) string {
	if j.LastStatus.IsZero() {
		return ""
	}
	return j.LastStatus.DisplayName()
}

func toJobViews(jobs []*entities.ScheduledJob) []*jobView {
	out := make([]*jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobView(j))
	}
	return out
}

type executionView struct {
	ID          string     `json:"id"`
	JobID       string     `json:"jobId"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	StatusText  string     `json:"statusText"`
	Summary     string     `json:"summary,omitempty"`
	ItemCount   int        `json:"itemCount"`
	Error       string     `json:"error,omitempty"`
	Manual      bool       `json:"manual"`
	Attempt     int        `json:"attempt"`
	ScheduledAt time.Time  `json:"scheduledAt"`
	QueuedAt    time.Time  `json:"queuedAt"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	// DurationMs / DelayMs / QueueWaitMs 都读固化值或由固化值直接得出，
	// 不在读路径上用 time.Since 重算——执行记录是历史，耗时不该随每次刷新而变长。
	//
	// 三段时长各自回答一个不同的问题，合成一个数字就再也分不开：
	//	DelayMs      调度器多久之后才发现这次触发（上限是巡检间隔）
	//	QueueWaitMs  消息在队列里排了多久（变大说明消费者不够用）
	//	DurationMs   真正执行花了多久
	DurationMs  int64 `json:"durationMs"`
	DelayMs     int64 `json:"delayMs"`
	QueueWaitMs int64 `json:"queueWaitMs"`
} // @name scheduling.ExecutionView

func toExecutionView(e *entities.JobExecution) *executionView {
	if e == nil {
		return nil
	}
	return &executionView{
		ID:          e.ID,
		JobID:       e.JobID,
		Kind:        e.JobKind.String(),
		Status:      e.Status.String(),
		StatusText:  e.Status.DisplayName(),
		Summary:     e.Summary,
		ItemCount:   e.ItemCount,
		Error:       e.ErrMsg,
		Manual:      e.Manual,
		Attempt:     e.Attempt,
		ScheduledAt: e.ScheduledFor,
		QueuedAt:    e.QueuedAt,
		StartedAt:   e.StartedAt,
		FinishedAt:  e.FinishedAt,
		DurationMs:  e.DurationMs,
		DelayMs:     e.Delay().Milliseconds(),
		QueueWaitMs: e.QueueWait().Milliseconds(),
	}
}

func toExecutionViews(execs []*entities.JobExecution) []*executionView {
	out := make([]*executionView, 0, len(execs))
	for _, e := range execs {
		out = append(out, toExecutionView(e))
	}
	return out
}

// 下面四个视图各自只装一两个字段，用 gin.H 也能写完。
// 不这么写的理由与 identity.LogoutView 相同：没有类型的响应体进不了 OpenAPI，
// 它的形状就只能靠人手写一份文档来描述，而手写的那份迟早和代码分家。

// deleteJobView 删除结果。
type deleteJobView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name scheduling.DeleteJobView

// pauseJobView 暂停结果。
type pauseJobView struct {
	Paused bool `json:"paused" example:"true"`
} // @name scheduling.PauseJobView

// resumeJobView 恢复结果。
type resumeJobView struct {
	Resumed bool `json:"resumed" example:"true"`
} // @name scheduling.ResumeJobView

// cronPreviewView 是 cron 预览的结果。
//
// 原样回显 cron：预览请求与响应往往隔着几次编辑，把表达式带回来，调用方才能确认
// 屏幕上这串时刻对应的是哪一版表达式。next 是从当前时刻起连续推演出的触发序列，
// 每一项都以上一项为起点，因此它就是任务真正会跑的那几个时刻。
type cronPreviewView struct {
	Cron string      `json:"cron" example:"30 9 * * *"`
	Next []time.Time `json:"next"`
} // @name scheduling.CronPreviewView

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *ScheduledJobHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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
