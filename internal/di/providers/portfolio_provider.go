package providers

import (
	"context"

	"github.com/google/wire"

	paper_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/domain_services"
	paper_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/repositories"
	paper_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	screening_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	screening_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/repositories"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	watchlist_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_services"
	watchlist_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/repositories"
	watchlist_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 自选股、模拟交易、选股筛选三个上下文的装配，以及它们各自的报价读取适配器。
//
// 自选股与模拟交易都需要「一次拿一批最新价」，但各自用自己的值对象表达报价：
// 自选股关心涨跌幅（展示用 float 够了），模拟交易关心可参与运算的精确价格（必须 decimal）。
//
// 与其让 stock 上下文去迁就两个下游的形状，不如在组装根各写一个几行的适配器：
// stock 只暴露它自己的 Quote，下游各自声明需要的窄端口，谁也不用认识谁。

// WatchlistQuoteReader 把行情仓储适配成自选股需要的报价读取端口。
type WatchlistQuoteReader struct {
	market *stock_repo.MarketDataRepository
}

var _ watchlist_services.QuoteReader = (*WatchlistQuoteReader)(nil)

func NewWatchlistQuoteReader(market *stock_repo.MarketDataRepository) *WatchlistQuoteReader {
	return &WatchlistQuoteReader{market: market}
}

func (r *WatchlistQuoteReader) LatestQuotes(ctx context.Context, codes []shared_vo.StockCode) ([]watchlist_vo.QuoteSnapshot, error) {
	// 一次聚合查询取回整批，绝不按代码逐个查——一个 200 只股票的分组
	// 会变成 200 次往返。
	quotes, err := r.market.LatestQuotes(ctx, codes)
	if err != nil {
		return nil, err
	}
	out := make([]watchlist_vo.QuoteSnapshot, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, watchlist_vo.QuoteSnapshot{
			Code: q.Code,
			// ChangePct 读数据源口径的存量值，不用 (close-preClose)/preClose 重算：
			// 涨跌幅是乘除派生值，各家数据源对复权与停牌的处理不同，重算会和行情页对不上。
			Price:     q.Close,
			ChangePct: q.ChangePct,
			TradeDate: q.TradeDate,
			UpdatedAt: q.UpdatedAt,
		})
	}
	return out, nil
}

// PaperQuoteReader 把行情仓储适配成模拟交易需要的报价读取端口。
type PaperQuoteReader struct {
	market *stock_repo.MarketDataRepository
}

var _ paper_services.QuoteReader = (*PaperQuoteReader)(nil)

func NewPaperQuoteReader(market *stock_repo.MarketDataRepository) *PaperQuoteReader {
	return &PaperQuoteReader{market: market}
}

func (r *PaperQuoteReader) LatestQuotes(ctx context.Context, codes []shared_vo.StockCode) ([]paper_vo.LiveQuote, error) {
	quotes, err := r.market.LatestQuotes(ctx, codes)
	if err != nil {
		return nil, err
	}
	out := make([]paper_vo.LiveQuote, 0, len(quotes))
	for _, q := range quotes {
		// 走构造函数而不是结构字面量：LiveQuote 的价格必须按货币精度归整，
		// 而那一步只在 NewLiveQuote 里做。绕开它会让一个多出几位小数的价格
		// 进入模拟撮合，最终体现为持仓市值对不上。
		//
		// AsOf 取 TradeDate 而不是 UpdatedAt：这两个时间戳回答的是不同的问题。
		// UpdatedAt 是「这一行什么时候写进库的」，一次历史数据回补会把它刷成今天，
		// 于是一条三个月前的收盘价看起来像是刚刚的报价。TradeDate 是「这个价格
		// 属于哪一天」，也就是报价自己的时钟——下单路径的新鲜度判断只能问它。
		asOf, _ := q.TradeDate.Time()
		out = append(out, paper_vo.NewLiveQuote(q.Code, q.Close, asOf))
	}
	return out, nil
}

var WatchlistSet = wire.NewSet(
	NewWatchlistQuoteReader,
	watchlist_repo.NewWatchlistGroupRepository,
	watchlist_services.NewWatchlistService,

	wire.Bind(new(watchlist_services.QuoteReader), new(*WatchlistQuoteReader)),
)

var PaperTradingSet = wire.NewSet(
	NewPaperQuoteReader,
	paper_repo.NewPaperAccountRepository,
	paper_services.NewPaperTradingService,

	wire.Bind(new(paper_services.QuoteReader), new(*PaperQuoteReader)),
)

// ScreeningSet：StockScreener 同时拿 MySQL 与 Mongo，由它负责把过滤下推到各自的库，
// 而不是把全市场载进内存再在 Go 里过一遍。
var ScreeningSet = wire.NewSet(
	screening_repo.NewScreeningTemplateRepository,
	screening_repo.NewStockScreener,
	screening_services.NewScreeningService,

	wire.Bind(new(screening_services.StockScreener), new(*screening_repo.StockScreener)),
)
