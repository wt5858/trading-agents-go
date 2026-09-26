// Package marketdata 是 domain_services.DataProvider 端口的基础设施实现集合。
//
// 这一层的职责是把各家外部数据源五花八门的报文（列式数组、字符串数字、
// 不同的单位与日期格式）统一收敛成领域模型，领域层不需要知道任何一家的怪癖。
package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

const (
	// tushareEndpoint Tushare Pro 只有这一个入口，所有接口靠 body 里的 api_name 区分，
	// 因此没法用 RESTful 路径复用 http.Client 的连接池分层，这里统一走一个 POST。
	tushareEndpoint = "https://api.tushare.pro"

	// dateLayoutTushare 是 Tushare 的紧凑日期格式。
	// 领域侧的 YYYY-MM-DD 不再需要常量：shared_vo.TradeDate 自己负责规范形式的解析与格式化。
	dateLayoutTushare = "20060102"
)

// TushareProvider 是 A 股数据源实现。
//
// 注意 Tushare 的两个强约束：
//  1. 所有接口按「积分」分级，同一个 token 可能 stock_basic 能调、fina_indicator 调不了，
//     所以错误必须原样带上 msg，否则排查时完全看不出是限权还是参数错。
//  2. 请求频率按分钟限流，超限返回 code!=0 而不是 HTTP 429，不能只看状态码。
type TushareProvider struct {
	token    string
	endpoint string
	http     *http.Client
}

// 编译期确认接口实现，端口签名变更时在这里先报错，而不是等到依赖注入处。
var _ domain_services.DataProvider = (*TushareProvider)(nil)

// NewTushareProvider 构造 A 股数据源。httpClient 由外部注入，便于统一超时与埋点。
func NewTushareProvider(token string, httpClient *http.Client) *TushareProvider {
	p := &TushareProvider{token: token, endpoint: tushareEndpoint}
	p.http = ensureHTTPClient(httpClient, p.Name())
	return p
}

func (p *TushareProvider) Name() string { return "tushare" }

// Supports Tushare 社区版只覆盖 A 股，港股/美股走别的源。
func (p *TushareProvider) Supports(market shared_vo.Market) bool {
	return market == shared_vo.MarketCN
}

// ---------------------------------------------------------------------------
// 通用调用
// ---------------------------------------------------------------------------

type tushareRequest struct {
	APIName string         `json:"api_name"`
	Token   string         `json:"token"`
	Params  map[string]any `json:"params"`
	Fields  string         `json:"fields"`
}

type tushareResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		Fields []string `json:"fields"`
		Items  [][]any  `json:"items"`
	} `json:"data"`
}

// call 是所有 Tushare 接口的唯一出口。
//
// Tushare 返回的是列式结构：fields 是列名数组，items 是二维值数组，值按列序排列。
// 这样省流量，但调用方直接用会写成一堆魔法下标，改一次 fields 就全错位。
// 所以这里在边界上一次性转成 []map[string]any，后续映射只按列名取值。
func (p *TushareProvider) call(ctx context.Context, apiName string, params map[string]any, fields string) ([]map[string]any, error) {
	if p.token == "" {
		return nil, custom_errors.Unavailable("tushare token 未配置，无法调用 %s", apiName)
	}
	if params == nil {
		// 必须是 {} 而不是 null，Tushare 对 null params 会直接报参数错误。
		params = map[string]any{}
	}
	// api_name 是这里唯一能安全落日志的调用标识：URL 对所有接口都一样，
	// 而请求体里带着 token，一个字节都不能打。
	ctx = withOutboundTarget(ctx, apiName)

	body, err := json.Marshal(tushareRequest{
		APIName: apiName,
		Token:   p.token,
		Params:  params,
		Fields:  fields,
	})
	if err != nil {
		return nil, custom_errors.Internal("序列化 tushare 请求失败").Wrap(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, custom_errors.Internal("构造 tushare 请求失败").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		// ctx 取消时 Do 会返回包装后的 context.Canceled，原样带出去让上层能 errors.Is。
		// RedactError 只换掉 *url.Error 里的 URL 字段，Err 原样保留，
		// 所以上面这条 errors.Is 的性质不受影响。
		return nil, custom_errors.Unavailable("调用 tushare %s 失败", apiName).Wrap(httpx.RedactError(err))
	}
	defer resp.Body.Close()

	// 限制读取体积，避免异常响应（比如网关返回的 HTML 错误页）撑爆内存。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, custom_errors.Unavailable("读取 tushare %s 响应失败", apiName).Wrap(httpx.RedactError(err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, custom_errors.Unavailable("tushare %s 返回 HTTP %d: %s", apiName, resp.StatusCode, truncate(string(raw), 200, p.token))
	}

	// 用 Decoder + UseNumber 而不是 json.Unmarshal：响应体的数值字段落进
	// map[string]any，默认解码会把它们全部变成 float64——**在 toDecimal 看到它们之前**
	// 精度就已经丢了，后面再转 decimal 只是把一个错的值精确地搬运下去。
	// UseNumber 让数字保持 json.Number（原始字符串形态），decimal 才有机会无损接手。
	var out tushareResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, custom_errors.Unavailable("解析 tushare %s 响应失败: %s", apiName, truncate(string(raw), 200, p.token)).Wrap(err)
	}
	if out.Code != 0 {
		// 积分不足（40203 等）与限频都走这条分支，msg 是唯一能区分的线索，必须原样透出。
		return nil, custom_errors.Unavailable("tushare %s 返回错误码 %d: %s", apiName, out.Code, out.Msg)
	}
	if out.Data == nil {
		return nil, custom_errors.NotFound("tushare %s 无数据返回", apiName)
	}

	rows := make([]map[string]any, 0, len(out.Data.Items))
	for _, item := range out.Data.Items {
		row := make(map[string]any, len(out.Data.Fields))
		for i, name := range out.Data.Fields {
			// items 里的某一行可能比 fields 短（历史上出现过），越界就留空值而不是 panic。
			if i < len(item) {
				row[name] = item[i]
			} else {
				row[name] = nil
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// 接口实现
// ---------------------------------------------------------------------------

// stockListPageSize / stockListMaxPages 是 stock_basic 的翻页参数。
//
// Tushare 对单次返回的行数有上限（社区档在 6000 行量级），而 A 股在市标的
// 已经超过 5000 且每年还在涨。不翻页的写法今天「刚好够用」，明天就会在
// 无任何报错的情况下静默截断——这类缺口比直接报错难发现得多。
// MaxPages 是纯粹的兜底，防止上游把 offset 参数忽略掉时在这里死循环。
const (
	stockListPageSize = 10000
	stockListMaxPages = 20
)

// FetchStockList 拉取 A 股上市列表（stock_basic），按 offset 翻页直到取完。
func (p *TushareProvider) FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
	if !p.Supports(market) {
		return nil, custom_errors.Invalid("tushare 不支持市场 %s", market)
	}

	var list []*entities.Stock
	for page := 0; page < stockListMaxPages; page++ {
		rows, err := p.call(ctx, "stock_basic", map[string]any{
			// list_status=L 只取在市股票；退市股由单独任务补，避免一次拉回几万行。
			"list_status": "L",
			"limit":       stockListPageSize,
			"offset":      page * stockListPageSize,
		}, "ts_code,symbol,name,area,industry,list_date,list_status")
		if err != nil {
			return nil, err
		}
		list = append(list, p.toStockList(rows)...)
		// 不满一页即到底。放在这里而不是循环顶部判断，省掉一次必定返回空集的调用。
		if len(rows) < stockListPageSize {
			break
		}
	}
	return list, nil
}

// toStockList 把 stock_basic 的行转成聚合根，脏行跳过。
func (p *TushareProvider) toStockList(rows []map[string]any) []*entities.Stock {
	list := make([]*entities.Stock, 0, len(rows))
	for _, row := range rows {
		symbol := toString(row["symbol"])
		if symbol == "" {
			// 极少数行 symbol 为空，此时从 ts_code（600519.SH）里回退切出代码。
			symbol = strings.SplitN(toString(row["ts_code"]), ".", 2)[0]
		}
		code, err := shared_vo.NewStockCode(symbol, shared_vo.MarketCN)
		if err != nil {
			// 指数、退市重列等脏数据直接跳过，不能让一行坏数据毁掉整批同步。
			continue
		}
		params := entities.ListParams{
			Code:     code,
			Name:     toString(row["name"]),
			Industry: toString(row["industry"]),
			Area:     toString(row["area"]),
			Delisted: toString(row["list_status"]) == "D",
			Source:   p.Name(),
		}
		if t, ok := parseTushareDate(toString(row["list_date"])); ok {
			params.ListDate = &t
		}
		// 走 entities.List 而不是直接拼结构体：市值等不变式只在构造函数里守着。
		// 构造失败同样跳过——一行坏数据不该毁掉整批同步。
		s, err := entities.List(params)
		if err != nil {
			continue
		}
		list = append(list, s)
	}
	return list
}

// FetchQuote 取最近一个交易日的行情快照。
//
// Tushare 的 daily 接口不支持「取最新一条」，只能给日期区间再自己挑。
// 这里往前找 14 个自然日，足以覆盖春节等长假，又不会拉太多数据。
func (p *TushareProvider) FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("tushare 不支持市场 %s", code.Market)
	}
	now := time.Now()
	rows, err := p.call(ctx, "daily", map[string]any{
		// Tushare 认带交易所后缀的 ts_code（600519.SH），FullSymbol 正好是这个格式。
		"ts_code":    code.FullSymbol(),
		"start_date": now.AddDate(0, 0, -14).Format(dateLayoutTushare),
		"end_date":   now.Format(dateLayoutTushare),
	}, "ts_code,trade_date,open,high,low,close,pre_close,change,pct_chg,vol,amount")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, custom_errors.NotFound("tushare 无 %s 的近期行情", code.FullSymbol())
	}

	// 官方文档说按交易日倒序，但不作保证，显式挑最大日期更稳。
	latest := rows[0]
	for _, row := range rows[1:] {
		if toString(row["trade_date"]) > toString(latest["trade_date"]) {
			latest = row
		}
	}

	q := p.toQuote(code, latest)
	return &q, nil
}

// FetchQuotes Tushare 的 daily 接口其实支持「按交易日取全市场」（只传 trade_date 不传 ts_code），
// 但那条路要 2000 积分，且拿不到换手率/PE/PB——它们在另一张 daily_basic 表里。
// 在补齐这两点之前如实声明没有这个能力，让降级链把批量行情交给别的源，
// 而不是先返回一份缺列的数据、再让上层去猜为什么换手率全是 0。
func (p *TushareProvider) FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error) {
	return nil, custom_errors.Unavailable(
		"tushare 未启用批量行情端点（市场 %s）", market,
	).Wrap(domain_services.ErrBatchUnsupported)
}

// FetchKlines 拉取日/周/月 K 线。
func (p *TushareProvider) FetchKlines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, r shared_vo.DateRange) ([]value_objects.Kline, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("tushare 不支持市场 %s", code.Market)
	}
	// Tushare 的周线/月线是独立接口而不是 daily 的参数，字段结构与 daily 完全一致。
	var apiName string
	switch period {
	case value_objects.PeriodDaily:
		apiName = "daily"
	case value_objects.PeriodWeekly:
		apiName = "weekly"
	case value_objects.PeriodMonthly:
		apiName = "monthly"
	default:
		return nil, custom_errors.Invalid("不支持的 K 线周期: %s", period)
	}

	params := map[string]any{"ts_code": code.FullSymbol()}
	// 日期必须是 YYYYMMDD，直接把 YYYY-MM-DD 传过去会被当成非法参数静默返回空集。
	// TradeDate.Compact() 就是为这类国内数据源准备的，不必再手写字符串替换。
	if v := r.Start.Compact(); v != "" {
		params["start_date"] = v
	}
	if v := r.End.Compact(); v != "" {
		params["end_date"] = v
	}

	rows, err := p.call(ctx, apiName, params, "ts_code,trade_date,open,high,low,close,vol,amount")
	if err != nil {
		return nil, err
	}

	klines := make([]value_objects.Kline, 0, len(rows))
	for _, row := range rows {
		date := parseTushareTradeDate(toString(row["trade_date"]))
		if date.IsZero() {
			continue
		}
		klines = append(klines, value_objects.Kline{
			Code:      code,
			Period:    period,
			TradeDate: date,
			Open:      toDecimal(row["open"]),
			High:      toDecimal(row["high"]),
			Low:       toDecimal(row["low"]),
			Close:     toDecimal(row["close"]),
			Volume:    toDecimal(row["vol"]).Mul(hundred),     // vol 单位是「手」，统一成股
			Amount:    toDecimal(row["amount"]).Mul(thousand), // amount 单位是「千元」，统一成元
			Adjusted:  false,                                  // daily/weekly/monthly 都是未复权价，复权因子在 adj_factor 接口
			Source:    p.Name(),
		})
	}
	return klines, nil
}

// K 线按交易日批量拉取时的翻页参数。
//
// 必须翻页，不能赌一次拉完：Tushare 的 daily 单次返回上限是 6000 行，而 A 股
// 在市标的已经 5900 余只——就贴着这个上限。多上市几十只就会静默截断，
// 而截断没有任何报错，表现是「某一天莫名其妙少了几十只票的日线」。
// 这正是 stock_basic 那边同样要翻页的理由，两处遵循同一条规矩。
const (
	klinesByDatePageSize = 5000
	klinesByDateMaxPages = 20
)

// FetchKlinesByDate 一次取回整个市场在某个交易日的 K 线。
//
// Tushare 的 daily/weekly/monthly 三个接口都支持「只传 trade_date、不传 ts_code」，
// 返回当天全市场。这条路径的调用次数只和回看天数有关，与标的数无关——
// 逐标的要 5900 次，这里 1 次（加翻页至多两三次）。
func (p *TushareProvider) FetchKlinesByDate(
	ctx context.Context, market shared_vo.Market, period value_objects.Period, date shared_vo.TradeDate,
) ([]value_objects.Kline, error) {
	if !p.Supports(market) {
		return nil, custom_errors.Invalid("tushare 不支持市场 %s", market)
	}
	if date.IsZero() {
		return nil, custom_errors.Invalid("按交易日批量拉取 K 线必须指定日期")
	}
	apiName, err := tushareKlineAPI(period)
	if err != nil {
		return nil, err
	}

	var klines []value_objects.Kline
	for page := 0; page < klinesByDateMaxPages; page++ {
		rows, err := p.call(ctx, apiName, map[string]any{
			// 日期必须是 YYYYMMDD。传 YYYY-MM-DD 会被当成非法参数**静默返回空集**，
			// 那会让这一天看起来像是休市。
			"trade_date": date.Compact(),
			"limit":      klinesByDatePageSize,
			"offset":     page * klinesByDatePageSize,
		}, "ts_code,trade_date,open,high,low,close,vol,amount")
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			k, ok := p.toKline(row, period)
			if !ok {
				continue
			}
			klines = append(klines, k)
		}
		// 不满一页即到底。放在这里而不是循环顶部，省掉一次必定返回空集的调用。
		if len(rows) < klinesByDatePageSize {
			break
		}
	}
	return klines, nil
}

// tushareKlineAPI 把周期映射到接口名。
// 周线/月线是独立接口而不是 daily 的参数，但字段结构与 daily 完全一致。
func tushareKlineAPI(period value_objects.Period) (string, error) {
	switch period {
	case value_objects.PeriodDaily:
		return "daily", nil
	case value_objects.PeriodWeekly:
		return "weekly", nil
	case value_objects.PeriodMonthly:
		return "monthly", nil
	default:
		return "", custom_errors.Invalid("不支持的 K 线周期: %s", period)
	}
}

// toKline 把一行 daily/weekly/monthly 转成 K 线值对象。
//
// 按交易日批量拉时，代码只能从行里的 ts_code 解析（逐标的那条路径上它由调用方给定），
// 所以解析失败要整行跳过而不是拼一个空 Code——没有自然键的 K 线落库时会被丢掉，
// 但会先虚报一次成功。
func (p *TushareProvider) toKline(row map[string]any, period value_objects.Period) (value_objects.Kline, bool) {
	date := parseTushareTradeDate(toString(row["trade_date"]))
	if date.IsZero() {
		return value_objects.Kline{}, false
	}
	code, err := shared_vo.NewStockCode(toString(row["ts_code"]), shared_vo.MarketCN)
	if err != nil {
		return value_objects.Kline{}, false
	}
	return value_objects.Kline{
		Code:      code,
		Period:    period,
		TradeDate: date,
		Open:      toDecimal(row["open"]),
		High:      toDecimal(row["high"]),
		Low:       toDecimal(row["low"]),
		Close:     toDecimal(row["close"]),
		Volume:    toDecimal(row["vol"]).Mul(hundred),     // vol 单位是「手」，统一成股
		Amount:    toDecimal(row["amount"]).Mul(thousand), // amount 单位是「千元」，统一成元
		Adjusted:  false,                                  // 未复权价，复权因子在 adj_factor 接口
		Source:    p.Name(),
	}, true
}

// FetchFinancials 拉取财务指标（fina_indicator）。
func (p *TushareProvider) FetchFinancials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("tushare 不支持市场 %s", code.Market)
	}
	if limit <= 0 {
		limit = 8
	}
	rows, err := p.call(ctx, "fina_indicator", map[string]any{
		"ts_code": code.FullSymbol(),
		"limit":   limit,
	}, "ts_code,ann_date,end_date,eps,revenue_ps,roe,roe_waa,grossprofit_margin,netprofit_margin,debt_to_assets,profit_dedt,op_income")
	if err != nil {
		return nil, err
	}

	now := time.Now()
	items := make([]value_objects.Financial, 0, len(rows))
	for _, row := range rows {
		reportDate := parseTushareTradeDate(toString(row["end_date"]))
		f := value_objects.Financial{
			Code:       code,
			ReportDate: reportDate,
			// ann_date 一直在请求字段里，此前却从未被读出来——于是回测时
			// 「这份财报当天公布了没有」只能靠报告期猜，而那是一处未来函数。
			AnnounceDate: parseTushareTradeDate(toString(row["ann_date"])),
			// 口径由报告期推断（12-31 是年报），规则归 VO 所有，这里不再自己判断月份。
			PeriodType: value_objects.PeriodTypeOfReportDate(reportDate),
			EPS:        toDecimal(row["eps"]),
			// fina_indicator 的 roe 已经是百分数（12.34 表示 12.34%），不要再乘 100。
			ROE:         pickDecimal(row, "roe", "roe_waa"),
			GrossMargin: toDecimal(row["grossprofit_margin"]),
			// netprofit_margin 是数据源算好的净利率，原样落库。
			// 下游绝不能拿 NetProfit/Revenue 自己除一遍：这张指标表里那两个字段
			// 本就常年为 0（见下），除出来只会得到一个看似合理的假值。
			NetMargin: toDecimal(row["netprofit_margin"]),
			DebtRatio: toDecimal(row["debt_to_assets"]),
			// fina_indicator 是「指标」表，没有稳定的绝对营收/净利字段：
			// op_income 为营业利润、profit_dedt 为扣非净利，只能当近似值，
			// 要精确数需另调 income 接口。取不到就留 0，不编造。
			Revenue:   pickDecimal(row, "revenue", "total_revenue", "op_income"),
			NetProfit: pickDecimal(row, "n_income_attr_p", "profit_dedt"),
			// PE/PB 属于估值指标，在 daily_basic 接口，这里保持 0 由上层按需补齐。
			Source:    p.Name(),
			UpdatedAt: now,
		}
		items = append(items, f)
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// FetchNews Tushare 的新闻类接口（news / major_news）需要较高积分等级，
// 社区 token 调用只会拿到权限错误。与其返回假数据骗上层，不如明确声明不可用，
// 由 Composite 降级到别的源。
func (p *TushareProvider) FetchNews(ctx context.Context, code shared_vo.StockCode, r shared_vo.DateRange, limit int) ([]value_objects.News, error) {
	return nil, custom_errors.Unavailable("tushare 新闻接口需要更高积分权限，当前数据源不提供 %s 的资讯", code.FullSymbol())
}

// toQuote 把 daily 的一行转成行情快照。
func (p *TushareProvider) toQuote(code shared_vo.StockCode, row map[string]any) value_objects.Quote {
	return value_objects.Quote{
		Code:      code,
		TradeDate: parseTushareTradeDate(toString(row["trade_date"])),
		Open:      toDecimal(row["open"]),
		High:      toDecimal(row["high"]),
		Low:       toDecimal(row["low"]),
		Close:     toDecimal(row["close"]),
		PreClose:  toDecimal(row["pre_close"]),
		Change:    toDecimal(row["change"]),
		// pct_chg 已是百分数，直接落库，展示层不要再 *100。
		ChangePct: toDecimal(row["pct_chg"]),
		Volume:    toDecimal(row["vol"]).Mul(hundred),
		Amount:    toDecimal(row["amount"]).Mul(thousand),
		// 换手率/PE/PB 在 daily_basic，daily 不返回，这里保持 0。
		Source:    p.Name(),
		UpdatedAt: time.Now(),
	}
}

// parseTushareDate 解析 YYYYMMDD。空值/占位符返回 ok=false。
func parseTushareDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 8 {
		return time.Time{}, false
	}
	t, err := time.Parse(dateLayoutTushare, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseTushareTradeDate 把 Tushare 的 YYYYMMDD 转成交易日值对象。
// 解析不出来返回零值而不是报错：单行日期缺失由调用方跳过该行，不影响整批。
func parseTushareTradeDate(s string) shared_vo.TradeDate {
	t, ok := parseTushareDate(s)
	if !ok {
		return shared_vo.TradeDate{}
	}
	return shared_vo.TradeDateOf(t)
}

// ---------------------------------------------------------------------------
// 包内共享的防御式解析工具
//
// 外部行情 API 普遍存在同一字段时而是数字、时而是字符串、时而是 null 的情况
// （Tushare 的停牌日价格为 null，Finnhub 的部分指标是字符串）。
// 一律走这两个函数，杜绝裸类型断言导致的 panic。
// ---------------------------------------------------------------------------

func toDecimal(v any) decimal.Decimal {
	switch x := v.(type) {
	case nil:
		return decimal.Zero
	case json.Number:
		// 解码器开了 UseNumber，正常路径都落在这里：x 是原始字面量，无损。
		d, err := decimal.NewFromString(strings.TrimSpace(x.String()))
		if err != nil {
			return decimal.Zero
		}
		return d
	case float64:
		// 兜底分支：某处忘了开 UseNumber 时仍然能work，但精度已经在解码时丢过一次，
		// 这里只能做最短十进制还原。
		return decimal.NewFromFloat(x)
	case float32:
		return decimal.NewFromFloat32(x)
	case int:
		return decimal.NewFromInt(int64(x))
	case int64:
		return decimal.NewFromInt(x)
	case string:
		s := strings.TrimSpace(x)
		if s == "" || s == "None" || s == "null" || s == "NaN" {
			return decimal.Zero
		}
		d, err := decimal.NewFromString(s)
		if err != nil {
			return decimal.Zero
		}
		return d
	case bool:
		if x {
			return decimal.NewFromInt(1)
		}
		return decimal.Zero
	default:
		return decimal.Zero
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		// 代码类字段偶尔被解析成数字，用 -1 精度避免出现 600519.000000。
		return strconv.FormatFloat(x, 'f', -1, 64)
	case decimal.Decimal:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// pickDecimal 按优先级取第一个非零字段，用于同一语义在不同接口版本里换过字段名的情况。
func pickDecimal(row map[string]any, keys ...string) decimal.Decimal {
	for _, k := range keys {
		if v, ok := row[k]; ok {
			if d := toDecimal(v); !d.IsZero() {
				return d
			}
		}
	}
	return decimal.Zero
}

// truncate 把上游响应体处理成可以安全拼进错误信息的字符串。finnhub 与 tushare 共用。
//
// 上游的错误响应是排查时最有价值的线索（「积分不足」「无此接口」只在里面），
// 不能整段丢掉；但它是上游完全可控的内容，而网关把收到的凭证回显在错误体里
// 是真实存在的行为——那等于我们自己把 token 抄进了日志。
//
// known 传本数据源的 token：能被回显的凭证只可能是我们刚发出去的那一个，
// 拿它做逐字替换不需要猜。SanitizeBody 之后还会按形态兜一遍，
// 但那一步防的是上游泄漏别人的密钥，不是这里的主要保证。
func truncate(s string, n int, known ...string) string {
	return httpx.SanitizeBody([]byte(s), n, known...)
}

// 单位换算常量：Tushare 的 vol 以「手」计（1 手 = 100 股），amount 以「千元」计。
var (
	hundred  = decimal.NewFromInt(100)
	thousand = decimal.NewFromInt(1000)
)
