package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/httpx"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

// EastmoneyProvider 直连东方财富的行情接口，覆盖 A 股 / 港股 / 美股。
//
// # 为什么是自己实现而不是接 AKShare
//
// AKShare 是 Python 库，Go 进程调不到；它底层做的事就是拼这几个 push2 请求再把
// 列式报文转成 DataFrame。这里把那几个请求用 Go 复刻一遍，省掉一个 Python 边车
// 与一次进程间跳转。代价是东财改版要自己跟——所以每个魔法参数下面都写清了它是什么，
// 改版时才有得对照。
//
// # 和 Tushare 的分工
//
// 东财的独门优势是 clist 这个**整表端点**：一次请求返回一页 100 条全市场快照，
// 带代码/名称/最新价/总市值/流通市值/市盈率/市净率。A 股 5900 余只 60 次请求拿完，
// 而逐标的取行情要 5900 次。股票列表与行情快照因此共用同一次扫描。
//
// 但它也有短板，两条都要记住：
//  1. clist 不返回行业/地区/上市日期，那几列只有 Tushare 的 stock_basic 有。
//     所以默认编排里 Tushare 排在前面，见 di/providers/stock_provider.go。
//  2. K 线仍然是每标的一次调用，这件事上东财并不比 Tushare 强。
//
// # 失败形态：它不报错，它掐你
//
// Tushare 超配额会返回 code!=0 加一句人话；东财直接关掉 TCP 连接，客户端看到的是
// 一个空响应——没有状态码、没有响应体，和网络抖动完全一样（Go 侧是 EOF）。
// 所以重试与限速不是优化，是这个数据源能不能用的前提，见 throttle.go。
//
// # 境外部署必须走 delay 主机
//
// 掐连接这件事有两个来源，排查时要分开看：
//
//  1. **主机选错**。非大陆 IP 打 push2 系主机，连接会在 3~5 秒时被掐；
//     实测 82.push2 三次成一次、72.push2 三次全废，而对应的 push2delay 三次全成。
//     push2 本来就会在它愿意的时候 302 到 push2delay，所以境外拿到的一直是
//     延时行情，只是多依赖了一跳并不可靠的重定向。现在直连 delay 主机，
//     不损失数据，只是去掉那一跳。这一条是确定性的，重试救不了。
//  2. **打太快**。这一条才是真正的限流，靠 EastmoneyRPS 压住。
//
// 部署到大陆网络内时把 spotHosts 换回 push2 可以拿到实时行情。
type EastmoneyProvider struct {
	spotHosts map[shared_vo.Market]string
	klineHost string
	// searchHost 是站内搜索，个股资讯走它。与行情/K 线是完全独立的一套接口，
	// 连响应格式都不同（它只给 JSONP）。
	searchHost string
	http       *http.Client
}

var _ domain_services.DataProvider = (*EastmoneyProvider)(nil)

const (
	// ut 是东财公开接口的固定令牌，不是凭据：它对所有匿名调用都一样，
	// 浏览器打开行情页时发的就是这两个值。放在代码里不构成泄密。
	// 两个接口用的 ut 不同，是东财自己的历史遗留，不能互换。
	eastmoneyClistUT = "bd1d9ddb04089700cf9c27f6f7426281"
	eastmoneyKlineUT = "7eea3edcaed734bea9cbfc24409ed989"

	// eastmoneyPageSize 是 clist 的每页条数。
	//
	// 写 100 不是保守，是服务端硬顶：实测请求 pz=1000 与 pz=5000 都只回 100 条，
	// 且不报错。翻页因此必须按响应里的 total 走，不能指望一次多要点。
	eastmoneyPageSize = 100

	// eastmoneyMaxPages 是翻页兜底，防止 total 异常时空转。
	// 美股 13800 余只是当前最大的市场，138 页，这里给到三倍余量。
	eastmoneyMaxPages = 400

	// eastmoneyFqt 复权类型：0 不复权、1 前复权、2 后复权。
	//
	// 写死 0，**刻意不做成参数**。klines 的自然键是 (symbol, period, trade_date)，
	// 不含复权标记——这是 MarketDataRepository.SaveKlines 的明确设计，前提是
	// 「全系统只写一种口径」。而 Tushare 的 daily 写的就是不复权（见 tushare.go 的
	// Adjusted: false）。这里一旦改成前复权，两个源会在同一个 key 上反复互相覆盖，
	// 指标序列随时间轴随机跳档，而且没有任何一层会报错。
	//
	// 另外前复权因子每个除权日都会重算，前复权序列在 upsert 下本就不是幂等的：
	// 昨天抓的那一段今天再抓，值就变了。
	eastmoneyFqt = "0"

	// clist 的字段清单。顺序即响应里 diff 元素的键顺序，改动这里要同步改 spotRow。
	eastmoneyClistFields = "f2,f3,f4,f5,f6,f8,f9,f12,f13,f14,f15,f16,f17,f18,f20,f21,f23,f124"

	// kline 的字段清单。fields2 决定 klines 里 CSV 的列序。
	eastmoneyKlineFields1 = "f1,f2,f3,f4,f5,f6"
	eastmoneyKlineFields2 = "f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61"
)

// eastmoneyFilters 是各市场的板块过滤表达式（东财自己的 DSL）。
//
//	m:0 t:6    深证主板      m:0 t:80   创业板
//	m:1 t:2    上证主板      m:1 t:23   科创板
//	m:0 t:81 s:2048          北交所
//	m:128 t:1..4             港股各板
//	m:105/106/107            美股三家交易所（NASDAQ / NYSE / AMEX）
var eastmoneyFilters = map[shared_vo.Market]string{
	shared_vo.MarketCN: "m:0 t:6,m:0 t:80,m:1 t:2,m:1 t:23,m:0 t:81 s:2048",
	shared_vo.MarketHK: "m:128 t:3,m:128 t:4,m:128 t:1,m:128 t:2",
	shared_vo.MarketUS: "m:105,m:106,m:107",
}

// NewEastmoneyProvider 构造东财数据源。
//
// rps/burst 直接决定这个源能不能用，不是性能旋钮，见文件头的限流说明。
// 不收 *zap.Logger：出站日志由 ensureHTTPClient 挂的 Transport 负责，
// 其余几处按 logger.FromContext 取（拿得到 trace_id），与 tushare/finnhub 一致。
func NewEastmoneyProvider(httpClient *http.Client, rps float64, burst int) *EastmoneyProvider {
	p := &EastmoneyProvider{
		// A 股与港美股分属不同的推送集群（82 / 72），走错主机会拿到空集而不是报错。
		//
		// 用 push2delay 而不是 push2，是实测结果不是保守：
		// 非大陆出口 IP 打 push2，连接会被建立之后再在 3~5 秒时掐掉（Go 侧表现为 EOF），
		// 实测 82.push2 三次里成一次、72.push2 三次全废；而对应的 push2delay
		// 三次全成。push2 本来就会在它心情好的时候 302 到 push2delay，
		// 也就是说我们**一直拿的都是延时行情**，只是多依赖了一跳并不可靠的重定向。
		// 直连 delay 主机不损失任何数据，只是把那一跳去掉。
		//
		// 如果部署到大陆网络内，把这里换回 push2 能拿到实时行情——那时 302 不会发生。
		spotHosts: map[shared_vo.Market]string{
			shared_vo.MarketCN: "https://82.push2delay.eastmoney.com",
			shared_vo.MarketHK: "https://72.push2delay.eastmoney.com",
			shared_vo.MarketUS: "https://72.push2delay.eastmoney.com",
		},
		// K 线没有对应的 delay 主机：实测 push2hisdelay 三次全废，而 push2his
		// 三次里成两次。这条路只能靠 throttle.go 的重试兜，别去找 delay 变体。
		klineHost:  "https://push2his.eastmoney.com",
		searchHost: "https://search-api-web.eastmoney.com",
	}
	p.http = ensureThrottledHTTPClient(httpClient, p.Name(), rps, burst)
	return p
}

func (p *EastmoneyProvider) Name() string { return "eastmoney" }

// Supports 东财三个市场都覆盖，这也是它目前唯一能给港股供数的源。
func (p *EastmoneyProvider) Supports(market shared_vo.Market) bool {
	_, ok := eastmoneyFilters[market]
	return ok
}

// ---------------------------------------------------------------------------
// clist：全市场快照
// ---------------------------------------------------------------------------

// spotRow 是 clist 响应里 diff 数组的一个元素。
//
// 用 json.Number 而不是 float64：这些字段会变成价格与市值落库，
// 走 float64 就是在解码那一刻先丢一次精度，后面再转 decimal 只是把错的值精确搬运。
// 停牌等情况下东财返回字符串 "-"，所以类型必须能同时容纳数字与字符串——
// 交给 toDecimal 去判，它已经处理了这两种形态。
type spotRow struct {
	Latest    any    `json:"f2"`  // 最新价
	ChangePct any    `json:"f3"`  // 涨跌幅（已是百分数）
	Change    any    `json:"f4"`  // 涨跌额
	Volume    any    `json:"f5"`  // 成交量（A股/港股为手，美股为股）
	Amount    any    `json:"f6"`  // 成交额（元）
	Turnover  any    `json:"f8"`  // 换手率
	PE        any    `json:"f9"`  // 市盈率（动态）
	Code      string `json:"f12"` // 代码
	// MarketID 是东财内部市场号（0 深/北, 1 沪, 128 港, 105/106/107 美）。
	//
	// 解析出来只为记录报文形态，**刻意不用它拼 K 线的 secid**：两者不是同一个
	// 命名空间，港股在这里报 128 而 K 线接口要的前缀是 116。secid 一律由
	// (market, symbol) 在 secid() 里集中派生，见那里的说明。
	MarketID  any    `json:"f13"`
	Name      string `json:"f14"` // 名称
	High      any    `json:"f15"`
	Low       any    `json:"f16"`
	Open      any    `json:"f17"`  // 今开
	PreClose  any    `json:"f18"`  // 昨收
	TotalMV   any    `json:"f20"`  // 总市值（元）
	CircMV    any    `json:"f21"`  // 流通市值（元）
	PB        any    `json:"f23"`  // 市净率
	QuoteTime any    `json:"f124"` // 行情时间，unix 秒
}

type clistResponse struct {
	RC   int `json:"rc"`
	Data *struct {
		Total int       `json:"total"`
		Diff  []spotRow `json:"diff"`
	} `json:"data"`
}

// fetchSpot 翻页取回整个市场的快照。
//
// **整次扫描是 all-or-nothing 的**：任何一页失败都让整个方法失败，绝不返回半截列表。
// 这条很关键——上层拿本地标的全集当分母算覆盖率，收到半截列表会把没问到的几千只票
// 记成「源没给」并计入失败，而真相是「我们没问完」。宁可一条不写，也不要谎报覆盖率。
func (p *EastmoneyProvider) fetchSpot(ctx context.Context, market shared_vo.Market) ([]spotRow, error) {
	filter, ok := eastmoneyFilters[market]
	if !ok {
		return nil, custom_errors.Invalid("eastmoney 不支持市场 %s", market)
	}
	host := p.spotHosts[market]
	ctx = withOutboundTarget(ctx, "clist:"+market.String())

	var (
		rows  []spotRow
		total = -1
	)
	for page := 1; page <= eastmoneyMaxPages; page++ {
		q := url.Values{
			"pn":     {strconv.Itoa(page)},
			"pz":     {strconv.Itoa(eastmoneyPageSize)},
			"po":     {"1"},
			"np":     {"1"},
			"ut":     {eastmoneyClistUT},
			"fltt":   {"2"},
			"invt":   {"2"},
			"fid":    {"f12"},
			"fs":     {filter},
			"fields": {eastmoneyClistFields},
		}

		var out clistResponse
		if err := p.getJSON(ctx, host+"/api/qt/clist/get", q, &out); err != nil {
			return nil, err
		}
		if out.RC != 0 {
			return nil, custom_errors.Unavailable("eastmoney clist 返回 rc=%d（市场 %s，第 %d 页）", out.RC, market, page)
		}
		if out.Data == nil {
			// data 为 null 是东财限流时的另一种形态。当成空集会让同步报「成功 0 条」，
			// 必须当硬错误。
			return nil, custom_errors.Unavailable("eastmoney clist 无数据返回（市场 %s，第 %d 页）", market, page)
		}
		if total < 0 {
			total = out.Data.Total
		}
		if len(out.Data.Diff) == 0 {
			if total > 0 && len(rows) < total {
				return nil, custom_errors.Unavailable(
					"eastmoney clist 第 %d 页为空但仅取到 %d/%d 条（市场 %s）", page, len(rows), total, market)
			}
			break
		}
		rows = append(rows, out.Data.Diff...)
		if len(rows) >= total {
			break
		}
	}
	if total > 0 && len(rows) < total {
		return nil, custom_errors.Unavailable(
			"eastmoney clist 翻页未取完：%d/%d（市场 %s）", len(rows), total, market)
	}
	return rows, nil
}

// FetchStockList 用一次全市场扫描换回股票主数据。
//
// 注意这里拿不到行业/地区/上市日期——clist 不返回这三列。仓储的 upsert 已经做成
// 「空值不覆盖」，所以这不会擦掉 Tushare 填过的值，但也填不上；
// 新股的这几列要靠 Tushare 补，见 stock_repository.go 的 stockOnConflict。
func (p *EastmoneyProvider) FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
	rows, err := p.fetchSpot(ctx, market)
	if err != nil {
		return nil, err
	}

	list := make([]*entities.Stock, 0, len(rows))
	dropped := 0
	for _, row := range rows {
		code, ok := p.toStockCode(row, market)
		if !ok {
			dropped++
			continue
		}
		s, err := entities.List(entities.ListParams{
			Code: code,
			Name: strings.TrimSpace(row.Name),
			// 市值统一用元，与 f20/f21 的原始口径一致，不做换算。
			TotalMV: toDecimal(row.TotalMV),
			CircMV:  toDecimal(row.CircMV),
			Source:  p.Name(),
		})
		if err != nil {
			// 东财偶尔会给出流通市值大于总市值的行（多为数据延迟），
			// 聚合的不变式会挡下来。跳过单行，不毁掉整批。
			dropped++
			continue
		}
		list = append(list, s)
	}
	p.logDropped(ctx, "FetchStockList", market, len(rows), dropped)
	return list, nil
}

// FetchQuotes 一次扫描换回整个市场的行情快照——这是接东财最主要的理由。
func (p *EastmoneyProvider) FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error) {
	rows, err := p.fetchSpot(ctx, market)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	quotes := make([]value_objects.Quote, 0, len(rows))
	dropped := 0
	for _, row := range rows {
		code, ok := p.toStockCode(row, market)
		if !ok {
			dropped++
			continue
		}
		date, ok := p.tradeDateOf(row)
		if !ok {
			dropped++
			continue
		}
		quotes = append(quotes, value_objects.Quote{
			Code:      code,
			TradeDate: date,
			Open:      toDecimal(row.Open),
			High:      toDecimal(row.High),
			Low:       toDecimal(row.Low),
			// clist 给的是「最新价」。收盘后它就是收盘价；盘中它是即时价，
			// 落进 Close 是刻意的——本系统的行情快照本来就是「截至此刻」的语义。
			Close:    toDecimal(row.Latest),
			PreClose: toDecimal(row.PreClose),
			Change:   toDecimal(row.Change),
			// f3 已经是百分数（1.07 表示 1.07%），展示层不要再乘 100。
			ChangePct: toDecimal(row.ChangePct),
			Volume:    p.toShares(toDecimal(row.Volume), market),
			Amount:    toDecimal(row.Amount),
			Turnover:  toDecimal(row.Turnover),
			PE:        toDecimal(row.PE),
			PB:        toDecimal(row.PB),
			Source:    p.Name(),
			UpdatedAt: now,
		})
	}
	p.logDropped(ctx, "FetchQuotes", market, len(rows), dropped)
	return quotes, nil
}

// FetchQuote 单只行情。
//
// 它存在只是为了满足端口：真正该走的是 FetchQuotes。读路径上偶尔补一只票会落到这里，
// 此时扫一遍全市场太浪费，所以直接问 K 线接口要最近一根日线。
func (p *EastmoneyProvider) FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("eastmoney 不支持市场 %s", code.Market)
	}
	// 往前取 14 天足以跨过春节这类长假，又不会拉回太多数据。
	klines, err := p.FetchKlines(ctx, code, value_objects.PeriodDaily, shared_vo.LastNDays(14))
	if err != nil {
		return nil, err
	}
	if len(klines) == 0 {
		return nil, custom_errors.NotFound("eastmoney 无 %s 的近期行情", code.FullSymbol())
	}
	last := klines[len(klines)-1]
	preClose := last.Open
	if len(klines) >= 2 {
		preClose = klines[len(klines)-2].Close
	}
	return &value_objects.Quote{
		Code:      code,
		TradeDate: last.TradeDate,
		Open:      last.Open,
		High:      last.High,
		Low:       last.Low,
		Close:     last.Close,
		PreClose:  preClose,
		Volume:    last.Volume,
		Amount:    last.Amount,
		// 涨跌额/幅、换手率、PE、PB 在 K 线接口里没有，留零由上层按需补。
		// 不在这里用 Close-PreClose 现算：那样算出来的值和 clist 给的口径对不上，
		// 下游分不清自己拿到的是哪一种。
		Source:    p.Name(),
		UpdatedAt: time.Now(),
	}, nil
}

// ---------------------------------------------------------------------------
// kline：历史 K 线
// ---------------------------------------------------------------------------

type klineResponse struct {
	RC   int `json:"rc"`
	Data *struct {
		Code   string   `json:"code"`
		Klines []string `json:"klines"`
	} `json:"data"`
}

// FetchKlines 拉取日/周/月 K 线。
func (p *EastmoneyProvider) FetchKlines(
	ctx context.Context,
	code shared_vo.StockCode,
	period value_objects.Period,
	r shared_vo.DateRange,
) ([]value_objects.Kline, error) {
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("eastmoney 不支持市场 %s", code.Market)
	}
	var klt string
	switch period {
	case value_objects.PeriodDaily:
		klt = "101"
	case value_objects.PeriodWeekly:
		klt = "102"
	case value_objects.PeriodMonthly:
		klt = "103"
	default:
		return nil, custom_errors.Invalid("不支持的 K 线周期: %s", period)
	}

	secid, err := p.secid(code)
	if err != nil {
		return nil, err
	}
	ctx = withOutboundTarget(ctx, "kline:"+code.FullSymbol())

	q := url.Values{
		"secid":   {secid},
		"klt":     {klt},
		"fqt":     {eastmoneyFqt},
		"fields1": {eastmoneyKlineFields1},
		"fields2": {eastmoneyKlineFields2},
		"ut":      {eastmoneyKlineUT},
		// 日期是 YYYYMMDD。给空串东财会当成参数缺失返回全量，所以缺省值写死边界。
		"beg": {compactOr(r.Start, "19700101")},
		"end": {compactOr(r.End, "20500101")},
	}

	var out klineResponse
	if err := p.getJSON(ctx, p.klineHost+"/api/qt/stock/kline/get", q, &out); err != nil {
		return nil, err
	}
	if out.RC != 0 {
		return nil, custom_errors.Unavailable("eastmoney kline 返回 rc=%d（%s）", out.RC, code.FullSymbol())
	}
	if out.Data == nil {
		// 代码不存在与被限流都会走到这里。当成空集会让同步把它记成「成功但无数据」，
		// 而实际上我们什么都没拿到。
		return nil, custom_errors.NotFound("eastmoney 无 %s 的 K 线数据", code.FullSymbol())
	}

	klines := make([]value_objects.Kline, 0, len(out.Data.Klines))
	for _, line := range out.Data.Klines {
		k, ok := p.parseKline(line, code, period)
		if !ok {
			continue
		}
		klines = append(klines, k)
	}
	return klines, nil
}

// parseKline 解析一行 CSV。
// 列序由 fields2 决定：日期,开盘,收盘,最高,最低,成交量,成交额,振幅,涨跌幅,涨跌额,换手率。
// 注意第 2 列是**收盘**不是最高——照着 OHLC 的直觉写会把开收高低全接错。
func (p *EastmoneyProvider) parseKline(
	line string,
	code shared_vo.StockCode,
	period value_objects.Period,
) (value_objects.Kline, bool) {
	f := strings.Split(line, ",")
	if len(f) < 7 {
		return value_objects.Kline{}, false
	}
	date, err := shared_vo.NewTradeDate(f[0])
	if err != nil || date.IsZero() {
		return value_objects.Kline{}, false
	}
	return value_objects.Kline{
		Code:      code,
		Period:    period,
		TradeDate: date,
		Open:      toDecimal(f[1]),
		Close:     toDecimal(f[2]),
		High:      toDecimal(f[3]),
		Low:       toDecimal(f[4]),
		Volume:    p.toShares(toDecimal(f[5]), code.Market),
		// 成交额本来就是元，和 Tushare 的「千元」不同，这里不做换算。
		Amount: toDecimal(f[6]),
		// 与 eastmoneyFqt = 0 对应。改一个必须改另一个。
		Adjusted: false,
		Source:   p.Name(),
	}, true
}

// ---------------------------------------------------------------------------
// 端口里东财不提供的部分
// ---------------------------------------------------------------------------

// FetchFinancials 东财的财务数据在另一套 emweb 接口上，报文结构与这里完全不同，
// 且分「按报告期」「按年度」多张表。先不实现，交给降级链里的 Tushare/Finnhub。
// FetchKlinesByDate 东财的 K 线接口（stock/kline/get）必须逐只查询，没有「某天全市场」
// 这种形态的端点。如实声明没有这个能力，让降级链把它交给 tushare。
func (p *EastmoneyProvider) FetchKlinesByDate(
	_ context.Context, market shared_vo.Market, _ value_objects.Period, _ shared_vo.TradeDate,
) ([]value_objects.Kline, error) {
	return nil, custom_errors.Unavailable(
		"eastmoney 未提供按交易日的批量 K 线端点（市场 %s）", market,
	).Wrap(domain_services.ErrBatchUnsupported)
}

func (p *EastmoneyProvider) FetchFinancials(
	ctx context.Context, code shared_vo.StockCode, limit int,
) ([]value_objects.Financial, error) {
	return nil, custom_errors.Unavailable("eastmoney 暂不提供 %s 的财务数据", code.FullSymbol())
}

// 个股资讯走东财的站内搜索接口（search-api-web），与行情/K 线完全是另一套。
const (
	// 这个接口只接受 JSONP：不带 cb 参数会直接回 400。因此下面必须剥壳再解析。
	eastmoneySearchCallback = "cb"
	// cmsArticleWebOld 是资讯类结果集的键名。同一个接口还能搜股票、基金、公告，
	// 靠 type 数组区分，这里只要资讯。
	eastmoneySearchNewsType = "cmsArticleWebOld"
	// 单次最多取多少条。刻意不翻页：资讯是**逐标的**同步的，五千多只票每多翻一页
	// 就是多五千次请求，而东财这边限速在 2 次/秒。一页拿最新的若干条已经够用——
	// 回看窗口默认只有 7 天，真正漏掉的是「一只票七天内发了超过一页的新闻」，
	// 那种票本来就该靠实时订阅而不是定时全量同步来覆盖。
	eastmoneySearchMaxPageSize = 50
)

// eastmoneySearchRequest 是 param 参数里那坨 JSON 的形状。
// 字段名必须和东财对齐，少一个都会被判 400。
type eastmoneySearchRequest struct {
	UID           string                          `json:"uid"`
	Keyword       string                          `json:"keyword"`
	Type          []string                        `json:"type"`
	Client        string                          `json:"client"`
	ClientType    string                          `json:"clientType"`
	ClientVersion string                          `json:"clientVersion"`
	Param         map[string]eastmoneySearchScope `json:"param"`
}

type eastmoneySearchScope struct {
	SearchScope string `json:"searchScope"`
	Sort        string `json:"sort"`
	PageIndex   int    `json:"pageIndex"`
	PageSize    int    `json:"pageSize"`
	// PreTag / PostTag 是命中关键词的高亮标签。置空是必须的，不是省事：
	// 默认值是 <em></em>，东财会把它们直接插进 title 与 content 中间，
	// 落库之后前端与 LLM 拿到的就是一段夹着 HTML 标签的正文。
	PreTag  string `json:"preTag"`
	PostTag string `json:"postTag"`
}

type eastmoneySearchResponse struct {
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
	Result struct {
		Articles []eastmoneyNewsItem `json:"cmsArticleWebOld"`
	} `json:"result"`
}

type eastmoneyNewsItem struct {
	Date      string `json:"date"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	MediaName string `json:"mediaName"`
	URL       string `json:"url"`
}

// FetchNews 拉取个股资讯。
//
// # 只支持 A 股
//
// 这个搜索接口按中文关键词检索东财自己的资讯库，用六位代码搜 A 股效果很好；
// 港股美股的代码在它库里几乎搜不出对应资讯。与其返回一堆不相干的结果，
// 不如如实声明只覆盖 CN——降级链会继续往下找别的源。
func (p *EastmoneyProvider) FetchNews(
	ctx context.Context, code shared_vo.StockCode, r shared_vo.DateRange, limit int,
) ([]value_objects.News, error) {
	if code.Market != shared_vo.MarketCN {
		return nil, custom_errors.Unavailable("eastmoney 的资讯搜索只覆盖 A 股，不提供 %s 的资讯", code.FullSymbol())
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > eastmoneySearchMaxPageSize {
		limit = eastmoneySearchMaxPageSize
	}

	param, err := json.Marshal(eastmoneySearchRequest{
		// 关键词用六位代码而不是股票名：名称会重（"中国银行"能搜出一堆行业新闻），
		// 而代码在正文里出现基本就意味着这条资讯确实在讲这只票。
		Keyword:       code.Symbol,
		Type:          []string{eastmoneySearchNewsType},
		Client:        "web",
		ClientType:    "web",
		ClientVersion: "curr",
		Param: map[string]eastmoneySearchScope{
			eastmoneySearchNewsType: {
				SearchScope: "default",
				// 按时间倒序而不是相关度：同步要的是「最近发生了什么」，
				// 默认的相关度排序会把几个月前的旧文顶到前面。
				Sort:      "time",
				PageIndex: 1,
				PageSize:  limit,
			},
		},
	})
	if err != nil {
		return nil, custom_errors.Internal("序列化 eastmoney 搜索参数失败").Wrap(err)
	}

	var out eastmoneySearchResponse
	err = p.getJSONP(ctx, p.searchHost+"/search/jsonp", url.Values{
		"cb":    {eastmoneySearchCallback},
		"param": {string(param)},
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, custom_errors.Unavailable("eastmoney 资讯搜索返回 code=%d: %s", out.Code, truncate(out.Msg, 100))
	}

	items := make([]value_objects.News, 0, len(out.Result.Articles))
	for _, a := range out.Result.Articles {
		published, ok := parseEastmoneyNewsTime(a.Date)
		if !ok {
			continue
		}
		// 接口没有日期过滤参数，只能在本地按回看窗口裁。
		// 不裁的话，一只冷门票会把几个月前的旧闻一路带进当天的分析上下文。
		if !withinRange(published, r) {
			continue
		}
		// URL 是资讯的去重依据，没有它落库会 upsert 出一条 url="" 的黑洞文档。
		if a.URL == "" || a.Title == "" {
			continue
		}
		source := a.MediaName
		if source == "" {
			source = p.Name()
		}
		items = append(items, value_objects.News{
			Code:        code,
			Title:       a.Title,
			Content:     a.Content,
			Source:      source,
			URL:         a.URL,
			PublishedAt: published,
		})
	}
	return items, nil
}

// parseEastmoneyNewsTime 解析 "2006-01-02 15:04:05"。
// 接口返回的是北京时间且不带时区，按 UTC 解析会整体偏移 8 小时——
// 那会让「最近 7 天」的窗口在每天早上 8 点前少算一天。
func parseEastmoneyNewsTime(s string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(s), beijing)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// withinRange 判断发布时间是否落在回看窗口内（闭区间，按日粒度比较）。
func withinRange(t time.Time, r shared_vo.DateRange) bool {
	day := t.In(beijing).Format("2006-01-02")
	if s := r.Start.String(); s != "" && day < s {
		return false
	}
	if e := r.End.String(); e != "" && day > e {
		return false
	}
	return true
}

// getJSONP 发一次 GET 并把 JSONP 外壳剥掉再解析。
//
// 这个接口不接受纯 JSON：不带 cb 参数直接回 400，所以剥壳是必经步骤而不是兼容处理。
func (p *EastmoneyProvider) getJSONP(ctx context.Context, endpoint string, q url.Values, out any) error {
	raw, err := p.getRaw(ctx, endpoint, q)
	if err != nil {
		return err
	}
	// 按第一个 '(' 与最后一个 ')' 剥壳，而不是按固定前缀长度切：
	// 回调名是我们自己传的，但万一将来换了名字，按长度切会静默错位。
	open := bytes.IndexByte(raw, '(')
	closeAt := bytes.LastIndexByte(raw, ')')
	if open < 0 || closeAt <= open {
		return custom_errors.Unavailable("eastmoney 资讯响应不是 JSONP: %s", truncate(string(raw), 200))
	}
	payload := raw[open+1 : closeAt]

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return custom_errors.Unavailable("解析 eastmoney 资讯响应失败: %s", truncate(string(payload), 200)).Wrap(err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// getJSON 发一次 GET 并解码 JSON。
func (p *EastmoneyProvider) getJSON(ctx context.Context, endpoint string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return custom_errors.Internal("构造 eastmoney 请求失败").Wrap(err)
	}
	// 东财对没有 UA 的请求偶尔直接拒绝。带一个普通浏览器 UA 不是伪装，
	// 是这个公开接口事实上的调用约定（AKShare 也是这么发的）。
	req.Header.Set("User-Agent", eastmoneyUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")

	raw, err := p.doRequest(req)
	if err != nil {
		return err
	}
	// UseNumber 的理由和 tushare.go 里一样：价格与市值落进 any 之后，
	// 默认解码会先变成 float64，精度在 toDecimal 看到它之前就已经丢了。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return custom_errors.Unavailable("解析 eastmoney 响应失败: %s", truncate(string(raw), 200)).Wrap(err)
	}
	return nil
}

// getRaw 发一次 GET 并返回原始响应体，供需要自己处理外壳的调用方使用（JSONP）。
func (p *EastmoneyProvider) getRaw(ctx context.Context, endpoint string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, custom_errors.Internal("构造 eastmoney 请求失败").Wrap(err)
	}
	req.Header.Set("User-Agent", eastmoneyUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	// 搜索接口会校验来源：不带 Referer 时它会回 400。和 UA 一样，
	// 这是这个公开接口事实上的调用约定，不是伪装。
	req.Header.Set("Referer", "https://so.eastmoney.com/")
	return p.doRequest(req)
}

// doRequest 是两条路径共用的发送与错误处理。
func (p *EastmoneyProvider) doRequest(req *http.Request) ([]byte, error) {
	resp, err := p.http.Do(req)
	if err != nil {
		// 限流在这里表现为空响应（EOF）。RedactError 只换掉 URL，
		// 底层错误原样保留，上层仍可 errors.Is 判断取消。
		return nil, custom_errors.Unavailable("调用 eastmoney 失败").Wrap(httpx.RedactError(err))
	}
	defer resp.Body.Close()

	// clist 一页约 40KB，K 线全量约 1MB。8MB 上限足够宽松，
	// 同时挡住异常响应（比如网关返回的 HTML 错误页）把内存撑爆。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, custom_errors.Unavailable("读取 eastmoney 响应失败").Wrap(httpx.RedactError(err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, custom_errors.Unavailable("eastmoney 返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// eastmoneyUserAgent 是一个普通桌面浏览器的 UA。
const eastmoneyUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// toStockCode 把 clist 的一行转成规范化代码。
//
// 美股要先把下划线换成连字符：东财用 BRK_A 表示 BRK.A，而领域的美股代码正则
// 不收下划线。不换的话这类票会被整批静默丢掉。
func (p *EastmoneyProvider) toStockCode(row spotRow, market shared_vo.Market) (shared_vo.StockCode, bool) {
	symbol := strings.TrimSpace(row.Code)
	if symbol == "" {
		return shared_vo.StockCode{}, false
	}
	if market == shared_vo.MarketUS {
		symbol = strings.ReplaceAll(symbol, "_", "-")
	}
	code, err := shared_vo.NewStockCode(symbol, market)
	if err != nil {
		// 指数、权证、优先股这类不符合领域规则的代码直接跳过。
		return shared_vo.StockCode{}, false
	}
	return code, true
}

// tradeDateOf 从 f124（行情时间，unix 秒）推交易日。
//
// **绝不能用 time.Now() 代替**：非交易日 clist 照样返回上一个交易日的数据，
// 盖上今天的日期就会造出一条幽灵记录，而 LatestQuote 是按 trade_date 倒序取的，
// 那条幽灵会被永远返回。美股更明显——它的交易时段跨越亚洲的午夜。
func (p *EastmoneyProvider) tradeDateOf(row spotRow) (shared_vo.TradeDate, bool) {
	sec, err := strconv.ParseInt(toString(row.QuoteTime), 10, 64)
	if err != nil || sec <= 0 {
		return shared_vo.TradeDate{}, false
	}
	// 东财的时间戳是北京时间语义，用固定东八区换算，不跟随服务器时区——
	// 否则同一份数据在不同部署环境里会落到不同的交易日。
	t := time.Unix(sec, 0).In(beijing)
	date := shared_vo.TradeDateOf(t)
	// 未来日期一定是脏数据，宁可丢掉也不能写进去：它同样会污染「最新行情」。
	if date.String() > shared_vo.Today().String() {
		return shared_vo.TradeDate{}, false
	}
	return date, true
}

var beijing = time.FixedZone("CST", 8*60*60)

// toShares 把成交量统一换算成「股」。
//
// A 股与港股的 clist / K 线给的都是「手」（1 手 = 100 股），美股本来就是股。
// 照抄 Tushare 那样无条件乘 100，会让美股的成交量全部大 100 倍——
// 而成交量是量比、换手这些因子的输入，错了不会报错，只会让选股结果悄悄失真。
func (p *EastmoneyProvider) toShares(v decimal.Decimal, market shared_vo.Market) decimal.Decimal {
	if market == shared_vo.MarketUS {
		return v
	}
	return v.Mul(hundred)
}

// secid 拼 K 线接口要的 {市场号}.{代码}。
//
// 这里从 (market, symbol) 重新派生，**不复用 clist 的 f13**：两者不是同一个命名空间。
// 港股在 clist 里报 f13=128，而 K 线接口要的前缀是 116；A 股与美股虽然目前两边一致，
// 但没有任何文档保证这一点。集中在这一个函数里派生，改起来只有一处。
func (p *EastmoneyProvider) secid(code shared_vo.StockCode) (string, error) {
	switch code.Market {
	case shared_vo.MarketCN:
		// 6 开头是上交所（含科创板 688），其余（含创业板 300、北交所 4/8）都在深证集群下。
		if strings.HasPrefix(code.Symbol, "6") {
			return "1." + code.Symbol, nil
		}
		return "0." + code.Symbol, nil
	case shared_vo.MarketHK:
		return "116." + code.Symbol, nil
	case shared_vo.MarketUS:
		// 美股的交易所号（105/106/107）无法从代码本身推出来，只有 clist 的 f13 知道。
		// 现阶段美股 K 线走 Finnhub，这里明确报错而不是猜一个前缀——
		// 猜错的表现是「返回空集」，比报错难查得多。
		return "", custom_errors.Unavailable("eastmoney 暂不支持美股 K 线（缺少交易所编号）")
	}
	return "", custom_errors.Invalid("eastmoney 不支持市场 %s", code.Market)
}

// logDropped 记录本次扫描丢弃了多少脏行。
//
// 「跳过脏行」是合理的，「跳过四千行」是 bug。不记这一笔，两者在日志里长得一样。
func (p *EastmoneyProvider) logDropped(ctx context.Context, op string, market shared_vo.Market, total, dropped int) {
	if dropped == 0 {
		return
	}
	logger.FromContext(ctx).Warn("eastmoney 跳过无法解析的行",
		zap.String("op", op), zap.String("market", market.String()),
		zap.Int("dropped", dropped), zap.Int("total", total))
}

// compactOr 把交易日转成 YYYYMMDD，零值时用兜底值。
func compactOr(d shared_vo.TradeDate, fallback string) string {
	if v := d.Compact(); v != "" {
		return v
	}
	return fallback
}
