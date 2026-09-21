// Package http_handlers 把股票上下文暴露成 HTTP 接口。
//
// 处理器只做三件事：绑定并做形状校验、调用 domain_service、渲染统一响应信封。
// 任何业务规则都不在这里；代码规范化、日期区间推导这类语义校验属于值对象与领域服务。
package http_handlers

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

type StockHandler struct {
	stockService *domain_services.StockService
}

func NewStockHandler(stockService *domain_services.StockService) *StockHandler {
	return &StockHandler{stockService: stockService}
}

// Register 挂载路由。
//
// 六个入口都是读路径，不要求登录：行情与公司主数据是公开信息，
// 加鉴权只会让前端的自选股页面为了看一眼股价先去换 token。
func (h *StockHandler) Register(rg *gin.RouterGroup) {
	stocks := rg.Group("/stocks")
	stocks.GET("", h.Search)
	stocks.GET("/:code", h.BasicInfo)
	stocks.GET("/:code/quote", h.LatestQuote)
	stocks.GET("/:code/klines", h.Klines)
	stocks.GET("/:code/financials", h.Financials)
	stocks.GET("/:code/news", h.News)
}

// Search 按关键词搜索标的，分页返回。
//
// 不声明 4xx：市场过滤器的非法值在领域服务里按「不限市场」处理，
// 页码解析也只退回默认值，这条路径没有调用方能修正的失败。
//
// @Summary  搜索标的
// @Tags     股票行情
// @Produce  json
// @Param    keyword  query    string false "关键词，匹配代码前缀、名称或行业"
// @Param    market   query    string false "市场（CN/HK/US），留空表示不限"
// @Param    page     query    int    false "页码，从 1 开始，默认 1"
// @Param    pageSize query    int    false "每页条数，默认 20，上限 200"
// @Success  200      {object} response.Envelope{data=response.PageData{items=[]stockView}}
// @Router   /stocks [get]
func (h *StockHandler) Search(c *gin.Context) {
	page := parsePage(c)
	list, total, err := h.stockService.Search(
		c.Request.Context(), c.Query("keyword"), c.Query("market"), page)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]*stockView, 0, len(list))
	for _, s := range list {
		views = append(views, toStockView(s))
	}
	response.OKPage(c, views, total, page.Number, page.Size)
}

// BasicInfo 取单只标的主数据。
//
// @Summary  查询标的主数据
// @Tags     股票行情
// @Produce  json
// @Param    code   path     string true  "标的代码，如 600519.SH"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Success  200    {object} response.Envelope{data=stockView}
// @Failure  400    {object} response.Envelope "标的代码为空或格式非法"
// @Failure  404    {object} response.Envelope "标的不存在，且补数后仍未命中"
// @Router   /stocks/{code} [get]
func (h *StockHandler) BasicInfo(c *gin.Context) {
	code, err := pathCode(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	stock, err := h.stockService.GetBasicInfo(c.Request.Context(), code, c.Query("market"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toStockView(stock))
}

// LatestQuote 取最新行情快照。
//
// 本地未命中会走数据源补数，因此失败码是 503 而不是 404：
// marketdata.Composite 把各数据源的错误（限流、缺权限、该源没有这只票）
// 统一聚合成 Unavailable 再抛出，上层拿不到「确实不存在」这个结论。
// 下面几个读接口同理。
//
// @Summary  查询最新行情快照
// @Tags     股票行情
// @Produce  json
// @Param    code   path     string true  "标的代码，如 600519.SH"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Success  200    {object} response.Envelope{data=quoteView}
// @Failure  400    {object} response.Envelope "标的代码为空或格式非法"
// @Failure  503    {object} response.Envelope "本地无快照且所有数据源取数失败"
// @Router   /stocks/{code}/quote [get]
func (h *StockHandler) LatestQuote(c *gin.Context) {
	code, err := pathCode(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	quote, err := h.stockService.LatestQuote(c.Request.Context(), code, c.Query("market"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, toQuoteView(*quote))
}

// Klines 取 K 线序列。
//
// @Summary  查询 K 线序列
// @Tags     股票行情
// @Produce  json
// @Param    code   path     string true  "标的代码，如 600519.SH"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Param    period query    string false "周期 daily/weekly/monthly，留空按日线"
// @Param    start  query    string false "起始交易日 YYYY-MM-DD，与 end 都留空时取近半年"
// @Param    end    query    string false "结束交易日 YYYY-MM-DD"
// @Param    limit  query    int    false "最多返回条数，留空不限，上限由仓储收敛"
// @Success  200    {object} response.Envelope{data=[]klineView}
// @Failure  400    {object} response.Envelope "代码、周期或日期区间非法"
// @Failure  503    {object} response.Envelope "本地无数据且所有数据源取数失败"
// @Router   /stocks/{code}/klines [get]
func (h *StockHandler) Klines(c *gin.Context) {
	code, err := pathCode(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	klines, err := h.stockService.Klines(c.Request.Context(), domain_services.KlineQuery{
		Code:   code,
		Market: c.Query("market"),
		Period: c.Query("period"),
		Start:  c.Query("start"),
		End:    c.Query("end"),
		Limit:  parseLimit(c, 0),
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]klineView, 0, len(klines))
	for _, k := range klines {
		views = append(views, toKlineView(k))
	}
	response.OK(c, views)
}

// Financials 取财务数据序列。
//
// @Summary  查询财务数据序列
// @Tags     股票行情
// @Produce  json
// @Param    code   path     string true  "标的代码，如 600519.SH"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Param    limit  query    int    false "返回期数，默认 8"
// @Success  200    {object} response.Envelope{data=[]financialView}
// @Failure  400    {object} response.Envelope "标的代码为空或格式非法"
// @Failure  503    {object} response.Envelope "本地无数据且所有数据源取数失败"
// @Router   /stocks/{code}/financials [get]
func (h *StockHandler) Financials(c *gin.Context) {
	code, err := pathCode(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	items, err := h.stockService.Financials(
		c.Request.Context(), code, c.Query("market"), parseLimit(c, 8))
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]financialView, 0, len(items))
	for _, f := range items {
		views = append(views, toFinancialView(f))
	}
	response.OK(c, views)
}

// News 取区间内的资讯。
//
// @Summary  查询标的资讯
// @Tags     股票行情
// @Produce  json
// @Param    code   path     string true  "标的代码，如 600519.SH"
// @Param    market query    string false "市场（CN/HK/US），留空时由代码自动推断"
// @Param    start  query    string false "起始日期 YYYY-MM-DD，与 end 都留空时取近半年"
// @Param    end    query    string false "结束日期 YYYY-MM-DD"
// @Param    limit  query    int    false "最多返回条数，默认 20"
// @Success  200    {object} response.Envelope{data=[]newsView}
// @Failure  400    {object} response.Envelope "标的代码或日期区间非法"
// @Failure  503    {object} response.Envelope "本地无数据且所有数据源取数失败"
// @Router   /stocks/{code}/news [get]
func (h *StockHandler) News(c *gin.Context) {
	code, err := pathCode(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	items, err := h.stockService.News(
		c.Request.Context(), code, c.Query("market"),
		c.Query("start"), c.Query("end"), parseLimit(c, 20))
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]newsView, 0, len(items))
	for _, n := range items {
		views = append(views, toNewsView(n))
	}
	response.OK(c, views)
}

// ---------------------------------------------------------------------------
// 入参解析
// ---------------------------------------------------------------------------

// pathCode 只检查「路径参数非空」这一条形状约束。
// 代码本身是否合法、属于哪个市场，由 shared_vo.StockCode 在领域服务里判定——
// 那套规则在这里复制一份，迟早会和值对象的规则不一致。
func pathCode(c *gin.Context) (string, error) {
	code := strings.TrimSpace(c.Param("code"))
	if code == "" {
		return "", custom_errors.Invalid("股票代码不能为空")
	}
	return code, nil
}

// parsePage 容忍垃圾查询参数并退回默认值：一个写错的页码不值得让读请求失败。
func parsePage(c *gin.Context) shared_vo.Page {
	num, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return shared_vo.NewPage(num, size)
}

// parseLimit 同理：解析不出来就用各接口自己的默认值，条数上限由仓储收敛。
func parseLimit(c *gin.Context, fallback int) int {
	v, err := strconv.Atoi(c.Query("limit"))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------------------
// 响应视图
//
// 不直接把实体/值对象序列化出去：StockCode 是嵌套结构，直接 marshal 会得到
// {"code":{"symbol":...}} 这种给前端添堵的形状，而且领域模型加字段会静默改变接口契约。
// ---------------------------------------------------------------------------

type stockView struct {
	ID       uint64 `json:"id"`
	Symbol   string `json:"symbol" example:"600519"`
	FullCode string `json:"fullCode" example:"600519.SH"`
	Market   string `json:"market" example:"CN"`
	Name     string `json:"name"`
	Industry string `json:"industry"`
	Area     string `json:"area"`
	ListDate string `json:"listDate" example:"2001-08-27"`
	Delisted bool   `json:"delisted"`
	// 市值原样输出数据源给的值，前端要换单位自己换——服务端换一次单位，
	// 跨源对账时就再也说不清页面上的数字是哪个口径了。
	//
	// 数值一律以字符串输出：JavaScript 的 number 就是 float64，
	// 服务端再怎么用 decimal 精确计算，一旦以 JSON 数字送出去，
	// 前端 JSON.parse 的那一刻精度就没了。
	TotalMV    string `json:"totalMarketValue"`
	CircMV     string `json:"circulatingMarketValue"`
	Analyzable bool   `json:"analyzable"`
	Source     string `json:"source"`
	UpdatedAt  string `json:"updatedAt"`
} // @name stock.StockView

func toStockView(s *entities.Stock) *stockView {
	if s == nil {
		return nil
	}
	v := &stockView{
		ID:         s.ID,
		Symbol:     s.Symbol(),
		FullCode:   s.FullSymbol(),
		Market:     string(s.Market()),
		Name:       s.Name,
		Industry:   s.Industry,
		Area:       s.Area,
		Delisted:   s.Delisted,
		TotalMV:    decimalx.FormatMoney(s.TotalMV),
		CircMV:     decimalx.FormatMoney(s.CircMV),
		Analyzable: s.IsAnalyzable(),
		Source:     s.Source,
		UpdatedAt:  s.UpdatedAt.Format(timeLayout),
	}
	if s.ListDate != nil {
		v.ListDate = s.ListDate.Format(dateLayout)
	}
	return v
}

type quoteView struct {
	Symbol    string `json:"symbol" example:"600519"`
	Market    string `json:"market" example:"CN"`
	TradeDate string `json:"tradeDate" example:"2024-05-31"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	PreClose  string `json:"preClose"`
	// Change / ChangePct 直接透传落库值，不在这里用 Close-PreClose 重算。
	Change    string `json:"change"`
	ChangePct string `json:"changePct"`
	Volume    string `json:"volume"`
	Amount    string `json:"amount"`
	Turnover  string `json:"turnover"`
	PE        string `json:"pe"`
	PB        string `json:"pb"`
	LimitUp   bool   `json:"limitUp"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updatedAt"`
} // @name stock.QuoteView

func toQuoteView(q value_objects.Quote) quoteView {
	return quoteView{
		Symbol:    q.Symbol(),
		Market:    string(q.Market()),
		TradeDate: q.TradeDate.String(),
		Open:      decimalx.FormatPrice(q.Open),
		High:      decimalx.FormatPrice(q.High),
		Low:       decimalx.FormatPrice(q.Low),
		Close:     decimalx.FormatPrice(q.Close),
		PreClose:  decimalx.FormatPrice(q.PreClose),
		Change:    decimalx.FormatPrice(q.Change),
		ChangePct: decimalx.FormatPercent(q.ChangePct),
		Volume:    decimalx.FormatQuantity(q.Volume),
		Amount:    decimalx.FormatMoney(q.Amount),
		Turnover:  decimalx.FormatPercent(q.Turnover),
		PE:        decimalx.FormatRatio(q.PE),
		PB:        decimalx.FormatRatio(q.PB),
		LimitUp:   q.IsLimitUp(),
		Source:    q.Source,
		UpdatedAt: q.UpdatedAt.Format(timeLayout),
	}
}

type klineView struct {
	Symbol    string `json:"symbol" example:"600519"`
	Period    string `json:"period" example:"daily"`
	TradeDate string `json:"tradeDate" example:"2024-05-31"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
	Amount    string `json:"amount"`
	Adjusted  bool   `json:"adjusted"`
	Source    string `json:"source"`
} // @name stock.KlineView

func toKlineView(k value_objects.Kline) klineView {
	return klineView{
		Symbol:    k.Symbol(),
		Period:    k.Period.String(),
		TradeDate: k.TradeDate.String(),
		Open:      decimalx.FormatPrice(k.Open),
		High:      decimalx.FormatPrice(k.High),
		Low:       decimalx.FormatPrice(k.Low),
		Close:     decimalx.FormatPrice(k.Close),
		Volume:    decimalx.FormatQuantity(k.Volume),
		Amount:    decimalx.FormatMoney(k.Amount),
		Adjusted:  k.Adjusted,
		Source:    k.Source,
	}
}

type financialView struct {
	Symbol     string `json:"symbol" example:"600519"`
	ReportDate string `json:"reportDate" example:"2023-12-31"`
	PeriodType string `json:"periodType" example:"annual"`
	Revenue    string `json:"revenue"`
	NetProfit  string `json:"netProfit"`
	EPS        string `json:"eps"`
	PE         string `json:"pe"`
	PB         string `json:"pb"`
	// 四个比率都是百分数口径，且都取自落库值；netMargin 尤其不要在前端
	// 用 netProfit/revenue 复算——两者来自不同的数据源字段，口径未必一致。
	ROE         string `json:"roe"`
	GrossMargin string `json:"grossMargin"`
	NetMargin   string `json:"netMargin"`
	DebtRatio   string `json:"debtRatio"`
	Source      string `json:"source"`
	UpdatedAt   string `json:"updatedAt"`
} // @name stock.FinancialView

func toFinancialView(f value_objects.Financial) financialView {
	return financialView{
		Symbol:      f.Symbol(),
		ReportDate:  f.ReportDate.String(),
		PeriodType:  f.PeriodType.String(),
		Revenue:     decimalx.FormatMoney(f.Revenue),
		NetProfit:   decimalx.FormatMoney(f.NetProfit),
		EPS:         decimalx.FormatRatio(f.EPS),
		PE:          decimalx.FormatRatio(f.PE),
		PB:          decimalx.FormatRatio(f.PB),
		ROE:         decimalx.FormatPercent(f.ROE),
		GrossMargin: decimalx.FormatPercent(f.GrossMargin),
		NetMargin:   decimalx.FormatPercent(f.NetMargin),
		DebtRatio:   decimalx.FormatPercent(f.DebtRatio),
		Source:      f.Source,
		UpdatedAt:   f.UpdatedAt.Format(timeLayout),
	}
}

type newsView struct {
	Symbol      string `json:"symbol" example:"600519"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Source      string `json:"source"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	// 情感分是 [-1, 1] 区间的数值，同样以字符串输出；
	// positive/negative 是服务端按 ±0.2 中性带给出的判定，前端不要自己定阈值。
	Sentiment string `json:"sentiment" example:"0.62"`
	Positive  bool   `json:"positive"`
	Negative  bool   `json:"negative"`
} // @name stock.NewsView

func toNewsView(n value_objects.News) newsView {
	return newsView{
		Symbol:      n.Symbol(),
		Title:       n.Title,
		Content:     n.Content,
		Source:      n.Source,
		URL:         n.URL,
		PublishedAt: n.PublishedAt.Format(timeLayout),
		Sentiment:   n.Sentiment.String(),
		Positive:    n.Sentiment.IsPositive(),
		Negative:    n.Sentiment.IsNegative(),
	}
}

const (
	dateLayout = "2006-01-02"
	timeLayout = "2006-01-02 15:04:05"
)
