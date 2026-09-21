// Package http_handlers 把模拟交易上下文暴露成 HTTP 接口。
//
// 本包只做三件事：绑定并做形状校验、转调 domain_service、按统一信封渲染。
// 没有任何业务规则住在这里——它们属于 entities/ 与 domain_services/。
package http_handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

// disclaimer 是每一个含业绩数字的响应都必须携带的免责声明。
//
// # 它是字段而不是文档里的一句话
//
// 模拟盘的收益率截图和真实业绩截图长得一模一样。只要这段话不在响应体里，
// 它就一定会在某次转发、某个前端改版、某张截图里消失，
// 然后一份模拟收益被当成真实业绩传播出去。把它做成响应字段，
// 任何消费者都拿不到一个「不带声明」的版本。
const disclaimer = "本账户为模拟交易：不涉及真实资金、真实券商与真实撮合，" +
	"所有持仓与盈亏均为按行情数据推演的模拟结果，仅供学习与回测使用。" +
	"过往表现不代表未来表现，投资有风险，可能损失本金。本内容不构成任何投资建议。"

// OperatorResolver 从请求上下文里取出调用者身份。
//
// 声明成注入的函数而不是直接读 identity 上下文的 Claims：模拟交易上下文
// 不该在编译期依赖身份上下文。怎么认证、Claims 存在哪个 key 里，
// 是组装根（internal/server）的事。
type OperatorResolver func(c *gin.Context) (domain_services.Operator, bool)

type PaperTradingHandler struct {
	service *domain_services.PaperTradingService
	resolve OperatorResolver
}

func NewPaperTradingHandler(
	service *domain_services.PaperTradingService,
	resolve OperatorResolver,
) *PaperTradingHandler {
	return &PaperTradingHandler{service: service, resolve: resolve}
}

func (h *PaperTradingHandler) Register(rg *gin.RouterGroup, authRequired gin.HandlerFunc) {
	g := rg.Group("/paper", authRequired)
	g.POST("/accounts", h.OpenAccount)
	g.GET("/accounts", h.ListAccounts)
	g.POST("/accounts/:id/reset", h.ResetAccount)
	g.POST("/accounts/:id/orders", h.PlaceOrder)
	g.GET("/accounts/:id/portfolio", h.GetPortfolio)
	g.GET("/accounts/:id/trades", h.ListTrades)
}

// ---------------------------------------------------------------------------
// 账户
//
// 下面每一个带 {id} 的接口，访问别人的账户返回的是 404 而不是 403。
// 这不是漏写：domain_services.requireOwnership 刻意这么定的——403 等于向调用方
// 确认「这个 ID 确实存在，只是不属于你」，接口于是变成账户 ID 的存在性探测器。
// 谁也别顺手把 403 「补」进这些注解里。
// ---------------------------------------------------------------------------

// openAccountRequest 的金额是 string 而不是 float64。
//
// 用 float64 绑定 JSON 数字，1000000.10 在进入领域层之前就已经变成
// 1000000.0999999999，后面整条链路用 decimal 也救不回来。
// 边界上收字符串，是「金额不用浮点」这条规则真正的起点。
type openAccountRequest struct {
	Name        string `json:"name" example:"打新策略"`
	InitialCash string `json:"initialCash" binding:"required" example:"1000000.00"`
} // @name paper.OpenAccountRequest

// OpenAccount 开立一个模拟账户。
//
// 不声明 409：账户 ID 由服务端生成，账户名也没有唯一约束，
// 这条路径上不存在调用方能触发的「已存在」。
//
// @Summary  开立模拟账户
// @Tags     模拟交易
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body     openAccountRequest true "开户参数"
// @Success  200  {object} response.Envelope{data=openAccountResult}
// @Failure  400  {object} response.Envelope "请求参数不合法，或初始资金不是大于 0 的金额"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  429  {object} response.Envelope "已达单用户模拟账户数上限"
// @Router   /paper/accounts [post]
func (h *PaperTradingHandler) OpenAccount(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req openAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	account, err := h.service.OpenAccount(c.Request.Context(), op, domain_services.OpenAccountInput{
		Name:        req.Name,
		InitialCash: req.InitialCash,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, openAccountResult{Account: toAccountView(account), Disclaimer: disclaimer})
}

// ListAccounts 列出调用者自己的全部模拟账户。
//
// 不声明 4xx：查询条件只有调用者自己的 userID，没有路径参数，也没有能写错的查询参数。
//
// @Summary  模拟账户列表
// @Tags     模拟交易
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} response.Envelope{data=accountListResult}
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Router   /paper/accounts [get]
func (h *PaperTradingHandler) ListAccounts(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	accounts, err := h.service.ListAccounts(c.Request.Context(), op)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]*accountView, 0, len(accounts))
	for _, a := range accounts {
		views = append(views, toAccountView(a))
	}
	response.OK(c, accountListResult{Accounts: views, Disclaimer: disclaimer})
}

// ResetAccount 把账户恢复到开户状态。
//
// 409 来自乐观锁：mutate 已经重新加载并重试过三次，再冲突就把结论交给调用方。
//
// @Summary  重置模拟账户
// @Tags     模拟交易
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "模拟账户 ID"
// @Success  200 {object} response.Envelope{data=resetAccountResult}
// @Failure  400 {object} response.Envelope "账户 ID 为空"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "账户不存在，或不属于调用者"
// @Failure  409 {object} response.Envelope "账户被并发修改，重试后仍然冲突"
// @Router   /paper/accounts/{id}/reset [post]
func (h *PaperTradingHandler) ResetAccount(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	account, err := h.service.ResetAccount(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, resetAccountResult{
		Account: toAccountView(account),
		// 重置不会删除成交历史，这一点必须在响应里说清楚，
		// 否则用户会以为「重置了怎么还有记录」是个 bug。
		Note:       "账户已重置为初始资金；成交历史属于只追加的审计记录，予以保留。",
		Disclaimer: disclaimer,
	})
}

// ---------------------------------------------------------------------------
// 下单
// ---------------------------------------------------------------------------

type placeOrderRequest struct {
	Code     string `json:"code" binding:"required" example:"600519.SH"`
	Market   string `json:"market" example:"CN"`
	Side     string `json:"side" binding:"required" enums:"buy,sell" example:"buy"`
	Quantity string `json:"quantity" binding:"required" example:"100"`
	// Price 留空表示按最新行情成交。
	Price string `json:"price" example:"1688.00"`
	Fee   string `json:"fee" example:"5.00"`
} // @name paper.PlaceOrderRequest

// PlaceOrder 下一笔模拟委托，立即成交。
//
// 400 与 409 的分界来自 entities：参数本身不合法（数量非正、方向未知、手续费吃掉成交额）
// 是 400，参数合法但当前账户状态不允许（钱不够、票不够、持仓标的数到顶）是 409。
// 调用方据此决定是改参数还是先补仓/补钱。
//
// @Summary  下单
// @Tags     模拟交易
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    id   path     string            true "模拟账户 ID"
// @Param    body body     placeOrderRequest true "委托内容"
// @Success  200  {object} response.Envelope{data=placeOrderResult}
// @Failure  400  {object} response.Envelope "请求参数不合法：代码、方向、数量、价格或手续费非法"
// @Failure  401  {object} response.Envelope "未登录或令牌无效"
// @Failure  404  {object} response.Envelope "账户不存在，或不属于调用者"
// @Failure  409  {object} response.Envelope "可用资金不足、可卖数量不足、持仓标的数超限，或账户被并发修改"
// @Failure  503  {object} response.Envelope "未指定委托价格且取不到最新行情"
// @Router   /paper/accounts/{id}/orders [post]
func (h *PaperTradingHandler) PlaceOrder(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	var req placeOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, custom_errors.Invalid("请求参数不合法: %v", err))
		return
	}
	trade, err := h.service.PlaceOrder(c.Request.Context(), op, domain_services.PlaceOrderInput{
		AccountID: c.Param("id"),
		Code:      req.Code,
		Market:    req.Market,
		Side:      req.Side,
		Quantity:  req.Quantity,
		Price:     req.Price,
		Fee:       req.Fee,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, placeOrderResult{Trade: toTradeView(trade), Disclaimer: disclaimer})
}

// ---------------------------------------------------------------------------
// 组合与历史
// ---------------------------------------------------------------------------

// GetPortfolio 给账户组合估值。
//
// 不声明 503：行情取不到时领域服务会降级成按成本价估值，
// 并用每个持仓上的 hasQuote=false 把这个事实透出去，而不是让整次查询失败。
//
// @Summary  组合估值
// @Tags     模拟交易
// @Produce  json
// @Security BearerAuth
// @Param    id  path     string true "模拟账户 ID"
// @Success  200 {object} response.Envelope{data=portfolioResult}
// @Failure  400 {object} response.Envelope "账户 ID 为空"
// @Failure  401 {object} response.Envelope "未登录或令牌无效"
// @Failure  404 {object} response.Envelope "账户不存在，或不属于调用者"
// @Router   /paper/accounts/{id}/portfolio [get]
func (h *PaperTradingHandler) GetPortfolio(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	summary, err := h.service.GetPortfolio(c.Request.Context(), op, c.Param("id"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, portfolioResult{Portfolio: toPortfolioView(summary), Disclaimer: disclaimer})
}

// ListTrades 分页返回成交历史。
//
// data 不是标准的 response.PageData：这条响应要额外带 disclaimer，
// 理由见下面 response.OK 处的注释。分页字段的名字与 PageData 保持一致。
//
// @Summary  成交历史
// @Tags     模拟交易
// @Produce  json
// @Security BearerAuth
// @Param    id       path     string true  "模拟账户 ID"
// @Param    page     query    int    false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int    false "每页条数，默认 20，上限 200"
// @Success  200      {object} response.Envelope{data=tradeHistoryResult}
// @Failure  400      {object} response.Envelope "账户 ID 为空"
// @Failure  401      {object} response.Envelope "未登录或令牌无效"
// @Failure  404      {object} response.Envelope "账户不存在，或不属于调用者"
// @Router   /paper/accounts/{id}/trades [get]
func (h *PaperTradingHandler) ListTrades(c *gin.Context) {
	op, ok := h.operator(c)
	if !ok {
		return
	}
	page := parsePage(c)
	records, total, err := h.service.TradeHistory(c.Request.Context(), op, c.Param("id"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]tradeView, 0, len(records))
	for _, r := range records {
		views = append(views, toTradeView(r))
	}
	// 成交历史里有 realizedPnl，属于业绩数字，同样要带声明。
	// 分页信封的 items 里塞不下这个字段，所以整体降级成普通信封 + 自带分页元数据。
	response.OK(c, tradeHistoryResult{
		Items:      views,
		Total:      total,
		Page:       page.Number,
		PageSize:   page.Size,
		Disclaimer: disclaimer,
	})
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// 所有金额字段在 JSON 里都是定长小数字符串。
//
// 不用 JSON 数字：JavaScript 的 number 就是 float64，
// 服务端辛辛苦苦用 decimal 保住的精度，会在浏览器解析那一刻丢掉。
// 定长（而不是 decimal.String()）则保证同一列在每一行都有相同的小数位。
type accountView struct {
	ID          string `json:"id" example:"pacct_2f7c9a1b"`
	UserID      uint64 `json:"userId"`
	Name        string `json:"name" example:"打新策略"`
	InitialCash string `json:"initialCash" example:"1000000.00"`
	Cash        string `json:"cash" example:"831200.00"`
	RealizedPnL string `json:"realizedPnl"`
	TotalFee    string `json:"totalFee"`
	Positions   int    `json:"positionCount"`
	CreatedAt   string `json:"createdAt" example:"2024-05-31 09:30:00"`
	UpdatedAt   string `json:"updatedAt" example:"2024-05-31 14:57:12"`
	// Simulated 恒为 true。它存在的理由和 disclaimer 一样：
	// 消费者不该有任何机会把这份数据当成真实账户的快照。
	Simulated bool `json:"simulated" example:"true"`
} // @name paper.AccountView

func toAccountView(a *entities.PaperAccount) *accountView {
	if a == nil {
		return nil
	}
	return &accountView{
		ID:          a.ID,
		UserID:      a.UserID,
		Name:        a.Name,
		InitialCash: value_objects.FormatMoney(a.InitialCash),
		Cash:        value_objects.FormatMoney(a.Cash),
		RealizedPnL: value_objects.FormatMoney(a.RealizedPnL),
		TotalFee:    value_objects.FormatMoney(a.TotalFee),
		Positions:   len(a.Positions),
		CreatedAt:   a.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt:   a.UpdatedAt.Format("2006-01-02 15:04:05"),
		Simulated:   true,
	}
}

type tradeView struct {
	ID        string `json:"id" example:"ptrade_6b31de04"`
	Symbol    string `json:"symbol" example:"600519.SH"`
	Market    string `json:"market" example:"CN"`
	Side      string `json:"side" enums:"buy,sell" example:"buy"`
	SideText  string `json:"sideText" example:"买入"`
	Quantity  string `json:"quantity" example:"100"`
	Price     string `json:"price" example:"1688.00"`
	Amount    string `json:"amount" example:"168800.00"`
	Fee       string `json:"fee" example:"5.00"`
	RealizedP string `json:"realizedPnl"`
	CashAfter string `json:"cashAfter"`
	TradedAt  string `json:"tradedAt" example:"2024-05-31 14:57:12"`
} // @name paper.TradeView

// toTradeView 直接搬运聚合固化好的 Amount。
//
// 这里刻意不写 Quantity × Price：真正从现金里划走的是落库的那个 amount，
// 接口层重算一遍，只要取整口径有半点差异，用户看到的成交额就和
// cashAfter 的变动对不上——而一个对不平的账本正是模拟盘最没用的状态。
// 这与 analysis 上下文不重算进度百分比是同一条纪律。
func toTradeView(r value_objects.TradeRecord) tradeView {
	return tradeView{
		ID:        r.ID,
		Symbol:    r.Code.FullSymbol(),
		Market:    r.Code.Market.String(),
		Side:      r.Side.String(),
		SideText:  r.Side.DisplayName(),
		Quantity:  value_objects.FormatQuantity(r.Quantity),
		Price:     value_objects.FormatMoney(r.Price),
		Amount:    value_objects.FormatMoney(r.Amount),
		Fee:       value_objects.FormatMoney(r.Fee),
		RealizedP: value_objects.FormatMoney(r.RealizedPnL),
		CashAfter: value_objects.FormatMoney(r.CashAfter),
		TradedAt:  r.TradedAt.Format("2006-01-02 15:04:05"),
	}
}

type positionView struct {
	Symbol      string `json:"symbol" example:"600519.SH"`
	Market      string `json:"market" example:"CN"`
	Quantity    string `json:"quantity" example:"100"`
	AvgCost     string `json:"avgCost" example:"1688.05"`
	CostBasis   string `json:"costBasis" example:"168805.00"`
	MarketPrice string `json:"marketPrice"`
	MarketValue string `json:"marketValue"`
	Unrealized  string `json:"unrealizedPnl"`
	// HasQuote=false 表示上面三个字段是按成本价的回退值，不是实时估值。
	HasQuote bool `json:"hasQuote"`
} // @name paper.PositionView

type portfolioView struct {
	AccountID     string         `json:"accountId"`
	Cash          string         `json:"cash"`
	InitialCash   string         `json:"initialCash"`
	TotalCost     string         `json:"totalCost"`
	MarketValue   string         `json:"marketValue"`
	TotalValue    string         `json:"totalValue"`
	RealizedPnL   string         `json:"realizedPnl"`
	UnrealizedPnL string         `json:"unrealizedPnl"`
	TotalPnL      string         `json:"totalPnl"`
	PositionCount int            `json:"positionCount"`
	Positions     []positionView `json:"positions"`
	Simulated     bool           `json:"simulated" example:"true"`
} // @name paper.PortfolioView

func toPortfolioView(s value_objects.PortfolioSummary) portfolioView {
	positions := make([]positionView, 0, len(s.Positions))
	for _, p := range s.Positions {
		positions = append(positions, positionView{
			Symbol:    p.Code.FullSymbol(),
			Market:    p.Code.Market.String(),
			Quantity:  value_objects.FormatQuantity(p.Quantity),
			AvgCost:   value_objects.FormatMoney(p.AvgCost),
			CostBasis: value_objects.FormatMoney(p.CostBasis),
			// marketPrice / marketValue / unrealizedPnl 是读时算出来的（依赖实时报价），
			// 这是「派生量必须落库」那条规则唯一的例外，理由写在
			// value_objects/portfolio.go 里。hasQuote=false 时它们是按成本价的回退值。
			MarketPrice: value_objects.FormatMoney(p.MarketPrice),
			MarketValue: value_objects.FormatMoney(p.MarketValue),
			Unrealized:  value_objects.FormatMoney(p.UnrealizedPnL),
			HasQuote:    p.HasQuote,
		})
	}
	return portfolioView{
		AccountID:   s.AccountID,
		Cash:        value_objects.FormatMoney(s.Cash),
		InitialCash: value_objects.FormatMoney(s.InitialCash),
		TotalCost:   value_objects.FormatMoney(s.TotalCost),
		MarketValue: value_objects.FormatMoney(s.MarketValue),
		TotalValue:  value_objects.FormatMoney(s.TotalValue),
		// 已实现盈亏读的是账户上逐笔累加的落库值，不是把成交历史 SUM 一遍。
		RealizedPnL:   value_objects.FormatMoney(s.RealizedPnL),
		UnrealizedPnL: value_objects.FormatMoney(s.UnrealizedPnL),
		TotalPnL:      value_objects.FormatMoney(s.TotalPnL()),
		PositionCount: s.PositionCount,
		Positions:     positions,
		Simulated:     true,
	}
}

// ---------------------------------------------------------------------------
// 响应体
//
// 这六个壳子本来都可以写成 gin.H——少六个类型，少六十行。但 gin.H 是
// map[string]any，它在 OpenAPI 里没有形状，于是 disclaimer 这个字段
// 就只存在于代码里，不存在于契约里。而本上下文的全部意义，恰恰是
// 「任何消费者都拿不到一个不带免责声明的版本」：一份对接方照着文档生成的
// 客户端模型如果没有这个字段，声明在传输的第一跳就掉了。
// 有类型，才轮得到它进文档。
// ---------------------------------------------------------------------------

type openAccountResult struct {
	Account    *accountView `json:"account"`
	Disclaimer string       `json:"disclaimer"`
} // @name paper.OpenAccountResult

type accountListResult struct {
	Accounts   []*accountView `json:"accounts"`
	Disclaimer string         `json:"disclaimer"`
} // @name paper.AccountListResult

type resetAccountResult struct {
	Account *accountView `json:"account"`
	Note    string       `json:"note"`
	// Disclaimer 与 note 是两回事：note 解释这次重置做了什么，
	// disclaimer 声明这些数字本来就不是真实业绩。
	Disclaimer string `json:"disclaimer"`
} // @name paper.ResetAccountResult

type placeOrderResult struct {
	Trade      tradeView `json:"trade"`
	Disclaimer string    `json:"disclaimer"`
} // @name paper.PlaceOrderResult

type portfolioResult struct {
	Portfolio  portfolioView `json:"portfolio"`
	Disclaimer string        `json:"disclaimer"`
} // @name paper.PortfolioResult

// tradeHistoryResult 是 response.PageData 加一个 disclaimer。
// 字段名与 PageData 逐字一致，前端的分页组件不用为这一个接口开特例。
type tradeHistoryResult struct {
	Items      []tradeView `json:"items"`
	Total      int64       `json:"total"`
	Page       int         `json:"page" example:"1"`
	PageSize   int         `json:"pageSize" example:"20"`
	Disclaimer string      `json:"disclaimer"`
} // @name paper.TradeHistoryResult

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// operator 取调用者身份；取不到直接写 401 并返回 false，调用方据此提前返回。
func (h *PaperTradingHandler) operator(c *gin.Context) (domain_services.Operator, bool) {
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
