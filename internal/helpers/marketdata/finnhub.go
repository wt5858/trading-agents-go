package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/httpx"
)

// finnhubBaseURL Finnhub 的 REST 根路径，所有接口都是 GET + query 参数。
const finnhubBaseURL = "https://finnhub.io/api/v1"

// FinnhubProvider 是美股数据源实现。
//
// 几个必须知道的怪癖：
//  1. 鉴权既可以走 token query 参数也可以走 X-Finnhub-Token 头，这里用头，
//     避免 token 出现在 URL 里被日志/代理记录下来。
//  2. 免费额度是 60 次/分钟，超限返回 HTTP 429，需要上层退避重试。
//  3. 多数接口「无数据」不是 404，而是 200 + 空对象/空数组，必须自己判空。
type FinnhubProvider struct {
	token   string
	baseURL string
	http    *http.Client
}

var _ domain_services.DataProvider = (*FinnhubProvider)(nil)

// NewFinnhubProvider 构造美股数据源。
func NewFinnhubProvider(token string, httpClient *http.Client) *FinnhubProvider {
	p := &FinnhubProvider{token: token, baseURL: finnhubBaseURL}
	p.http = ensureHTTPClient(httpClient, p.Name())
	return p
}

func (p *FinnhubProvider) Name() string { return "finnhub" }

// Supports Finnhub 也有港股数据，但免费档只开放美股，这里只声明美股避免误降级。
func (p *FinnhubProvider) Supports(market shared_vo.Market) bool {
	return market == shared_vo.MarketUS
}

// get 是所有 Finnhub 接口的统一出口，把响应直接解到 out 指向的结构。
func (p *FinnhubProvider) get(ctx context.Context, path string, query url.Values, out any) error {
	if p.token == "" {
		return custom_errors.Unavailable("finnhub token 未配置，无法调用 %s", path)
	}

	endpoint := p.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	// 出站日志只记 host+path，query 整段不落盘（见 outbound_log.go）。
	// 但排查时总得知道是哪只票，所以按白名单单独挑两个确定不含凭证的参数当标识——
	// 白名单是显式的，新增参数不会自动进日志。
	if s := query.Get("symbol"); s != "" {
		ctx = withOutboundTarget(ctx, s)
	} else if e := query.Get("exchange"); e != "" {
		ctx = withOutboundTarget(ctx, e)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return custom_errors.Internal("构造 finnhub 请求失败").Wrap(err)
	}
	req.Header.Set("X-Finnhub-Token", p.token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		// RedactError：Do 返回的 *url.Error 带着完整 URL（含 query）。本实现把
		// token 放在头里，所以今天不会泄——但那是这一行调用点的巧合，不是错误链
		// 的性质。Finnhub 同样支持 ?token= 认证，哪天有人改成那种写法，
		// 一次连接超时就会把 token 写进日志，而没有任何一行代码会提示他。
		return custom_errors.Unavailable("调用 finnhub %s 失败", path).Wrap(httpx.RedactError(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return custom_errors.Unavailable("读取 finnhub %s 响应失败", path).Wrap(httpx.RedactError(err))
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// 单独区分限流码，上层可据此做退避而不是直接把数据源标记为坏掉。
		return custom_errors.QuotaExceeded("finnhub %s 触发限流（60 次/分钟）", path)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 403 在 Finnhub 上通常意味着「该接口属于付费档」，而不是 token 错误。
		return custom_errors.Unavailable("finnhub %s 无访问权限（HTTP %d），该接口可能需要付费订阅", path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return custom_errors.Unavailable("finnhub %s 返回 HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 200, p.token))
	}

	// UseNumber 的理由见 tushare.go 的同名注释：默认解码会把数值先压成 float64，
	// 精度在我们拿到它之前就没了。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return custom_errors.Unavailable("解析 finnhub %s 响应失败: %s", path, truncate(string(raw), 200, p.token)).Wrap(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 接口实现
// ---------------------------------------------------------------------------

type finnhubSymbol struct {
	Symbol        string `json:"symbol"`
	DisplaySymbol string `json:"displaySymbol"`
	Description   string `json:"description"`
	Type          string `json:"type"`
	Currency      string `json:"currency"`
	MIC           string `json:"mic"`
}

// FetchStockList 拉取美股全量标的列表。
func (p *FinnhubProvider) FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
	if !p.Supports(market) {
		return nil, custom_errors.Invalid("finnhub 不支持市场 %s", market)
	}
	var raw []finnhubSymbol
	if err := p.get(ctx, "/stock/symbol", url.Values{"exchange": {"US"}}, &raw); err != nil {
		return nil, err
	}

	// UpdatedAt 由聚合根在 entities.List 里自己打时间戳，这里不再各自取一次 now。
	list := make([]*entities.Stock, 0, len(raw))
	for _, item := range raw {
		// 这个接口混着 ETF、ADR、权证、优先股，只留普通股，否则几万条噪声会淹没主数据。
		if item.Type != "" && item.Type != "Common Stock" {
			continue
		}
		symbol := item.DisplaySymbol
		if symbol == "" {
			symbol = item.Symbol
		}
		code, err := shared_vo.NewStockCode(symbol, shared_vo.MarketUS)
		if err != nil {
			// 带数字或过长的代码（多为非普通股）不符合领域规则，直接跳过。
			continue
		}
		// 走 entities.List 而不是直接拼结构体，不变式只在构造函数里守着；构造失败（市场识别不了）同样跳过，
		// 一行坏数据不该毁掉整批同步。
		s, err := entities.List(entities.ListParams{
			Code:   code,
			Name:   strings.TrimSpace(item.Description),
			Source: p.Name(),
		})
		if err != nil {
			continue
		}
		list = append(list, s)
	}
	return list, nil
}

// finnhubQuote /quote 的响应。字段名极简：c=current, d=change, dp=change percent,
// h=high, l=low, o=open, pc=previous close, t=报价时间戳（秒）。
type finnhubQuote struct {
	C  decimal.Decimal `json:"c"`
	D  decimal.Decimal `json:"d"`
	DP decimal.Decimal `json:"dp"`
	H  decimal.Decimal `json:"h"`
	L  decimal.Decimal `json:"l"`
	O  decimal.Decimal `json:"o"`
	PC decimal.Decimal `json:"pc"`
	T  int64           `json:"t"`
}

// FetchQuote 取实时报价快照。
func (p *FinnhubProvider) FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("finnhub 不支持市场 %s", code.Market)
	}
	var raw finnhubQuote
	// 美股代码不带交易所后缀，FullSymbol 对 US 市场原样返回 Symbol。
	if err := p.get(ctx, "/quote", url.Values{"symbol": {code.FullSymbol()}}, &raw); err != nil {
		return nil, err
	}
	// 代码不存在时 Finnhub 返回 200 + 全 0，只能靠收盘价判空。
	if raw.C.IsZero() && raw.PC.IsZero() && raw.O.IsZero() {
		return nil, custom_errors.NotFound("finnhub 无 %s 的行情数据", code.Symbol)
	}

	tradeDate := shared_vo.Today()
	if raw.T > 0 {
		// t 是 UTC 秒级时间戳，用 UTC 换算交易日，避免服务器时区把日期推前一天。
		tradeDate = shared_vo.TradeDateOf(time.Unix(raw.T, 0).UTC())
	}

	return &value_objects.Quote{
		// Code 一个字段同时承载 symbol 与 market，不再分别赋值——
		// 两者分开存迟早会出现 market 与代码所属市场不一致的脏数据。
		Code:      code,
		TradeDate: tradeDate,
		Open:      raw.O,
		High:      raw.H,
		Low:       raw.L,
		Close:     raw.C,
		PreClose:  raw.PC,
		Change:    raw.D,
		ChangePct: raw.DP, // 已是百分数
		// /quote 不返回成交量/成交额/估值指标，保持 0，不用其他接口的数据冒充。
		Source:    p.Name(),
		UpdatedAt: time.Now(),
	}, nil
}

// finnhubCandles /stock/candle 的响应，同样是列式：各数组按下标一一对应。
type finnhubCandles struct {
	C []decimal.Decimal `json:"c"`
	H []decimal.Decimal `json:"h"`
	L []decimal.Decimal `json:"l"`
	O []decimal.Decimal `json:"o"`
	V []decimal.Decimal `json:"v"`
	T []int64           `json:"t"`
	S string            `json:"s"` // ok / no_data
}

// FetchQuotes Finnhub 没有「一次取回全市场快照」的端点，/quote 只吃单只代码。
//
// 这里零 IO 返回哨兵，而不是在方法内部循环调 /quote：那样会把 N+1 藏进数据源，
// SyncRun 看不到逐只的成败，也就没有分片检查点与失败计数——
// 端口刻意不提供批量取单只信息，就是为了防这件事。
func (p *FinnhubProvider) FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error) {
	return nil, custom_errors.Unavailable(
		"finnhub 无批量行情端点（市场 %s）", market,
	).Wrap(domain_services.ErrBatchUnsupported)
}

// FetchKlines 拉取 K 线。
//
// 注意 /stock/candle 已被 Finnhub 划入付费档，免费 token 会拿到 403；
// get 会把它转成 Unavailable，Composite 据此降级到下一个源。
func (p *FinnhubProvider) FetchKlines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, r shared_vo.DateRange) ([]value_objects.Kline, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("finnhub 不支持市场 %s", code.Market)
	}
	var resolution string
	switch period {
	case value_objects.PeriodDaily:
		resolution = "D"
	case value_objects.PeriodWeekly:
		resolution = "W"
	case value_objects.PeriodMonthly:
		resolution = "M"
	default:
		return nil, custom_errors.Invalid("不支持的 K 线周期: %s", period)
	}

	// from/to 是 Unix 秒。to 取当日 23:59:59，否则会漏掉区间最后一个交易日。
	from, err := tradeDateUnix(r.Start, false)
	if err != nil {
		return nil, err
	}
	to, err := tradeDateUnix(r.End, true)
	if err != nil {
		return nil, err
	}

	var raw finnhubCandles
	if err := p.get(ctx, "/stock/candle", url.Values{
		"symbol":     {code.FullSymbol()},
		"resolution": {resolution},
		"from":       {formatInt64(from)},
		"to":         {formatInt64(to)},
	}, &raw); err != nil {
		return nil, err
	}
	if raw.S != "ok" || len(raw.T) == 0 {
		// no_data 视为「本源取不到」，返回 NotFound 让 Composite 继续尝试下一个源，
		// 而不是把空集当成功结果直接截断降级链。
		return nil, custom_errors.NotFound("finnhub 无 %s 在 %s~%s 的 K 线数据", code.Symbol, r.Start, r.End)
	}

	n := len(raw.T)
	klines := make([]value_objects.Kline, 0, n)
	for i := 0; i < n; i++ {
		// 各数组长度理论上一致，但只要有一个短了就会越界，逐个兜底。
		klines = append(klines, value_objects.Kline{
			Code:      code,
			Period:    period,
			TradeDate: shared_vo.TradeDateOf(time.Unix(raw.T[i], 0).UTC()),
			Open:      atIdx(raw.O, i),
			High:      atIdx(raw.H, i),
			Low:       atIdx(raw.L, i),
			Close:     atIdx(raw.C, i),
			Volume:    atIdx(raw.V, i), // 单位是股，与 A 股换算后的口径一致
			// Finnhub 不返回成交额，留 0 而不是用 close*volume 估算，避免下游误当真实数据。
			Adjusted: true, // 美股蜡烛图默认已做拆股复权
			Source:   p.Name(),
		})
	}
	return klines, nil
}

// finnhubMetric /stock/metric?metric=all 的响应。metric 是一张扁平的指标字典，
// 值可能是数字、字符串或 null，所以用 any 接再走 toFloat。
type finnhubMetric struct {
	Metric map[string]any `json:"metric"`
	Symbol string         `json:"symbol"`
}

// FetchFinancials 取财务/估值指标快照。
//
// 这个接口返回的是 TTM/最新一期的横截面指标，不是按报告期的时间序列，
// 所以最多只能产出 1 条 Financial；limit 在这里只起「是否需要」的作用。
// FetchKlinesByDate Finnhub 的 /stock/candle 只按 symbol 查询，没有全市场端点。
func (p *FinnhubProvider) FetchKlinesByDate(
	_ context.Context, market shared_vo.Market, _ value_objects.Period, _ shared_vo.TradeDate,
) ([]value_objects.Kline, error) {
	return nil, custom_errors.Unavailable(
		"finnhub 未提供按交易日的批量 K 线端点（市场 %s）", market,
	).Wrap(domain_services.ErrBatchUnsupported)
}

func (p *FinnhubProvider) FetchFinancials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("finnhub 不支持市场 %s", code.Market)
	}
	if limit <= 0 {
		limit = 1
	}
	var raw finnhubMetric
	if err := p.get(ctx, "/stock/metric", url.Values{
		"symbol": {code.FullSymbol()},
		"metric": {"all"},
	}, &raw); err != nil {
		return nil, err
	}
	if len(raw.Metric) == 0 {
		return nil, custom_errors.NotFound("finnhub 无 %s 的财务指标", code.Symbol)
	}

	m := raw.Metric
	f := value_objects.Financial{
		Code: code,
		// 这个接口不给报告期，只能用「今天」当占位。它同时也是 Mongo 侧的自然键之一，
		// 所以每天会各存一条 TTM 快照——这正是想要的：TTM 指标本来就是逐日滚动的。
		ReportDate: shared_vo.Today(),
		PeriodType: value_objects.PeriodTypeTTM, // 明确标注口径，避免和 A 股的 annual/quarter 混淆
		EPS:        pickDecimal(m, "epsTTM", "epsBasicExclExtraItemsTTM", "epsAnnual"),
		PE:         pickDecimal(m, "peTTM", "peBasicExclExtraTTM", "peNormalizedAnnual"),
		PB:         pickDecimal(m, "pbQuarterly", "pbAnnual"),
		// roeTTM / grossMarginTTM 都是百分数口径，与 Tushare 的 roe 保持一致，无需换算。
		ROE:         pickDecimal(m, "roeTTM", "roeRfy", "roeAnnual"),
		GrossMargin: pickDecimal(m, "grossMarginTTM", "grossMarginAnnual"),
		// netProfitMarginTTM 是数据源给的净利率（百分数），直接落到 NetMargin，
		// 读路径不再用 NetProfit/Revenue 现算——本接口根本给不出可靠的绝对净利。
		NetMargin: pickDecimal(m, "netProfitMarginTTM", "netProfitMarginAnnual"),
		// Finnhub 给的是「总负债/总权益」倍数而非资产负债率，语义不同但都是杠杆指标，
		// 下游解读时需注意口径差异。
		DebtRatio: pickDecimal(m, "totalDebt/totalEquityQuarterly", "totalDebt/totalEquityAnnual"),
		// 只认口径确实是「绝对营收」的字段。revenuePerShareTTM 是每股营收、
		// netIncomeEmployeeTTM 是人均净利，量纲差好几个数量级，
		// 当兜底值填进来会让下游的营收同比直接算飞，宁可留 0 让缺失可见。
		Revenue:   pickDecimal(m, "revenueTTM", "revenueAnnual"),
		NetProfit: pickDecimal(m, "netIncomeTTM", "netIncomeAnnual"),
		Source:    p.Name(),
		UpdatedAt: time.Now(),
	}
	return []value_objects.Financial{f}, nil
}

// finnhubNews /company-news 的响应元素。
type finnhubNews struct {
	Category string `json:"category"`
	Datetime int64  `json:"datetime"`
	Headline string `json:"headline"`
	ID       int64  `json:"id"`
	Related  string `json:"related"`
	Source   string `json:"source"`
	Summary  string `json:"summary"`
	URL      string `json:"url"`
}

// FetchNews 拉取公司新闻。
func (p *FinnhubProvider) FetchNews(ctx context.Context, code shared_vo.StockCode, r shared_vo.DateRange, limit int) ([]value_objects.News, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("finnhub 不支持市场 %s", code.Market)
	}
	if limit <= 0 {
		limit = 20
	}
	// company-news 的 from/to 用的是 YYYY-MM-DD（和 candle 的 Unix 秒不一样，容易踩坑）。
	var raw []finnhubNews
	if err := p.get(ctx, "/company-news", url.Values{
		"symbol": {code.FullSymbol()},
		// TradeDate.String() 就是 YYYY-MM-DD 规范形式，无需再格式化。
		"from": {r.Start.String()},
		"to":   {r.End.String()},
	}, &raw); err != nil {
		return nil, err
	}

	items := make([]value_objects.News, 0, len(raw))
	for _, n := range raw {
		if n.Headline == "" {
			continue
		}
		items = append(items, value_objects.News{
			Code:        code,
			Title:       n.Headline,
			Content:     n.Summary,
			Source:      n.Source,
			URL:         n.URL,
			PublishedAt: time.Unix(n.Datetime, 0).UTC(),
			// Finnhub 不带情感分，留 0（中性）由情感分析环节回填，不在这里瞎猜。
		})
		if len(items) >= limit {
			break
		}
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// tradeDateUnix 把交易日值对象转成 Unix 秒。endOfDay 为真时取当日 23:59:59。
//
// 入参换成 TradeDate 之后这里不再需要解析字符串：格式校验在 VO 的构造点已经做过，
// 剩下唯一可能的失败是「日期未指定」，而 Finnhub 的 candle 接口强制要求 from/to。
func tradeDateUnix(d shared_vo.TradeDate, endOfDay bool) (int64, error) {
	t, ok := d.Time()
	if !ok {
		return 0, custom_errors.Invalid("finnhub K 线查询必须指定起止交易日")
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Second)
	}
	return t.Unix(), nil
}

func formatInt64(v int64) string {
	return toString(v)
}

// atIdx 越界返回 0，防止 Finnhub 各数组长度不一致时 panic。
func atIdx(s []decimal.Decimal, i int) decimal.Decimal {
	if i < 0 || i >= len(s) {
		return decimal.Zero
	}
	return s[i]
}
