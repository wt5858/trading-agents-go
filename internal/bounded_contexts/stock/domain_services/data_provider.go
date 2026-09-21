package domain_services

import (
	"context"
	"errors"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// ErrBatchUnsupported 表示该数据源没有某个批量端点——这是能力缺席，不是故障。
//
// 两者必须区分开：故障要重试、要告警、要降级；能力缺席只意味着「换一条路走」。
// 混为一谈的后果很具体——上层会把一次真实故障当成「该走逐标的路径了」，
// 转身对一个刚刚掐掉我们连接的数据源打几千次请求。
var ErrBatchUnsupported = errors.New("数据源无批量端点")

// DataProvider 是外部行情数据源端口（Tushare / Finnhub / Mock ...）。
//
// 它声明在消费方这一侧而不是实现方，和 identity 的 TokenIssuer 是同一套做法：
// 本服务只关心「能取到数据」，至于背后是 HTTP、gRPC 还是本地假数据，
// 由 internal/helpers/marketdata 决定，领域层不因为换数据源而改动。
//
// 数据源切换与降级由 marketdata.Composite 负责，本层只注入一个 DataProvider，
// 不感知背后有几个源。
//
// 签名全部使用值对象：代码规范化、日期格式换算这类事必须在进入本端口之前完成，
// 各家数据源自己的格式怪癖（Tushare 要 YYYYMMDD、Finnhub 要 Unix 秒）
// 由各自的实现用 StockCode.FullSymbol() / TradeDate.Compact() 就地换算。
type DataProvider interface {
	// Name 是数据源标识，会被写进 Stock.Source / Quote.Source，用于跨源对账。
	Name() string
	// Supports 声明该源覆盖哪些市场。降级链靠它跳过不支持的源，
	// 而不是靠「调用失败再换下一个」——后者要白白付出一次网络往返。
	Supports(market shared_vo.Market) bool

	// FetchStockList 是本端口唯一的批量拉取入口，也是做 read-through 时
	// 补齐主数据的唯一手段：端口刻意不提供「取单只股票基本信息」，
	// 因为那会诱使调用方在 for 里逐个取，一篮子标的就是 N 次外部 API 调用。
	FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error)

	FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error)

	// FetchQuotes 一次取回整个市场的行情快照。
	//
	// 它和 FetchQuote 不是「批量版/单只版」的关系，而是两种截然不同的外部能力：
	// 有的源提供整表端点（东财 clist 一次一页 100 条），有的源只能一只一只问
	// （Finnhub 的 /quote）。前者把一次全市场同步从几千次调用压到几十次，
	// 这个差别大到必须显式出现在端口上，而不是让调用方在 for 里自己拼。
	//
	// 没有这个能力的源必须**零 IO 立即**返回包着 ErrBatchUnsupported 的错误，
	// 绝不能返回 (nil, nil)：空切片会被降级链当成成功，链子就此停住，
	// 同步报告「成功 0 条」——这是最难排查的那种失败。
	FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error)
	FetchKlines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, r shared_vo.DateRange) ([]value_objects.Kline, error)

	// FetchKlinesByDate 一次取回整个市场在某一个交易日的 K 线。
	//
	// 它与 FetchKlines 的关系同 FetchQuotes 与 FetchQuote：不是「批量版/单只版」，
	// 而是两种截然不同的外部能力。Tushare 的 daily 接口不传 ts_code、只传 trade_date
	// 就返回当天全市场；东财的 K 线接口则只能一只一只问。
	//
	// 这个差别的量级值得单独开一个端口方法：A 股 5900 余只标的、回看一年，
	// 逐标的是 5900 次调用，按交易日是 365 次——而后者还与标的数**无关**，
	// 市场再扩容也不会更慢。
	//
	// 返回的 Kline 自带 Code，因此是扁平切片而不是 map：调用方本来就要按自然键
	// (symbol, period, trade_date) 落库，再包一层 map 只是多一次拆装。
	//
	// 没有这个能力的源必须**零 IO 立即**返回包着 ErrBatchUnsupported 的错误，
	// 理由同 FetchQuotes：返回 (nil, nil) 会被降级链当成成功，
	// 同步报告「成功 0 条」，而这是最难排查的那种失败。
	FetchKlinesByDate(ctx context.Context, market shared_vo.Market, period value_objects.Period, date shared_vo.TradeDate) ([]value_objects.Kline, error)

	FetchFinancials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error)
	FetchNews(ctx context.Context, code shared_vo.StockCode, r shared_vo.DateRange, limit int) ([]value_objects.News, error)
}
