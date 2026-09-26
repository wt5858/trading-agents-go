package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 本文件是 MongoDB 侧全部读模型的持久化对象与映射。
//
// 为什么必须有这一层，而不是直接给 value_objects.Quote 之流打 bson 标签：
//
//  1. shared_vo.TradeDate 的内部字段不导出，bson 编解码器看不见它，直接序列化会得到 {}；
//     shared_vo.StockCode 虽然字段导出，但它是个嵌套结构，落成子文档后
//     symbol 就变成了 code.symbol，现有的 uk_quotes_symbol_date 索引直接失效。
//  2. 给领域 VO 加 bson 标签等于把存储格式钉死在领域层，以后换存储要动领域模型。
//
// 价量列是 decimal.Decimal，落库类型为 BSON Decimal128。这依赖 internal/db 里注册的
// 编解码器——shopspring/decimal 没有实现 bson.ValueMarshaler，不注册的话驱动会把它
// 当普通结构体反射未导出字段，静默写进一个空文档 {}，编译期毫无提示。
// 换句话说：这些列能正确落库，前提是 Mongo 客户端是经 db.Open 建的。
//
// 所以 DTO 一律存「原始形态」：symbol/market/raw 三个扁平字符串列 + trade_date 定长串，
// 转换只发生在 ToDomain / FromDomainXxx 里。
//
// trade_date 存 YYYY-MM-DD 定长串而不是 BSON date：定长串的字典序与时间序等价，
// 区间查询可以直接 $gte/$lte，省掉时区换算，也让索引前缀压缩更有效。

// codeColumns 是 StockCode 在 Mongo 文档里的扁平形态。
// 用内嵌结构体复用到五个 DTO 上，保证五个集合的字段名严格一致——
// 一旦某个集合把 symbol 写成 stock_symbol，跨集合的聚合查询就全废了。
type codeColumns struct {
	Symbol string `bson:"symbol"`
	Market string `bson:"market"`
	Raw    string `bson:"raw_code,omitempty"`
}

func codeColumnsOf(c shared_vo.StockCode) codeColumns {
	return codeColumns{Symbol: c.Symbol, Market: string(c.Market), Raw: c.Raw}
}

// toDomain 直接拼装而不走 shared_vo.NewStockCode：库里的数据是既成事实，
// 读路径再跑一次写入期的校验，只会让历史脏数据把整个查询打挂。
func (c codeColumns) toDomain() shared_vo.StockCode {
	return shared_vo.StockCode{Symbol: c.Symbol, Market: shared_vo.Market(c.Market), Raw: c.Raw}
}

// ---------------------------------------------------------------------------
// 行情快照
// ---------------------------------------------------------------------------

// QuoteDto 对应 quotes 集合。自然键 = (symbol, trade_date)。
//
// change / change_pct / amount / turnover 都各自占一列落库。
// 它们是数据源算好的派生量，存下来读回去；读路径上没有任何一处会用
// close-pre_close 或 close*volume 重新推导——重算的结果和源口径对不上，
// 而下游无从分辨自己拿到的是哪一种。
type QuoteDto struct {
	codeColumns `bson:",inline"`
	TradeDate   string          `bson:"trade_date"`
	Open        decimal.Decimal `bson:"open"`
	High        decimal.Decimal `bson:"high"`
	Low         decimal.Decimal `bson:"low"`
	Close       decimal.Decimal `bson:"close"`
	PreClose    decimal.Decimal `bson:"pre_close"`
	Change      decimal.Decimal `bson:"change"`
	ChangePct   decimal.Decimal `bson:"change_pct"`
	Volume      decimal.Decimal `bson:"volume"`
	Amount      decimal.Decimal `bson:"amount"`
	Turnover    decimal.Decimal `bson:"turnover"`
	PE          decimal.Decimal `bson:"pe"`
	PB          decimal.Decimal `bson:"pb"`
	Source      string          `bson:"source"`
	UpdatedAt   time.Time       `bson:"updated_at"`
}

func (dto QuoteDto) ToDomain() value_objects.Quote {
	return value_objects.Quote{
		Code:      dto.toDomain(),
		TradeDate: shared_vo.MustTradeDate(dto.TradeDate),
		Open:      dto.Open,
		High:      dto.High,
		Low:       dto.Low,
		Close:     dto.Close,
		PreClose:  dto.PreClose,
		Change:    dto.Change,
		ChangePct: dto.ChangePct,
		Volume:    dto.Volume,
		Amount:    dto.Amount,
		Turnover:  dto.Turnover,
		PE:        dto.PE,
		PB:        dto.PB,
		Source:    dto.Source,
		UpdatedAt: dto.UpdatedAt,
	}
}

func FromDomainQuote(q value_objects.Quote) *QuoteDto {
	return &QuoteDto{
		codeColumns: codeColumnsOf(q.Code),
		TradeDate:   q.TradeDate.String(),
		Open:        q.Open,
		High:        q.High,
		Low:         q.Low,
		Close:       q.Close,
		PreClose:    q.PreClose,
		Change:      q.Change,
		ChangePct:   q.ChangePct,
		Volume:      q.Volume,
		Amount:      q.Amount,
		Turnover:    q.Turnover,
		PE:          q.PE,
		PB:          q.PB,
		Source:      q.Source,
		UpdatedAt:   q.UpdatedAt,
	}
}

func ToDomainQuotes(rows []QuoteDto) []value_objects.Quote {
	out := make([]value_objects.Quote, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

// ---------------------------------------------------------------------------
// K 线
// ---------------------------------------------------------------------------

// KlineDto 对应 klines 集合。自然键 = (symbol, period, trade_date)。
type KlineDto struct {
	codeColumns `bson:",inline"`
	Period      string          `bson:"period"`
	TradeDate   string          `bson:"trade_date"`
	Open        decimal.Decimal `bson:"open"`
	High        decimal.Decimal `bson:"high"`
	Low         decimal.Decimal `bson:"low"`
	Close       decimal.Decimal `bson:"close"`
	Volume      decimal.Decimal `bson:"volume"`
	// amount 独立落列，而不是查询时用 close*volume 算。
	// 没有成交额的数据源就存 0，让缺失可见，好过存一个看起来合理的假值。
	Amount   decimal.Decimal `bson:"amount"`
	Adjusted bool            `bson:"adjusted"`
	Source   string          `bson:"source"`
}

func (dto KlineDto) ToDomain() value_objects.Kline {
	return value_objects.Kline{
		Code:      dto.toDomain(),
		Period:    value_objects.Period(dto.Period),
		TradeDate: shared_vo.MustTradeDate(dto.TradeDate),
		Open:      dto.Open,
		High:      dto.High,
		Low:       dto.Low,
		Close:     dto.Close,
		Volume:    dto.Volume,
		Amount:    dto.Amount,
		Adjusted:  dto.Adjusted,
		Source:    dto.Source,
	}
}

func FromDomainKline(k value_objects.Kline) *KlineDto {
	return &KlineDto{
		codeColumns: codeColumnsOf(k.Code),
		Period:      k.Period.String(),
		TradeDate:   k.TradeDate.String(),
		Open:        k.Open,
		High:        k.High,
		Low:         k.Low,
		Close:       k.Close,
		Volume:      k.Volume,
		Amount:      k.Amount,
		Adjusted:    k.Adjusted,
		Source:      k.Source,
	}
}

func ToDomainKlines(rows []KlineDto) []value_objects.Kline {
	out := make([]value_objects.Kline, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

// ---------------------------------------------------------------------------
// 财务
// ---------------------------------------------------------------------------

// FinancialDto 对应 financials 集合。自然键 = (symbol, report_date)。
//
// net_margin 单独占一列：它是数据源直接给出的比率指标，存下来读回去，
// 读路径不再用 net_profit / revenue 现算。Tushare 的 fina_indicator 表
// 本来就没有稳定的绝对营收/净利字段，现算只会得到 0 这个假值。
type FinancialDto struct {
	codeColumns `bson:",inline"`
	ReportDate  string `bson:"report_date"`
	// AnnounceDate 是披露日，omitempty 因为老数据没有这一列。
	// 它与 ReportDate 的区别见 value_objects.Financial 的字段说明——
	// 回测能不能用这份财报，取决于它而不是报告期。
	AnnounceDate string          `bson:"announce_date,omitempty"`
	PeriodType   string          `bson:"period_type"`
	Revenue      decimal.Decimal `bson:"revenue"`
	NetProfit    decimal.Decimal `bson:"net_profit"`
	EPS          decimal.Decimal `bson:"eps"`
	PE           decimal.Decimal `bson:"pe"`
	PB           decimal.Decimal `bson:"pb"`
	ROE          decimal.Decimal `bson:"roe"`
	GrossMargin  decimal.Decimal `bson:"gross_margin"`
	NetMargin    decimal.Decimal `bson:"net_margin"`
	DebtRatio    decimal.Decimal `bson:"debt_ratio"`
	Source       string          `bson:"source"`
	UpdatedAt    time.Time       `bson:"updated_at"`
}

// tradeDateOrZero 把可能为空的日期串转成交易日，空串得到零值。
//
// 不能直接用 MustTradeDate：它对空串 panic，而 announce_date 这一列
// 在老数据上本来就不存在——读一条 2024 年入库的财报就会打崩整个查询。
func tradeDateOrZero(s string) shared_vo.TradeDate {
	if s == "" {
		return shared_vo.TradeDate{}
	}
	return shared_vo.MustTradeDate(s)
}

func (dto FinancialDto) ToDomain() value_objects.Financial {
	return value_objects.Financial{
		Code:       dto.toDomain(),
		ReportDate: shared_vo.MustTradeDate(dto.ReportDate),
		// 空串走零值：MustTradeDate 对空串会 panic，而老数据这一列本就是空的。
		AnnounceDate: tradeDateOrZero(dto.AnnounceDate),
		PeriodType:   value_objects.PeriodType(dto.PeriodType),
		Revenue:      dto.Revenue,
		NetProfit:    dto.NetProfit,
		EPS:          dto.EPS,
		PE:           dto.PE,
		PB:           dto.PB,
		ROE:          dto.ROE,
		GrossMargin:  dto.GrossMargin,
		NetMargin:    dto.NetMargin,
		DebtRatio:    dto.DebtRatio,
		Source:       dto.Source,
		UpdatedAt:    dto.UpdatedAt,
	}
}

func FromDomainFinancial(f value_objects.Financial) *FinancialDto {
	return &FinancialDto{
		codeColumns:  codeColumnsOf(f.Code),
		ReportDate:   f.ReportDate.String(),
		AnnounceDate: f.AnnounceDate.String(),
		PeriodType:   f.PeriodType.String(),
		Revenue:      f.Revenue,
		NetProfit:    f.NetProfit,
		EPS:          f.EPS,
		PE:           f.PE,
		PB:           f.PB,
		ROE:          f.ROE,
		GrossMargin:  f.GrossMargin,
		NetMargin:    f.NetMargin,
		DebtRatio:    f.DebtRatio,
		Source:       f.Source,
		UpdatedAt:    f.UpdatedAt,
	}
}

func ToDomainFinancials(rows []FinancialDto) []value_objects.Financial {
	out := make([]value_objects.Financial, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

// ---------------------------------------------------------------------------
// 资讯与舆情
// ---------------------------------------------------------------------------

// NewsDto 对应 news 集合。自然键 = (symbol, url)。
type NewsDto struct {
	codeColumns `bson:",inline"`
	Title       string          `bson:"title"`
	Content     string          `bson:"content"`
	Source      string          `bson:"source"`
	URL         string          `bson:"url"`
	PublishedAt time.Time       `bson:"published_at"`
	Sentiment   decimal.Decimal `bson:"sentiment"`
}

func (dto NewsDto) ToDomain() value_objects.News {
	return value_objects.News{
		Code:        dto.toDomain(),
		Title:       dto.Title,
		Content:     dto.Content,
		Source:      dto.Source,
		URL:         dto.URL,
		PublishedAt: dto.PublishedAt,
		// 读回也走 NewSentiment：历史上有量纲不对的存量数据（0~100），
		// 钳位一次好过让它把加权情绪算飞。
		Sentiment: value_objects.NewSentiment(dto.Sentiment),
	}
}

func FromDomainNews(n value_objects.News) *NewsDto {
	return &NewsDto{
		codeColumns: codeColumnsOf(n.Code),
		Title:       n.Title,
		Content:     n.Content,
		Source:      n.Source,
		URL:         n.URL,
		PublishedAt: n.PublishedAt,
		Sentiment:   n.Sentiment.Value(),
	}
}

func ToDomainNewsList(rows []NewsDto) []value_objects.News {
	out := make([]value_objects.News, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

// SocialPostDto 对应 social_posts 集合。自然键 = (symbol, platform, published_at)。
type SocialPostDto struct {
	codeColumns `bson:",inline"`
	Platform    string          `bson:"platform"`
	Author      string          `bson:"author"`
	Content     string          `bson:"content"`
	Sentiment   decimal.Decimal `bson:"sentiment"`
	Engagement  int64           `bson:"engagement"`
	PublishedAt time.Time       `bson:"published_at"`
}

func (dto SocialPostDto) ToDomain() value_objects.SocialPost {
	return value_objects.SocialPost{
		Code:        dto.toDomain(),
		Platform:    dto.Platform,
		Author:      dto.Author,
		Content:     dto.Content,
		Sentiment:   value_objects.NewSentiment(dto.Sentiment),
		Engagement:  dto.Engagement,
		PublishedAt: dto.PublishedAt,
	}
}

func FromDomainSocialPost(p value_objects.SocialPost) *SocialPostDto {
	return &SocialPostDto{
		codeColumns: codeColumnsOf(p.Code),
		Platform:    p.Platform,
		Author:      p.Author,
		Content:     p.Content,
		Sentiment:   p.Sentiment.Value(),
		Engagement:  p.Engagement,
		// BSON 的 datetime 只有毫秒精度，先截断到毫秒，
		// 否则写进去的时间和拿来做 filter 的时间不相等，upsert 会每次都插新文档。
		PublishedAt: p.PublishedAt.Truncate(time.Millisecond),
	}
}

func ToDomainSocialPosts(rows []SocialPostDto) []value_objects.SocialPost {
	out := make([]value_objects.SocialPost, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
