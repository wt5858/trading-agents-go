// Package http_handlers 把站内通知上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：通知上下文不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type NotificationHandler struct {
	notificationService *domain_services.NotificationService
	resolve             OperatorResolver
}

func NewNotificationHandler(
	notificationService *domain_services.NotificationService,
	resolve OperatorResolver,
) *NotificationHandler {
	return &NotificationHandler{notificationService: notificationService, resolve: resolve}
}

// Register 挂载路由。
//
// 没有 POST /notifications：通知只能由系统事实（领域事件）产生。开一个创建接口
// 等于允许任何登录用户给任意 user_id 塞通知，那是一条现成的骚扰与钓鱼通道。
//
// read-all 与 :id/read 同在 POST 树下不冲突：gin 的路由树里静态段优先于参数段，
// 「read-all」不会被 :id 吃掉（与 scheduling 的 /jobs/preview-cron 同理）。
func (h *NotificationHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/notifications", authRequired)
	g.GET("", h.ListMine)
	g.GET("/unread-count", h.UnreadCount)
	g.POST("/read-all", h.MarkAllRead)
	g.POST("/:id/read", h.MarkRead)
	g.DELETE("/:id", h.Delete)
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// ListMine 列出我的通知，可按已读状态筛选。
//
// 没有 userId 参数：通知是个人数据，管理员也没有「看别人通知」这个用例，
// 因此这个接口根本不存在一个能查到别人数据的参数组合。
//
// @Summary  我的通知列表
// @Tags     通知
// @Produce  json
// @Security BearerAuth
// @Param    status   query    string false "已读状态筛选，留空表示不限" Enums(unread, read)
// @Param    page     query    int    false "页码，从 1 开始" default(1)
// @Param    pageSize query    int    false "每页条数" default(20)
// @Success  200 {object} response.Envelope{data=response.PageData{items=[]notificationView}}
// @Failure  400 {object} response.Envelope "已读状态筛选值非法"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /notifications [get]
func (h *NotificationHandler) ListMine(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	// status 的合法性由领域服务判定，这里不预先过滤：一个非法值该换来
	// 400 而不是一份静默为空的列表。
	items, total, err := h.notificationService.ListMine(c.Request.Context(), op, c.Query("status"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, toNotificationViews(items), total, page.Number, page.Size)
}

// UnreadCount 返回未读条数，供前端画红点。
//
// 单独一个接口而不是让前端拉一遍列表再数：红点是全站最高频的请求之一，
// 它只需要一个数字，不该把整页通知正文也传一遍。
//
// @Summary  未读条数
// @Tags     通知
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=unreadCountView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /notifications/unread-count [get]
func (h *NotificationHandler) UnreadCount(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	n, err := h.notificationService.UnreadCount(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, unreadCountView{Unread: n})
}

// ---------------------------------------------------------------------------
// 变更
// ---------------------------------------------------------------------------

// MarkRead 把一条通知标为已读。
//
// 404 同时覆盖「不存在」与「不是你的」：领域侧刻意不返回 403，
// 那等于确认这个 ID 存在，会把通知 ID 空间变成可探测的信道。
//
// @Summary  标记已读
// @Tags     通知
// @Produce  json
// @Security BearerAuth
// @Param    id path int true "通知 ID"
// @Success  200 {object} response.Envelope{data=markReadView}
// @Failure  400 {object} response.Envelope "非法的通知 ID"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "通知不存在或不属于当前用户"
// @Failure  409 {object} response.Envelope "通知已是已读状态"
// @Router   /notifications/{id}/read [post]
func (h *NotificationHandler) MarkRead(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	if err := h.notificationService.MarkRead(c.Request.Context(), op, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, markReadView{Read: true})
}

// MarkAllRead 一键已读，返回本次实际标记的条数。
//
// 返回条数而不是只回一个 true：前端据此决定要不要提示「已将 12 条标为已读」，
// 也让「点了没反应」与「本来就没有未读」在响应里就能分辨。
//
// @Summary  一键已读
// @Tags     通知
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=markAllReadView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /notifications/read-all [post]
func (h *NotificationHandler) MarkAllRead(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	n, err := h.notificationService.MarkAllRead(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, markAllReadView{Marked: n})
}

// Delete 删除我的一条通知。
//
// 404 的口径与 MarkRead 一致：删别人的通知与删一条不存在的通知给同一个回答。
//
// @Summary  删除通知
// @Tags     通知
// @Produce  json
// @Security BearerAuth
// @Param    id path int true "通知 ID"
// @Success  200 {object} response.Envelope{data=deleteView}
// @Failure  400 {object} response.Envelope "非法的通知 ID"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "通知不存在或不属于当前用户"
// @Router   /notifications/{id} [delete]
func (h *NotificationHandler) Delete(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	if err := h.notificationService.Delete(c.Request.Context(), op, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deleteView{Deleted: true})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// notificationView 是通知的对外形状。
//
// 它不暴露 dedupeKey：那是纯粹的内部幂等机制，对前端没有任何用处，
// 而把它吐出去等于把「怎么构造一条能覆盖别人通知的键」也一并公开了。
type notificationView struct {
	ID        uint64     `json:"id" example:"1024"`
	Kind      string     `json:"kind" example:"analysis_completed"`
	KindText  string     `json:"kindText"`
	Level     string     `json:"level" example:"info"`
	LevelText string     `json:"levelText"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	LinkType  string     `json:"linkType,omitempty" example:"analysis_task"`
	LinkID    string     `json:"linkId,omitempty"`
	Read      bool       `json:"read"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
} // @name notification.NotificationView

// 以下四个视图各自只包一个标量。写成 gin.H 也能跑，但那样的响应体没有类型，
// 就进不了 OpenAPI——文档只能靠人手写，而手写的那份迟早和代码分家。
// JSON 字段名与原先的 gin.H 键逐字一致，对前端没有任何变化。

// unreadCountView 未读条数，前端红点上的数字。
type unreadCountView struct {
	Unread int64 `json:"unread" example:"3"`
} // @name notification.UnreadCountView

// markReadView 单条已读的结果。
type markReadView struct {
	Read bool `json:"read" example:"true"`
} // @name notification.MarkReadView

// markAllReadView 一键已读的结果，marked 是本次实际标记的条数。
type markAllReadView struct {
	Marked int64 `json:"marked" example:"12"`
} // @name notification.MarkAllReadView

// deleteView 删除结果。
type deleteView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name notification.DeleteView

func toNotificationView(n *entities.Notification) *notificationView {
	if n == nil {
		return nil
	}
	return &notificationView{
		ID:        n.ID,
		Kind:      n.Kind.String(),
		KindText:  n.Kind.DisplayName(),
		Level:     n.Level.String(),
		LevelText: n.Level.DisplayName(),
		Title:     n.Title,
		Body:      n.Body,
		LinkType:  n.LinkType.String(),
		LinkID:    n.LinkID,
		// 已读与否直接问聚合，不在这里写 n.ReadAt != nil：判定只该有一处，
		// 多写一遍就多一处将来会忘记同步的地方。
		Read:      n.IsRead(),
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

func toNotificationViews(items []*entities.Notification) []*notificationView {
	out := make([]*notificationView, 0, len(items))
	for _, n := range items {
		out = append(out, toNotificationView(n))
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *NotificationHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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

// pathID 解析路径上的通知 ID。
//
// 这里不容错（与 parsePage 相反）：一个解析不出来的 ID 意味着调用方想改的是
// 一条它自己都说不清是哪条的通知，回落到 0 只会换来一句更含糊的错误。
func (h *NotificationHandler) pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Fail(c, custom_errors.Invalid("非法的通知 ID: %s", c.Param("id")))
		return 0, false
	}
	return id, true
}

// parsePage 容忍垃圾查询参数，回落到默认值：
// 一个写错的页码不值得让一次读请求整体失败。
func parsePage(c *gin.Context) shared_vo.Page {
	num, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return shared_vo.NewPage(num, size)
}
