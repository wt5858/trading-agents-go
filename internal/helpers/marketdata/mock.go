package marketdata

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// MockProvider 是全市场可用的假数据源，唯一目的是让整条链路在没有任何 API Key
// 的情况下也能端到端跑起来（本地开发、CI、演示）。
//
// !!! 仅供开发调试使用，严禁在生产环境注册到 Composite 中 !!!
// 它产出的所有价格、财务、资讯都是程序生成的，不具备任何投资参考价值。
//
// 设计上刻意不用 math/rand 的全局状态：全局种子会让同一个请求在不同进程/不同调用
// 顺序下拿到不同结果，调试时无法复现。这里所有随机数都由「标的代码 + 用途」
// 哈希出的确定性种子驱动，因此同样的输入永远得到同样的输出。
type MockProvider struct{}

var _ domain_services.DataProvider = (*MockProvider)(nil)

// NewMockProvider 构造开发用假数据源。
func NewMockProvider() *MockProvider { return &MockProvider{} }

func (p *MockProvider) Name() string { return "mock" }

// Supports 假数据源覆盖全部合法市场，作为降级链的最后一环兜底。
func (p *MockProvider) Supports(market shared_vo.Market) bool { return market.Valid() }

// ---------------------------------------------------------------------------
// 确定性伪随机
// ---------------------------------------------------------------------------

// detRand 是 xorshift64* 伪随机数发生器。选它而不是 math/rand 是因为：
// 实现只有几行、无锁、无全局状态，且同一种子跨 Go 版本结果稳定
// （math/rand 的算法在 Go 1.20+ 有过调整，不适合做可复现的测试基线）。
type detRand struct{ s uint64 }

// newDetRand 用 FNV-1a 把字符串哈希成种子。种子为 0 时 xorshift 会退化成恒 0，
// 所以强制替换成一个非零常数。
func newDetRand(parts ...string) *detRand {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0x1f}) // 分隔符，避免 ("ab","c") 与 ("a","bc") 撞种子
	}
	seed := h.Sum64()
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &detRand{s: seed}
}

func (r *detRand) next() uint64 {
	r.s ^= r.s >> 12
	r.s ^= r.s << 25
	r.s ^= r.s >> 27
	return r.s * 2685821657736338717
}

// float 返回 [0,1)。
func (r *detRand) float() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

// between 返回 [lo,hi)。
func (r *detRand) between(lo, hi float64) float64 { return lo + r.float()*(hi-lo) }

// norm 用 Irwin–Hall 近似标准正态（12 个均匀分布求和减 6，方差恰好为 1）。
// K 线的随机游走需要正态扰动，否则价格序列看起来像方波而不像行情。
func (r *detRand) norm() float64 {
	sum := 0.0
	for i := 0; i < 12; i++ {
		sum += r.float()
	}
	return sum - 6
}

func (r *detRand) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// basePrice 由代码哈希出一个「像那么回事」的基准价：
// A 股多在 5~200 元，美股 20~500 美元，港股 3~300 港元。
func basePrice(code shared_vo.StockCode) float64 {
	r := newDetRand("base", code.FullSymbol())
	switch code.Market {
	case shared_vo.MarketCN:
		return round2(r.between(5, 200))
	case shared_vo.MarketHK:
		return round2(r.between(3, 300))
	default:
		return round2(r.between(20, 500))
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// dec2 是模拟器与领域模型之间的边界转换。
//
// 随机游走本身（对数收益、正态扰动、指数衰减）继续用 float64：
// 它生成的是假数据，没有任何账目意义，而 math.Exp/Sqrt 这些函数也只有浮点版本。
// 但**交给领域模型的那一刻**必须是 decimal——否则 mock 会产出真实数据源
// 不可能给出的值（比如 12.340000000000001 的收盘价），
// 让指标计算在测试环境里表现得和生产环境不一样。
func dec2(v float64) decimal.Decimal { return decimal.NewFromFloat(v).Round(2) }

// ---------------------------------------------------------------------------
// 接口实现
// ---------------------------------------------------------------------------

// mockNames 各市场的样例标的。用真实代码+名称而不是随机字符串，
// 是为了让前端调试时看到的列表可读、可搜索。
var mockNames = map[shared_vo.Market][][2]string{
	shared_vo.MarketCN: {
		{"600519", "贵州茅台"}, {"000001", "平安银行"}, {"600036", "招商银行"},
		{"000858", "五粮液"}, {"601318", "中国平安"}, {"300750", "宁德时代"},
		{"002594", "比亚迪"}, {"600900", "长江电力"},
	},
	shared_vo.MarketHK: {
		{"00700", "腾讯控股"}, {"09988", "阿里巴巴-SW"}, {"00939", "建设银行"},
		{"01299", "友邦保险"}, {"03690", "美团-W"}, {"00388", "香港交易所"},
	},
	shared_vo.MarketUS: {
		{"AAPL", "Apple Inc."}, {"MSFT", "Microsoft Corp."}, {"NVDA", "NVIDIA Corp."},
		{"GOOGL", "Alphabet Inc."}, {"AMZN", "Amazon.com Inc."}, {"TSLA", "Tesla Inc."},
	},
}

var mockIndustries = []string{"白酒", "银行", "保险", "半导体", "新能源", "医药生物", "软件服务", "电力设备"}

// FetchStockList 返回该市场的样例标的列表。
func (p *MockProvider) FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(market) {
		return nil, custom_errors.Invalid("非法市场: %s", market)
	}

	seeds := mockNames[market]
	list := make([]*entities.Stock, 0, len(seeds))
	for _, item := range seeds {
		code, err := shared_vo.NewStockCode(item[0], market)
		if err != nil {
			continue
		}
		r := newDetRand("list", code.FullSymbol())
		listDate := time.Date(1995+r.intn(28), time.Month(1+r.intn(12)), 1+r.intn(28), 0, 0, 0, 0, time.UTC)
		// 单位是**元**，和东财的 f20/f21 对齐。
		//
		// 原先这里生成的是「亿元」，和真实源差了 1e8。选股的 market_cap 过滤直接读
		// stocks.total_mv 跨市场排序比较，两种单位混在一张表里会让任何阈值筛选静默失真：
		// 不报错，只是筛出来的票不对。假数据源尤其不能在单位上偷懒——
		// 它的全部价值就是让本地跑出来的东西和线上是一回事。
		totalMV := round2(r.between(100, 20000)) * 1e8
		// 走 entities.List 而不是直接拼结构体，这样假数据也必须满足
		// 「流通市值 <= 总市值」这类真实不变式，否则 mock 会生成真实源不可能产出的数据。
		s, err := entities.List(entities.ListParams{
			Code:     code,
			Name:     item[1],
			Industry: mockIndustries[r.intn(len(mockIndustries))],
			Area:     "MOCK",
			ListDate: &listDate,
			TotalMV:  dec2(totalMV),
			CircMV:   dec2(totalMV * r.between(0.5, 1.0)), // 流通市值必然小于等于总市值
			Source:   p.Name(),
		})
		if err != nil {
			continue
		}
		list = append(list, s)
	}
	return list, nil
}

// FetchQuote 返回最近一个交易日的快照，数据取自同一套随机游走，
// 保证 Quote 与 FetchKlines 的最后一根 K 线口径一致（同种子 → 同序列）。
func (p *MockProvider) FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("非法市场: %s", code.Market)
	}

	klines := p.walk(code, value_objects.PeriodDaily, shared_vo.LastNDays(60))
	if len(klines) == 0 {
		return nil, custom_errors.NotFound("mock 无 %s 的行情", code.FullSymbol())
	}
	last := klines[len(klines)-1]
	preClose := last.Open
	if len(klines) >= 2 {
		preClose = klines[len(klines)-2].Close
	}

	r := newDetRand("quote", code.FullSymbol())
	// last.Close 与 preClose 已经是 decimal，涨跌额与涨跌幅直接用 decimal 算，
	// 不再退回浮点绕一圈。
	change := decimalx.RoundPrice(last.Close.Sub(preClose))
	changePct := decimalx.PercentChange(last.Close, preClose)

	return &value_objects.Quote{
		Code:      code,
		TradeDate: last.TradeDate,
		Open:      last.Open,
		High:      last.High,
		Low:       last.Low,
		Close:     last.Close,
		PreClose:  preClose,
		Change:    change,
		ChangePct: changePct,
		Volume:    last.Volume,
		Amount:    last.Amount,
		Turnover:  dec2(r.between(0.2, 5.0)),
		PE:        dec2(r.between(8, 60)),
		PB:        dec2(r.between(0.8, 12)),
		Source:    p.Name(),
		UpdatedAt: time.Now(),
	}, nil
}

// FetchQuotes 返回该市场全部样例标的的快照。
//
// 假数据源**必须**真实现这个方法，不能跟 tushare/finnhub 一样返回哨兵。
// 理由是执行路径覆盖：一把密钥都没配时整条链只有 mock，如果它也说「不支持批量」，
// 那本地和 CI 跑的永远是逐标的回退路径，批量路径第一次执行就在生产环境。
//
// 这里的循环不是 N+1：样例清单只有几只，且 FetchQuote 不做任何 IO。
func (p *MockProvider) FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(market) {
		return nil, custom_errors.Invalid("非法市场: %s", market)
	}

	seeds := mockNames[market]
	quotes := make([]value_objects.Quote, 0, len(seeds))
	for _, item := range seeds {
		code, err := shared_vo.NewStockCode(item[0], market)
		if err != nil {
			continue
		}
		q, err := p.FetchQuote(ctx, code)
		if err != nil || q == nil {
			continue
		}
		quotes = append(quotes, *q)
	}
	return quotes, nil
}

// FetchKlines 生成一段随机游走 K 线。
func (p *MockProvider) FetchKlines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, r shared_vo.DateRange) ([]value_objects.Kline, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("非法市场: %s", code.Market)
	}
	if !period.Valid() {
		return nil, custom_errors.Invalid("不支持的 K 线周期: %s", period)
	}
	return p.walk(code, period, r), nil
}

// walk 是 K 线生成的核心：几何布朗运动式的随机游走。
//
// 关键是保证 OHLC 的不变量 low <= min(open,close) <= max(open,close) <= high，
// 否则前端画蜡烛图会出现「上影线在实体下方」这种一眼假的图形，
// 技术指标计算（ATR、KDJ）也会算出负数。
func (p *MockProvider) walk(code shared_vo.StockCode, period value_objects.Period, rng shared_vo.DateRange) []value_objects.Kline {
	// DateRange 的两端已经是校验过的 TradeDate，这里只需要解不出来就放弃，
	// 不用再做格式判断。
	start, ok1 := rng.Start.Time()
	end, ok2 := rng.End.Time()
	if !ok1 || !ok2 || end.Before(start) {
		return nil
	}

	// 随机游走统一从固定锚点日推进，只输出落在查询区间内的部分。
	// 这样「某只票某一天的价格」是确定的：查近 30 天和查近 90 天，重叠段完全一致。
	// 如果直接从 rng.Start 起步，改一次时间范围整条曲线就变形，前端联调时根本对不上。
	anchor := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	if start.Before(anchor) {
		anchor = start
	}

	// 种子只含代码与周期、不含日期区间，配合固定锚点保证序列全局稳定。
	r := newDetRand("kline", code.FullSymbol(), string(period))
	price := basePrice(code)
	// 日线波动率约 1.8%，周线/月线按 sqrt(t) 放大，符合波动率随时间开方缩放的常识。
	vol := 0.018
	step := 24 * time.Hour
	switch period {
	case value_objects.PeriodWeekly:
		vol *= math.Sqrt(5)
		step = 7 * 24 * time.Hour
	case value_objects.PeriodMonthly:
		vol *= math.Sqrt(21)
		step = 30 * 24 * time.Hour
	}
	drift := r.between(-0.0004, 0.0008) // 轻微正漂移，长期看略微上行

	const (
		maxBars = 5000  // 输出上限，防止超大区间把内存打满
		maxIter = 40000 // 推进上限，兜住 start 被设成 1990 年这种极端入参
	)
	klines := make([]value_objects.Kline, 0, 256)
	iter := 0
	for d := anchor; !d.After(end) && len(klines) < maxBars && iter < maxIter; d = d.Add(step) {
		iter++
		// 只在日线上跳过周末；周线/月线本身就是聚合周期，不需要过滤。
		if period == value_objects.PeriodDaily && (d.Weekday() == time.Saturday || d.Weekday() == time.Sunday) {
			continue
		}

		open := round2(price)
		// 对数收益随机游走，保证价格恒为正。
		price = price * math.Exp(drift+vol*r.norm())
		if price < 0.5 {
			price = 0.5 // 防止长序列衰减成 0，0 价会让所有涨跌幅计算除零
		}
		closePx := round2(price)

		hi := math.Max(open, closePx)
		lo := math.Min(open, closePx)
		// 影线在实体之外单向延伸，天然满足 low <= open,close <= high。
		high := round2(hi * (1 + math.Abs(r.norm())*vol*0.5))
		low := round2(lo * (1 - math.Abs(r.norm())*vol*0.5))

		volume := math.Round(r.between(1e6, 5e7))

		// 区间之前的 bar 只用来推进游走与消耗随机数，不产出——这正是价格能跨查询稳定的原因。
		if d.Before(start) {
			continue
		}

		klines = append(klines, value_objects.Kline{
			Code:      code,
			Period:    period,
			TradeDate: shared_vo.TradeDateOf(d),
			Open:      dec2(open),
			High:      dec2(high),
			Low:       dec2(low),
			Close:     dec2(closePx),
			Volume:    dec2(volume),
			Amount:    dec2(volume * (high + low) / 2), // 成交额用均价估算，与量价自洽
			Adjusted:  true,
			Source:    p.Name(),
		})
	}
	return klines
}

// FetchFinancials 按季度倒推生成财报序列。
// FetchKlinesByDate mock 不提供批量端点：联调时真正要覆盖的是降级链退回逐标的
// 那条路径，假装支持反而会把它遮住。
func (p *MockProvider) FetchKlinesByDate(
	_ context.Context, market shared_vo.Market, _ value_objects.Period, _ shared_vo.TradeDate,
) ([]value_objects.Kline, error) {
	return nil, custom_errors.Unavailable(
		"mock 未提供按交易日的批量 K 线端点（市场 %s）", market,
	).Wrap(domain_services.ErrBatchUnsupported)
}

func (p *MockProvider) FetchFinancials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("非法市场: %s", code.Market)
	}
	if limit <= 0 {
		limit = 8
	}

	r := newDetRand("fina", code.FullSymbol())
	now := time.Now()
	revenue := r.between(5e8, 2e11) // 单位：元
	margin := r.between(0.05, 0.35)

	items := make([]value_objects.Financial, 0, limit)
	for i := 0; i < limit; i++ {
		// 从最近的季末往前推，i=0 是最新一期。
		end := quarterEnd(now.AddDate(0, -3*i, 0))
		periodType := value_objects.PeriodTypeQuarter
		if end.Month() == time.December {
			periodType = value_objects.PeriodTypeAnnual
		}
		// 越往前营收越低，制造出一条增长曲线（而不是毫无规律的噪声）。
		rev := revenue * math.Pow(0.97, float64(i)) * (1 + 0.05*r.norm()/10)
		net := rev * margin * (1 + 0.1*r.norm()/10)
		items = append(items, value_objects.Financial{
			Code:        code,
			ReportDate:  shared_vo.TradeDateOf(end),
			PeriodType:  periodType,
			Revenue:     dec2(rev),
			NetProfit:   dec2(net),
			EPS:         dec2(net / r.between(1e8, 2e10)),
			PE:          dec2(r.between(8, 60)),
			PB:          dec2(r.between(0.8, 12)),
			ROE:         dec2(r.between(3, 30)),              // 百分数口径，与真实源一致
			GrossMargin: dec2(margin*100 + r.between(5, 25)), // 毛利率必然高于净利率
			// 假数据源和真实源一样，把净利率当成一个独立产出的指标交出去，
			// 而不是留给读路径用 NetProfit/Revenue 去除——那样 mock 就复现不了
			// 「源给的比率和绝对值对不上」这种线上真实存在的情况。
			NetMargin: dec2(margin * 100),
			DebtRatio: dec2(r.between(20, 75)),
			Source:    p.Name(),
			UpdatedAt: now,
		})
	}
	return items, nil
}

var mockHeadlines = []string{
	"%s发布最新季度业绩，营收同比增长超预期",
	"机构调研密集关注%s，多家券商上调评级",
	"%s宣布新一轮产能扩张计划",
	"行业景气度回暖，%s订单饱满",
	"%s管理层增持股份，释放积极信号",
	"分析师下调%s目标价，关注短期回调风险",
	"%s公布股东回报方案，拟提高分红比例",
	"监管新规落地，%s所处赛道格局生变",
}

// FetchNews 生成样例资讯。情感分与标题倾向无关联（纯随机），
// 因此不要拿它去验证情感分析链路的正确性。
func (p *MockProvider) FetchNews(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]value_objects.News, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !p.Supports(code.Market) {
		return nil, custom_errors.Invalid("非法市场: %s", code.Market)
	}
	if limit <= 0 {
		limit = 10
	}

	// 区间末端未指定时退化为今天：资讯是「截至某日往前推」的序列，没有末端就没有起点。
	end, ok := rng.End.Time()
	if !ok {
		end = time.Now()
	}

	r := newDetRand("news", code.FullSymbol())
	items := make([]value_objects.News, 0, limit)
	for i := 0; i < limit; i++ {
		tpl := mockHeadlines[r.intn(len(mockHeadlines))]
		published := end.AddDate(0, 0, -i).Add(-time.Duration(r.intn(24)) * time.Hour)
		items = append(items, value_objects.News{
			Code:        code,
			Title:       fmt.Sprintf(tpl, code.Symbol),
			Content:     fmt.Sprintf("【模拟数据】这是为 %s 生成的开发测试资讯正文，不代表任何真实信息。", code.FullSymbol()),
			Source:      "mock-news",
			URL:         "https://example.invalid/news/" + code.Symbol,
			PublishedAt: published,
			Sentiment:   value_objects.NewSentiment(dec2(r.between(-1, 1))),
		})
	}
	return items, nil
}

// quarterEnd 返回 t 所在季度的最后一天。
func quarterEnd(t time.Time) time.Time {
	q := (int(t.Month())-1)/3 + 1
	month := time.Month(q * 3)
	// 下个月 1 号减一天，自动处理 30/31 天与闰年。
	return time.Date(t.Year(), month+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
}

// ctxErr 把 ctx 的取消状态转换成领域错误。假数据源不做 IO，
// 但仍需响应取消，否则大区间生成会在调用方超时后继续空转。
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return custom_errors.Unavailable("请求已取消").Wrap(err)
	}
	return nil
}
