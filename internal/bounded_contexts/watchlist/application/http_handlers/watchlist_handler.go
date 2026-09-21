// Package http_handlers 把自选股上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
// 本包也绝不直接构造或修改子实体：编译器不允许（子实体的构造与修改方法都不导出），
// 这正是聚合设计想要的结果。
package http_handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：自选股上下文不该在编译期
// 依赖身份上下文。怎么认证、Claims 存在哪个 key 里，是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type WatchlistHandler struct {
	watchlistService *domain_services.WatchlistService
	resolve          OperatorResolver
}

func NewWatchlistHandler(
	watchlistService *domain_services.WatchlistService,
	resolve OperatorResolver,
) *WatchlistHandler {
	return &WatchlistHandler{watchlistService: watchlistService, resolve: resolve}
}

// Register 挂载路由。
//
// 路径形状刻意让「自选项是分组的下级资源」在 URL 上就成立：
// /groups/:id/items/... 而不是 /items/:itemId。后者会诱导出一个
// 「按自选项 ID 直接改」的接口，而那正是绕过聚合根的那条路。
//
// 唯一的例外是移动接口：它跨两个分组，挂在任何一个分组下面都是误导。
func (h *WatchlistHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/watchlist", authRequired)

	g.GET("/groups", h.ListGroups)
	g.POST("/groups", h.CreateGroup)
	g.PUT("/groups/:id", h.RenameGroup)
	g.DELETE("/groups/:id", h.DeleteGroup)

	g.GET("/groups/:id/items", h.ListItems)
	g.POST("/groups/:id/items", h.AddStock)
	g.POST("/groups/:id/items/reorder", h.Reorder)
	// :code 里会出现 600519.SH 这样带点的值，gin 的路径参数对此没有问题。
	g.PUT("/groups/:id/items/:code/note", h.UpdateNote)
	g.DELETE("/groups/:id/items/:code", h.RemoveStock)

	g.POST("/items/move", h.MoveStock)
}

// ---------------------------------------------------------------------------
// 分组
// ---------------------------------------------------------------------------

// ListGroups 列出当前用户的全部分组。
//
// 不带行情，也不分页：分组是用户手工建的，数量以个位数计，为它做分页
// 只会让前端多写一段永远走不到的翻页逻辑。行情见 ListItems。
//
// @Summary  分组列表
// @Tags     自选股
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=[]groupView}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /watchlist/groups [get]
func (h *WatchlistHandler) ListGroups(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groups, err := h.watchlistService.ListGroups(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toGroupViews(groups))
}

// groupNameRequest 新建与重命名共用同一份入参：两者都只有一个分组名。
type groupNameRequest struct {
	Name string `json:"name" binding:"required" example:"科技股"`
} // @name watchlist.GroupNameRequest

// CreateGroup 新建一个自选分组。
//
// @Summary  新建分组
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     groupNameRequest true "分组名"
// @Success  200  {object} response.Envelope{data=groupView}
// @Failure  400  {object} response.Envelope "分组名为空、超过 16 个字符或含控制字符"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  409  {object} response.Envelope "已存在同名分组"
// @Router   /watchlist/groups [post]
func (h *WatchlistHandler) CreateGroup(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req groupNameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	group, err := h.watchlistService.CreateGroup(c.Request.Context(), op, req.Name)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toGroupView(group))
}

// RenameGroup 重命名分组。
//
// 下面这些接口都**没有** 403：动别人的分组返回的是 404 而不是 403，
// 这是领域服务 loadOwned 刻意选的（403 等于确认「这个 id 存在」，
// 会让分组 ID 空间变成可探测的信道）。文档如实照抄那个决定，
// 写成 403 会诱导前端去区分一个服务端永远不会发出的状态码。
//
// 409 在这里有两个来源：撞上同名分组，以及保存时发现分组已被并发改动。
//
// @Summary  重命名分组
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int              true "分组 ID"
// @Param    body body     groupNameRequest true "新分组名"
// @Success  200  {object} response.Envelope{data=groupView}
// @Failure  400  {object} response.Envelope "分组 ID 非法，或分组名为空、超长、含控制字符"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "分组不存在或不属于当前用户"
// @Failure  409  {object} response.Envelope "已存在同名分组，或分组已被并发修改"
// @Router   /watchlist/groups/{id} [put]
func (h *WatchlistHandler) RenameGroup(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req groupNameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	group, err := h.watchlistService.RenameGroup(c.Request.Context(), op, groupID, req.Name)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toGroupView(group))
}

// DeleteGroup 删除分组及其全部自选股。
//
// 没有「默认分组不可删」这条规则，本上下文压根没有默认分组的概念，
// 因此这里也就没有对应的 409。
//
// @Summary  删除分组
// @Tags     自选股
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "分组 ID"
// @Success  200 {object} response.Envelope{data=deletedView}
// @Failure  400 {object} response.Envelope "分组 ID 非法"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "分组不存在或不属于当前用户"
// @Router   /watchlist/groups/{id} [delete]
func (h *WatchlistHandler) DeleteGroup(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.watchlistService.DeleteGroup(c.Request.Context(), op, groupID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, deletedView{Deleted: true})
}

// ---------------------------------------------------------------------------
// 自选股
// ---------------------------------------------------------------------------

// ListItems 返回分组明细，每行带当前行情。
//
// 不声明 503：行情源挂了不会让这个接口失败，缺行情的行会把价格字段渲染成 null
// （见 latestQuotes 的注释）。把辅助信息的可用性传染给主功能是这里刻意避开的。
//
// @Summary  分组明细
// @Tags     自选股
// @Produce  json
// @Security BearerAuth
// @Param    id  path     int true "分组 ID"
// @Success  200 {object} response.Envelope{data=groupDetailView}
// @Failure  400 {object} response.Envelope "分组 ID 非法"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "分组不存在或不属于当前用户"
// @Router   /watchlist/groups/{id}/items [get]
func (h *WatchlistHandler) ListItems(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	detail, err := h.watchlistService.ListItems(c.Request.Context(), op, groupID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, groupDetailView{
		Group: toGroupView(detail.Group),
		Items: toRowViews(detail.Rows),
	})
}

type addStockRequest struct {
	Code string `json:"code" binding:"required" example:"600519.SH"`
	// Market 留空时由代码自动推断，只有代码本身不带后缀时才需要显式给出。
	Market string `json:"market" example:"CN"`
	Note   string `json:"note" example:"业绩拐点，盯一下"`
} // @name watchlist.AddStockRequest

// AddStock 往分组里加一只标的。
//
// @Summary  添加自选股
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int             true "分组 ID"
// @Param    body body     addStockRequest true "标的与备注"
// @Success  200  {object} response.Envelope{data=itemView}
// @Failure  400  {object} response.Envelope "分组 ID 或标的代码非法，或备注超过 100 个字符"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "分组不存在或不属于当前用户"
// @Failure  409  {object} response.Envelope "该标的已在分组内"
// @Failure  429  {object} response.Envelope "分组已达 200 只上限"
// @Router   /watchlist/groups/{id}/items [post]
func (h *WatchlistHandler) AddStock(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req addStockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	item, err := h.watchlistService.AddStock(c.Request.Context(), op, domain_services.AddStockInput{
		GroupID: groupID,
		Code:    req.Code,
		Market:  req.Market,
		Note:    req.Note,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 新加的一行还没有行情（参考价已经落库，但当前行情要等下一次列表刷新），
	// 这里如实返回不带行情的视图，而不是编一个 0。
	response.OK(c, toItemView(item, nil))
}

// RemoveStock 把一只标的移出分组。
//
// @Summary  移除自选股
// @Tags     自选股
// @Produce  json
// @Security BearerAuth
// @Param    id     path     int    true  "分组 ID"
// @Param    code   path     string true  "标的代码，形如 600519.SH；点号无需转义，直接放进路径即可"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Success  200    {object} response.Envelope{data=removedView}
// @Failure  400    {object} response.Envelope "分组 ID 或标的代码非法"
// @Failure  401    {object} response.Envelope "未登录或令牌无效"
// @Failure  404    {object} response.Envelope "分组不存在、不属于当前用户，或该标的不在分组内"
// @Failure  409    {object} response.Envelope "分组已被并发修改，请重新加载后重试"
// @Router   /watchlist/groups/{id}/items/{code} [delete]
func (h *WatchlistHandler) RemoveStock(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	err = h.watchlistService.RemoveStock(c.Request.Context(), op, domain_services.StockRef{
		GroupID: groupID,
		Code:    c.Param("code"),
		Market:  c.Query("market"),
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, removedView{Removed: true})
}

// updateNoteRequest 备注不是必填：传空串就是清空备注。
type updateNoteRequest struct {
	Note string `json:"note" example:"业绩拐点，盯一下"`
} // @name watchlist.UpdateNoteRequest

// UpdateNote 改某只自选股的备注。
//
// @Summary  修改自选股备注
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id     path     int               true  "分组 ID"
// @Param    code   path     string            true  "标的代码，形如 600519.SH；点号无需转义，直接放进路径即可"
// @Param    market query    string            false "市场（CN/HK/US），留空时由代码自动推断"
// @Param    body   body     updateNoteRequest true  "备注，传空串表示清空"
// @Success  200    {object} response.Envelope{data=updatedView}
// @Failure  400    {object} response.Envelope "分组 ID 或标的代码非法，或备注超过 100 个字符"
// @Failure  401    {object} response.Envelope "未登录或令牌无效"
// @Failure  404    {object} response.Envelope "分组不存在、不属于当前用户，或该标的不在分组内"
// @Failure  409    {object} response.Envelope "分组已被并发修改，请重新加载后重试"
// @Router   /watchlist/groups/{id}/items/{code}/note [put]
func (h *WatchlistHandler) UpdateNote(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req updateNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	ref := domain_services.StockRef{
		GroupID: groupID,
		Code:    c.Param("code"),
		Market:  c.Query("market"),
	}
	if err := h.watchlistService.UpdateNote(c.Request.Context(), op, ref, req.Note); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, updatedView{Updated: true})
}

// reorderRequest 只需传「被拖动的那几只」，未提及的自选项保持原有相对顺序接在后面。
type reorderRequest struct {
	Codes []string `json:"codes" binding:"required" example:"600519.SH,000001.SZ"`
} // @name watchlist.ReorderRequest

// Reorder 重排分组内自选股的顺序。
//
// 列表里混进重复代码或组内没有的代码一律 400，不做静默跳过：
// 那两种都是客户端的真实错误，忽略只会让用户拖出来的顺序和看到的对不上。
//
// @Summary  重排自选股顺序
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     int            true "分组 ID"
// @Param    body body     reorderRequest true "期望排在最前面的标的代码，按新顺序给出"
// @Success  200  {object} response.Envelope{data=reorderedView}
// @Failure  400  {object} response.Envelope "分组 ID 非法，或排序列表为空、含非法代码、含重复代码、含组内不存在的标的"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "分组不存在或不属于当前用户"
// @Failure  409  {object} response.Envelope "分组已被并发修改，请重新加载后重试"
// @Router   /watchlist/groups/{id}/items/reorder [post]
func (h *WatchlistHandler) Reorder(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	groupID, err := parseUintParam(c, "id")
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req reorderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	if err := h.watchlistService.ReorderGroup(c.Request.Context(), op, groupID, req.Codes); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, reorderedView{Reordered: true})
}

type moveStockRequest struct {
	FromGroupID uint64 `json:"fromGroupId" binding:"required"`
	ToGroupID   uint64 `json:"toGroupId" binding:"required"`
	Code        string `json:"code" binding:"required" example:"600519.SH"`
	Market      string `json:"market" example:"CN"`
} // @name watchlist.MoveStockRequest

// MoveStock 把一只标的从一个分组移到另一个分组。
//
// 成功返回并不保证两个分组都已落库：这是刻意允许的中间态（「两个分组里都有」），
// 理由见 WatchlistService.MoveStock 的长注释。因此 409/429 描述的都是**目标分组**的状态。
//
// @Summary  跨分组移动自选股
// @Tags     自选股
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     moveStockRequest true "源分组、目标分组与标的"
// @Success  200  {object} response.Envelope{data=movedView}
// @Failure  400  {object} response.Envelope "标的代码非法，或源分组与目标分组相同"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "源或目标分组不存在、不属于当前用户，或该标的不在源分组内"
// @Failure  409  {object} response.Envelope "目标分组中已存在该标的"
// @Failure  429  {object} response.Envelope "目标分组已达 200 只上限"
// @Router   /watchlist/items/move [post]
func (h *WatchlistHandler) MoveStock(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req moveStockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	err := h.watchlistService.MoveStock(c.Request.Context(), op, domain_services.MoveStockInput{
		FromGroupID: req.FromGroupID,
		ToGroupID:   req.ToGroupID,
		Code:        req.Code,
		Market:      req.Market,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, movedView{Moved: true})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type groupView struct {
	ID        uint64    `json:"id"`
	Name      string    `json:"name" example:"科技股"`
	ItemCount int       `json:"itemCount"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
} // @name watchlist.GroupView

// groupDetailView 是分组明细页的响应：分组本身 + 带行情的自选股列表。
//
// 这几个「本可以用 gin.H 写完」的响应之所以都各自成了一个类型，是因为
// 没有类型的响应体就进不了 OpenAPI，只能靠人手写文档，而手写的那份迟早和代码分家。
// 理由与 identity 的 logoutView 完全相同。
type groupDetailView struct {
	Group *groupView  `json:"group"`
	Items []*itemView `json:"items"`
} // @name watchlist.GroupDetailView

// 下面五个是写操作的确认视图。字段名各不相同（deleted/removed/updated/…）
// 是历史契约，不合并成一个通用的 {"ok":true}：改字段名会静默打断已有的前端。
type deletedView struct {
	Deleted bool `json:"deleted" example:"true"`
} // @name watchlist.DeletedView

type removedView struct {
	Removed bool `json:"removed" example:"true"`
} // @name watchlist.RemovedView

type updatedView struct {
	Updated bool `json:"updated" example:"true"`
} // @name watchlist.UpdatedView

type reorderedView struct {
	Reordered bool `json:"reordered" example:"true"`
} // @name watchlist.ReorderedView

type movedView struct {
	Moved bool `json:"moved" example:"true"`
} // @name watchlist.MovedView

func toGroupView(g *entities.WatchlistGroup) *groupView {
	if g == nil {
		return nil
	}
	return &groupView{
		ID:   g.ID,
		Name: g.Name.String(),
		// 数量直接问聚合，不在这里 len(g.Items)：同一个数只该有一个来源。
		ItemCount: g.ItemCount(),
		CreatedAt: g.CreatedAt,
		UpdatedAt: g.UpdatedAt,
	}
}

func toGroupViews(groups []*entities.WatchlistGroup) []*groupView {
	out := make([]*groupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, toGroupView(g))
	}
	return out
}

// itemView 是自选股列表的一行。
//
// # 三个百分比字段的口径，必须分清楚
//
//	changePct       当日涨跌幅。数据源给出、由 stock 上下文落库，这里**原样照搬**。
//	                绝不用 (price - preClose)/preClose 现算：各家源的复权口径不同，
//	                现算的数字和源给的对不上，用户看到两处打架时无从解释。
//	gainPctSinceAdded  自选以来涨跌幅。它是**读时算**出来的，分子是实时价、
//	                分母是落库的 referencePrice。它没有持久化副本可言——
//	                实时价每一跳都在变，不可能为它重写所有自选行。
//	                算法只有一处：value_objects.ReferencePrice.GainPctSince。
//	referencePrice / referenceDate  上面那个百分比的分母与它的日期，一并返回，
//	                让用户能核对「相对哪一天的哪个价」。少了它，那个百分比不可审计。
type itemView struct {
	Symbol string `json:"symbol" example:"600519"`
	Market string `json:"market" example:"CN"`
	Note   string `json:"note"`
	Sort   int    `json:"sortOrder"`

	// 行情缺失时以下字段为 null，前端显示「暂无行情」，而不是 0。
	//
	// 类型是 *string 而不是 *float64：指针保留「有没有这个数」的语义，
	// 字符串保留精度。两者都不能省——用 float64 则 12.34 到前端可能变成
	// 12.339999999999999，用非指针则「停牌没有行情」和「价格为 0」再也分不开。
	Price     *string `json:"price"`
	ChangePct *string `json:"changePct"`
	TradeDate string  `json:"tradeDate,omitempty"`

	ReferencePrice    *string `json:"referencePrice"`
	ReferenceDate     string  `json:"referenceDate,omitempty"`
	GainPctSinceAdded *string `json:"gainPctSinceAdded"`

	CreatedAt time.Time `json:"createdAt"`
} // @name watchlist.ItemView

func toItemView(it *entities.WatchlistItem, q *value_objects.QuoteSnapshot) *itemView {
	if it == nil {
		return nil
	}
	v := &itemView{
		Symbol:    it.Symbol(),
		Market:    it.Market().String(),
		Note:      it.Note.String(),
		Sort:      it.SortOrder,
		CreatedAt: it.CreatedAt,
	}
	if !it.RefPrice.IsZero() {
		price := decimalx.FormatPrice(it.RefPrice.Price)
		v.ReferencePrice = &price
		v.ReferenceDate = it.RefPrice.TradeDate.String()
	}
	if q == nil {
		return v
	}

	price, changePct := decimalx.FormatPrice(q.Price), decimalx.FormatPercent(q.ChangePct)
	v.Price = &price
	v.ChangePct = &changePct
	v.TradeDate = q.TradeDate.String()
	// 口径只有一处，在领域层。这里只负责把算不出来的情况渲染成 null。
	if gain, ok := it.GainPctSinceWatched(q.Price); ok {
		pct := decimalx.FormatPercent(gain)
		v.GainPctSinceAdded = &pct
	}
	return v
}

func toRowViews(rows []domain_services.WatchlistRow) []*itemView {
	out := make([]*itemView, 0, len(rows))
	for _, row := range rows {
		out = append(out, toItemView(row.Item, row.Quote))
	}
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *WatchlistHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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
