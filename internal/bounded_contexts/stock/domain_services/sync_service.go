package domain_services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// SyncConfig 是同步的调优参数。
type SyncConfig struct {
	// FanOutLimit 是逐标的取数时的并发上限。
	//
	// 这个值由外部数据源的配额决定，不是由 CPU 决定：Tushare 免费档约 500 次/分钟，
	// Finnhub 免费档只有 60 次/分钟。按单次 300ms 估算，并发 8 大约是 1600 次/分钟——
	// 已经超出两家的配额，所以真正的节流必须做在数据源实现里；
	// 这里的并发上限解决的是另一个问题：不让几千个请求同时在飞，把内存和连接池打爆。
	FanOutLimit int
	// ChunkSize 是分片大小：每处理这么多标的就落一次库并推进断点。
	// 太大则崩溃后回退太多，太小则落库次数过多。
	ChunkSize int

	KlineLookbackDays int
	NewsLookbackDays  int
	NewsLimit         int
	FinancialLimit    int

	// StaleAfter 超过这个时长仍停在 running 的记录会被判为进程异常退出。
	StaleAfter time.Duration
}

func (c SyncConfig) normalized() SyncConfig {
	if c.FanOutLimit <= 0 {
		c.FanOutLimit = 8
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = 200
	}
	if c.KlineLookbackDays <= 0 {
		c.KlineLookbackDays = 365
	}
	if c.NewsLookbackDays <= 0 {
		c.NewsLookbackDays = 7
	}
	if c.NewsLimit <= 0 {
		c.NewsLimit = 20
	}
	if c.FinancialLimit <= 0 {
		c.FinancialLimit = 8
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 2 * time.Hour
	}
	return c
}

// SyncService 编排行情数据同步。
//
// 它与 StockService 并列而不是合并进去：查询服务面向请求线程、要求低延迟，
// 同步服务面向后台作业、一次跑几十分钟，两者的失败语义与可观测性诉求完全不同。
type SyncService struct {
	stockRepo  *repositories.StockRepository
	marketRepo *repositories.MarketDataRepository
	runRepo    *repositories.SyncRunRepository
	provider   DataProvider
	publisher  domain_event.Publisher
	log        *zap.Logger
	cfg        SyncConfig
}

func NewSyncService(
	stockRepo *repositories.StockRepository,
	marketRepo *repositories.MarketDataRepository,
	runRepo *repositories.SyncRunRepository,
	provider DataProvider,
	publisher domain_event.Publisher,
	log *zap.Logger,
	cfg SyncConfig,
) *SyncService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &SyncService{
		stockRepo: stockRepo, marketRepo: marketRepo, runRepo: runRepo,
		provider: provider, publisher: publisher, log: log, cfg: cfg.normalized(),
	}
}

// RunSync 是同步的统一入口，定时任务与手动触发都走它。
//
// 返回 (摘要, 处理条数, error) 的形状是为了适配调度上下文的 JobRunner 端口，
// 但本服务不 import 调度上下文——适配由组装根完成。
func (s *SyncService) RunSync(ctx context.Context, rawKind, rawMarket, triggeredBy string) (string, int, error) {
	kind, err := value_objects.NewSyncKind(rawKind)
	if err != nil {
		return "", 0, err
	}
	market, err := parseMarket(rawMarket)
	if err != nil {
		return "", 0, err
	}
	if !s.provider.Supports(market) {
		return "", 0, custom_errors.Unavailable("没有数据源支持市场 %s", market)
	}

	run, err := entities.StartSyncRun(idx.Prefixed("sync"), kind, market, triggeredBy)
	if err != nil {
		return "", 0, err
	}
	// 并发重复触发在这里被数据库唯一索引挡掉，而不是靠先查后插。
	if err := s.runRepo.TryStart(ctx, run); err != nil {
		return "", 0, err
	}

	stats, execErr := s.execute(ctx, run)
	if execErr != nil {
		if ferr := run.Fail(execErr.Error()); ferr == nil {
			_ = s.runRepo.Save(ctx, run)
			s.publish(ctx, run)
		}
		return "", 0, execErr
	}

	if err := run.Finish(stats); err != nil {
		return "", 0, err
	}
	if err := s.runRepo.Save(ctx, run); err != nil {
		return "", 0, err
	}
	s.publish(ctx, run)

	summary := fmt.Sprintf("%s/%s 同步完成：共 %d，成功 %d，失败 %d，成功率 %s%%",
		kind.DisplayName(), market.DisplayName(),
		stats.Total, stats.Succeeded, stats.Failed, decimalx.FormatPercent(stats.SuccessRate))
	return summary, stats.Succeeded, nil
}

func (s *SyncService) execute(ctx context.Context, run *entities.SyncRun) (value_objects.SyncStats, error) {
	switch run.Kind {
	case value_objects.SyncStockList:
		return s.syncStockList(ctx, run)
	case value_objects.SyncQuotes:
		// 批量优先。哨兵是零 IO 返回的，所以这次「尝试」不花任何代价。
		//
		// 顺序是硬约束：syncQuotesBatch 必须在碰 run 之前先把 FetchQuotes 调掉，
		// 否则退回逐标的时会在一个已经 PlanTotal 过的 run 上再 PlanTotal 一次，
		// 统计口径直接错乱。
		stats, err := s.syncQuotesBatch(ctx, run)
		if errors.Is(err, ErrBatchUnsupported) {
			s.log.Info("没有数据源提供批量行情端点，退回逐标的同步",
				zap.String("run_id", run.ID), zap.String("market", run.Market.String()))
			return s.syncPerSymbol(ctx, run)
		}
		return stats, err
	case value_objects.SyncKlines:
		// 同上，批量优先。东财只能逐只查，tushare 的 daily 不传 ts_code 就返回
		// 当天全市场——差别是 5900 次调用对 365 次，而且后者与标的数无关。
		stats, err := s.syncKlinesBatch(ctx, run)
		if errors.Is(err, ErrBatchUnsupported) {
			s.log.Info("没有数据源提供按交易日的批量 K 线端点，退回逐标的同步",
				zap.String("run_id", run.ID), zap.String("market", run.Market.String()))
			return s.syncPerSymbol(ctx, run)
		}
		return stats, err
	default:
		return s.syncPerSymbol(ctx, run)
	}
}

// syncQuotesBatch 用一次批量端点覆盖整个市场的行情快照。
//
// 和 syncPerSymbol 的分工：那条路径的外部调用数等于标的数（几千次），这条是常数次
// （东财 clist 每页 100 条，A 股约 60 页）。两条路径的统计口径必须完全一致，
// 否则「今天成功率 100%、昨天 82%」反映的只是走了哪条路，而不是数据质量。
func (s *SyncService) syncQuotesBatch(ctx context.Context, run *entities.SyncRun) (value_objects.SyncStats, error) {
	// 放在最前面：哨兵是零 IO 的，排在 listSymbols 之前，退回逐标的时就不会多读一次库。
	quotes, err := s.provider.FetchQuotes(ctx, run.Market)
	if err != nil {
		return value_objects.SyncStats{}, err // 含哨兵，由 execute 判定
	}

	// listSymbols 是本地翻页查询，不是循环内 RPC。它提供的是**稳定的分母**。
	//
	// 这一步是整个方法的关键。若把 Total 设成 len(quotes)，Failed 就结构性恒为 0，
	// AllFailed() 永远不会触发——而东财被限流时的表现恰恰是 rc=0 但 diff 为空，
	// 于是一次全量封禁会被 Finish 判成 succeeded，看板上显示一行绿色的「共 0，成功 0」。
	known, err := s.listSymbols(ctx, run.Market)
	if err != nil {
		return value_objects.SyncStats{}, err
	}
	universe := make(map[string]struct{}, len(known))
	for _, c := range known {
		universe[c.Symbol] = struct{}{}
	}

	// 只保留「本地有 Stock 锚点」且自然键完整的快照。
	//
	// 丢掉源里多出来的代码是刻意的：Stock 是全系统行情/财务/资讯的身份锚点，
	// 写入没有锚点的 quote 会让选股的跨存储求交把幽灵代码算进候选集。
	// 过滤 HasNaturalKey 则是为了让 succeeded 等于「真正写进去的条数」——
	// SaveQuotes 内部也会丢掉它们，不在这里过滤就会虚报成功。
	kept := make([]value_objects.Quote, 0, len(quotes))
	covered := make(map[string]struct{}, len(quotes))
	for _, q := range quotes {
		if !q.HasNaturalKey() {
			continue
		}
		if _, ok := universe[q.Symbol()]; !ok {
			continue
		}
		if _, dup := covered[q.Symbol()]; dup {
			continue // 源里同一只票出现两次，只认第一条
		}
		covered[q.Symbol()] = struct{}{}
		kept = append(kept, q)
	}
	gap := len(known) - len(covered)

	source := ""
	if len(kept) > 0 {
		source = kept[0].Source
	}
	s.log.Info("批量行情拉取完成",
		zap.String("run_id", run.ID), zap.String("market", run.Market.String()),
		zap.Int("fetched", len(quotes)), zap.Int("kept", len(kept)),
		zap.Int("gap", gap), zap.Int("universe", len(known)),
		zap.String("source", source))

	if err := run.PlanTotal(len(known)); err != nil {
		return value_objects.SyncStats{}, err
	}
	if err := s.runRepo.Save(ctx, run); err != nil {
		return value_objects.SyncStats{}, err
	}

	// 排序只为让游标与进度日志有确定语义，与逐标的路径口径一致。
	sort.Slice(kept, func(i, j int) bool { return kept[i].Symbol() < kept[j].Symbol() })

	for start := 0; start < len(kept); start += s.cfg.ChunkSize {
		if err := ctx.Err(); err != nil {
			return run.Stats, err
		}
		end := start + s.cfg.ChunkSize
		if end > len(kept) {
			end = len(kept)
		}
		chunk := kept[start:end]

		// 落库失败是整批失败（存储不可用），不是「这几只票失败」。
		// 记成 failed 会让运维以为是数据源缺票，转头去查一个根本没问题的数据源。
		if err := s.marketRepo.SaveQuotes(ctx, chunk); err != nil {
			return run.Stats, err
		}
		if err := run.Advance(chunk[len(chunk)-1].Symbol(), len(chunk), 0, 0); err != nil {
			return run.Stats, err
		}
		if err := s.runRepo.Save(ctx, run); err != nil {
			return run.Stats, err
		}
	}

	// 覆盖缺口记 failed 而不是 skipped。
	//
	// skipped 在本服务里的含义是「我们主动没做」（退市票被 listSymbols 过滤掉），
	// 而这里是「我们要了，源没给」——这和逐标的路径上 FetchQuote 返回 NotFound
	// 被 processChunk 记成 failed 是同一件事，两条路径必须给出同样的数字。
	//
	// 这一笔也是 AllFailed 唯一的触发来源，见本方法开头关于分母的说明。
	if gap > 0 {
		// cursor 传空串：Advance 只在非空时才改 Cursor，这笔补记不代表进度推进。
		if err := run.Advance("", 0, gap, 0); err != nil {
			return run.Stats, err
		}
		if err := s.runRepo.Save(ctx, run); err != nil {
			return run.Stats, err
		}
	}
	return run.Stats, nil
}

// syncKlinesBatch 按交易日逐天拉取整个市场的 K 线。
//
// # 为什么按天而不是按标的
//
// 逐标的路径的外部调用数等于标的数：A 股 5900 余只，而东财的 K 线接口必须限速到
// 约 2 次/秒（超了它直接掐 TCP，速率是正确性不是调优），一次全量同步下限就是
// 五十分钟——而且这个数字会随着市场扩容线性变长。
//
// 按交易日拉则是「回看天数」次调用，与标的数**无关**。回看一年是 365 次，
// 其中约三分之一是休市日、返回空集。
//
// # 统计口径必须与逐标的路径一致
//
// 分母取 len(known) 而不是「拉回来多少条」：若用后者，Failed 会结构性恒为 0，
// 一次被限流到只返回几十只票的同步会显示成 100% 成功。这条和 syncQuotesBatch
// 是同一个道理，两条路径给出的数字必须可比，否则「今天成功率 100%、昨天 82%」
// 反映的只是走了哪条路。
func (s *SyncService) syncKlinesBatch(ctx context.Context, run *entities.SyncRun) (value_objects.SyncStats, error) {
	// 跳过周末：CN/HK/US 三个市场都是周一到周五交易，问周六周日只会拿回空集。
	// 一年 365 天里这能省掉约 104 次无谓调用。法定节假日不在这里排除——
	// 那需要一份会过期的节假日表，而多问一个休市日的代价只是一次空响应。
	dates := make([]shared_vo.TradeDate, 0, s.cfg.KlineLookbackDays)
	for _, d := range shared_vo.LastNDays(s.cfg.KlineLookbackDays).Days() {
		if t, ok := d.Time(); ok && (t.Weekday() == time.Saturday || t.Weekday() == time.Sunday) {
			continue
		}
		dates = append(dates, d)
	}
	if len(dates) == 0 {
		return value_objects.NewSyncStats(0, 0, 0, 0), nil
	}

	// 第一天先探路：哨兵是零 IO 返回的，因此这次「尝试」不花任何代价，
	// 而它必须发生在碰 run 之前——否则退回逐标的时会在一个已经 PlanTotal 过的
	// run 上再 PlanTotal 一次，统计口径直接错乱。
	first, err := s.provider.FetchKlinesByDate(ctx, run.Market, value_objects.PeriodDaily, dates[0])
	if err != nil {
		return value_objects.SyncStats{}, err // 含哨兵，由 execute 判定
	}

	known, err := s.listSymbols(ctx, run.Market)
	if err != nil {
		return value_objects.SyncStats{}, err
	}
	universe := make(map[string]struct{}, len(known))
	for _, c := range known {
		universe[c.Symbol] = struct{}{}
	}

	if err := run.PlanTotal(len(known)); err != nil {
		return value_objects.SyncStats{}, err
	}
	if err := s.runRepo.Save(ctx, run); err != nil {
		return value_objects.SyncStats{}, err
	}

	// covered 记录「至少拿到过一根 K 线」的标的，它是 succeeded 的来源。
	// 按天累计而不是按天各记一笔：同一只票在 365 天里会出现 365 次，
	// 逐天累加会让 succeeded 变成「行数」，和逐标的路径的「标的数」对不上。
	covered := make(map[string]struct{}, len(known))
	source := ""

	for i, date := range dates {
		if err := ctx.Err(); err != nil {
			return run.Stats, err
		}
		klines := first
		if i > 0 {
			klines, err = s.provider.FetchKlinesByDate(ctx, run.Market, value_objects.PeriodDaily, date)
			if err != nil {
				// 单日失败不该中断整次同步：这一天可能只是上游抖了一下，
				// 而其余 364 天的数据仍然值得写进去。缺口会体现在最后的 gap 里。
				s.log.Warn("按交易日拉取 K 线失败，跳过该日",
					zap.String("run_id", run.ID), zap.String("date", date.String()), zap.Error(err))
				continue
			}
		}
		if len(klines) == 0 {
			continue // 休市日，正常
		}
		if source == "" {
			source = klines[0].Source
		}

		// 只保留本地有 Stock 锚点、且自然键完整的 K 线。理由同 syncQuotesBatch：
		// Stock 是全系统的身份锚点，写入没有锚点的行会让选股把幽灵代码算进候选集。
		kept := make([]value_objects.Kline, 0, len(klines))
		for _, k := range klines {
			if !k.HasNaturalKey() {
				continue
			}
			if _, ok := universe[k.Code.Symbol]; !ok {
				continue
			}
			kept = append(kept, k)
			covered[k.Code.Symbol] = struct{}{}
		}
		if len(kept) == 0 {
			continue
		}
		// 落库失败是整批失败（存储不可用），不是「这些票失败」。
		if err := s.marketRepo.SaveKlines(ctx, kept); err != nil {
			return run.Stats, err
		}
	}

	gap := len(known) - len(covered)
	s.log.Info("按交易日批量拉取 K 线完成",
		zap.String("run_id", run.ID), zap.String("market", run.Market.String()),
		zap.Int("days", len(dates)), zap.Int("covered", len(covered)),
		zap.Int("gap", gap), zap.Int("universe", len(known)),
		zap.String("source", source))

	// 一次性记账而不是按天推进：断点续传的游标语义是「这个标的之前的都处理完了」，
	// 而按天拉的中途状态没法用一个标的游标表达——强行写一个只会让续传从错误的
	// 位置开始。这条路径本来就只有几百次调用，整体重跑的代价远小于续传出错。
	if len(covered) > 0 {
		if err := run.Advance("", len(covered), 0, 0); err != nil {
			return run.Stats, err
		}
	}
	// 覆盖缺口记 failed 而不是 skipped：口径同 syncQuotesBatch——
	// skipped 是「我们主动没做」，这里是「我们要了，源没给」。
	if gap > 0 {
		if err := run.Advance("", 0, gap, 0); err != nil {
			return run.Stats, err
		}
	}
	if err := s.runRepo.Save(ctx, run); err != nil {
		return run.Stats, err
	}
	return run.Stats, nil
}

// syncStockList 同步股票主数据：一次批量拉取 + 一次批量落库，天然 O(1) 次外部调用。
func (s *SyncService) syncStockList(ctx context.Context, run *entities.SyncRun) (value_objects.SyncStats, error) {
	list, err := s.provider.FetchStockList(ctx, run.Market)
	if err != nil {
		return value_objects.SyncStats{}, err
	}
	// 记下是哪个源给的数据。降级链对上层是透明的，一旦真实数据源限权/限频而
	// 由兜底源接手，同步依然会报「成功」，条数却只有个位数——
	// 没有这一行，排查时完全看不出「同步了 8 条」是数据源的问题还是落库的问题。
	source := ""
	if len(list) > 0 {
		source = list[0].Source
	}
	s.log.Info("股票列表拉取完成",
		zap.String("run_id", run.ID), zap.String("market", run.Market.String()),
		zap.Int("count", len(list)), zap.String("source", source))

	if len(list) == 0 {
		return value_objects.NewSyncStats(0, 0, 0, 0), nil
	}
	if err := s.stockRepo.BulkUpsert(ctx, list); err != nil {
		return value_objects.SyncStats{}, err
	}
	return value_objects.NewSyncStats(len(list), len(list), 0, 0), nil
}

// syncPerSymbol 处理需要逐标的取数的同步类型。
//
// 这是全系统「禁止循环内 RPC」最尖锐的一处：一个市场几千只标的，
// 朴素写法就是 for 里发几千次外部请求、再发几千次落库。
// 这里的做法是：分片 → 片内有界并发取数 → 整片一次批量落库 → 推进断点。
// 外部调用数量无法低于标的数（数据源没有批量接口），但并发受控、
// 落库次数降到 N/ChunkSize，且每片结束都有可续传的检查点。
func (s *SyncService) syncPerSymbol(ctx context.Context, run *entities.SyncRun) (value_objects.SyncStats, error) {
	codes, err := s.listSymbols(ctx, run.Market)
	if err != nil {
		return value_objects.SyncStats{}, err
	}
	// 按代码排序，断点续传才有确定的语义：游标之前的都已处理完。
	sort.Slice(codes, func(i, j int) bool { return codes[i].Symbol < codes[j].Symbol })

	if run.Cursor != "" {
		codes = skipUntilAfter(codes, run.Cursor)
	}
	if len(codes) == 0 {
		return value_objects.NewSyncStats(0, 0, 0, 0), nil
	}

	if err := run.PlanTotal(len(codes)); err != nil {
		return value_objects.SyncStats{}, err
	}
	if err := s.runRepo.Save(ctx, run); err != nil {
		return value_objects.SyncStats{}, err
	}

	for start := 0; start < len(codes); start += s.cfg.ChunkSize {
		if err := ctx.Err(); err != nil {
			// 被取消时保留已推进的断点，下次可以接着跑。
			return run.Stats, err
		}
		end := start + s.cfg.ChunkSize
		if end > len(codes) {
			end = len(codes)
		}
		chunk := codes[start:end]

		succeeded, failed, err := s.processChunk(ctx, run.Kind, chunk)
		if err != nil {
			return run.Stats, err
		}
		if err := run.Advance(chunk[len(chunk)-1].Symbol, succeeded, failed, 0); err != nil {
			return run.Stats, err
		}
		if err := s.runRepo.Save(ctx, run); err != nil {
			return run.Stats, err
		}

		s.log.Debug("同步分片完成",
			zap.String("run_id", run.ID), zap.String("kind", run.Kind.String()),
			zap.Int("processed", run.Stats.Processed()), zap.Int("total", run.Stats.Total))
	}
	return run.Stats, nil
}

// processChunk 片内有界并发取数，然后整片一次落库。
func (s *SyncService) processChunk(
	ctx context.Context,
	kind value_objects.SyncKind,
	chunk []shared_vo.StockCode,
) (succeeded, failed int, err error) {
	// Settle 而非 Map：单只标的取数失败（停牌、数据源缺该票）不该中断整次同步，
	// 那是「部分成功」这个业务结果的来源。
	outcomes, err := concurrency.Settle(ctx, chunk, s.cfg.FanOutLimit,
		func(ctx context.Context, code shared_vo.StockCode) (any, error) {
			return s.fetchOne(ctx, kind, code)
		})
	if err != nil {
		return 0, 0, err
	}

	// 先把整片结果聚起来，再一次性批量写——逐条落库会让写次数等于标的数。
	var (
		quotes     []value_objects.Quote
		klines     []value_objects.Kline
		financials []value_objects.Financial
		news       []value_objects.News
	)
	for _, o := range outcomes {
		if o.Err != nil {
			failed++
			continue
		}
		switch v := o.Value.(type) {
		case *value_objects.Quote:
			if v != nil {
				quotes = append(quotes, *v)
			}
		case []value_objects.Kline:
			klines = append(klines, v...)
		case []value_objects.Financial:
			financials = append(financials, v...)
		case []value_objects.News:
			news = append(news, v...)
		}
		succeeded++
	}

	if err := s.saveChunk(ctx, quotes, klines, financials, news); err != nil {
		return 0, 0, err
	}
	return succeeded, failed, nil
}

func (s *SyncService) fetchOne(ctx context.Context, kind value_objects.SyncKind, code shared_vo.StockCode) (any, error) {
	switch kind {
	case value_objects.SyncQuotes:
		return s.provider.FetchQuote(ctx, code)
	case value_objects.SyncKlines:
		rng := shared_vo.LastNDays(s.cfg.KlineLookbackDays)
		return s.provider.FetchKlines(ctx, code, value_objects.PeriodDaily, rng)
	case value_objects.SyncFinancials:
		return s.provider.FetchFinancials(ctx, code, s.cfg.FinancialLimit)
	case value_objects.SyncNews:
		rng := shared_vo.LastNDays(s.cfg.NewsLookbackDays)
		return s.provider.FetchNews(ctx, code, rng, s.cfg.NewsLimit)
	}
	return nil, custom_errors.Invalid("不支持的逐标的同步类型: %s", kind)
}

func (s *SyncService) saveChunk(
	ctx context.Context,
	quotes []value_objects.Quote,
	klines []value_objects.Kline,
	financials []value_objects.Financial,
	news []value_objects.News,
) error {
	if len(quotes) > 0 {
		if err := s.marketRepo.SaveQuotes(ctx, quotes); err != nil {
			return err
		}
	}
	if len(klines) > 0 {
		if err := s.marketRepo.SaveKlines(ctx, klines); err != nil {
			return err
		}
	}
	if len(financials) > 0 {
		if err := s.marketRepo.SaveFinancials(ctx, financials); err != nil {
			return err
		}
	}
	if len(news) > 0 {
		if err := s.marketRepo.SaveNews(ctx, news); err != nil {
			return err
		}
	}
	return nil
}

// listSymbols 翻页取出该市场的全部在市标的。
//
// 这里的循环是数据库翻页，不是「循环内 RPC」：本地查询没有配额与网络往返成本，
// 且一次性把几千行拉进内存才是真正的问题。
func (s *SyncService) listSymbols(ctx context.Context, market shared_vo.Market) ([]shared_vo.StockCode, error) {
	// 要一个够大的页，实际值由 Page 自己收敛到 maxPageSize——
	// 后面所有翻页算术都必须用收敛后的 page.Size，不能用这个请求值。
	const wantPageSize = 500
	var out []shared_vo.StockCode
	for pageNum := 1; ; pageNum++ {
		page := shared_vo.NewPage(pageNum, wantPageSize)
		items, total, err := s.stockRepo.ListByMarket(ctx, market, page)
		if err != nil {
			return nil, err
		}
		for _, st := range items {
			if st.Delisted {
				continue
			}
			out = append(out, st.Code)
		}
		// 用 page.Size 而不是 wantPageSize：请求 500 实际只会拿到 200 行，
		// 按 500 算翻页进度会在读到 2/5 的标的时就判定「翻完了」，
		// 后面几千只票被静默跳过——同步照常报成功，只是少同步了一大半。
		if len(items) < page.Size || int64(pageNum*page.Size) >= total {
			break
		}
	}
	if len(out) == 0 {
		return nil, custom_errors.NotFound("市场 %s 没有可同步的标的，请先同步股票列表", market)
	}
	return out, nil
}

// RecoverStaleRuns 清理被强杀进程留下的僵死记录，应在服务启动时调用一次。
func (s *SyncService) RecoverStaleRuns(ctx context.Context) (int64, error) {
	n, err := s.runRepo.MarkStaleAsFailed(ctx, time.Now().Add(-s.cfg.StaleAfter))
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Warn("清理僵死同步记录", zap.Int64("count", n))
	}
	return n, nil
}

// History 查询同步历史。
func (s *SyncService) History(ctx context.Context, rawKind, rawMarket string, page shared_vo.Page) ([]*entities.SyncRun, int64, error) {
	var kind value_objects.SyncKind
	if rawKind != "" {
		k, err := value_objects.NewSyncKind(rawKind)
		if err != nil {
			return nil, 0, err
		}
		kind = k
	}
	var market shared_vo.Market
	if rawMarket != "" {
		m, err := parseMarket(rawMarket)
		if err != nil {
			return nil, 0, err
		}
		market = m
	}
	return s.runRepo.List(ctx, kind, market, page)
}

func (s *SyncService) GetRun(ctx context.Context, id string) (*entities.SyncRun, error) {
	return s.runRepo.FindByID(ctx, id)
}

// LatestOf 返回某类型某市场最近一次运行。
func (s *SyncService) LatestOf(ctx context.Context, rawKind, rawMarket string) (*entities.SyncRun, error) {
	kind, err := value_objects.NewSyncKind(rawKind)
	if err != nil {
		return nil, err
	}
	market, err := parseMarket(rawMarket)
	if err != nil {
		return nil, err
	}
	return s.runRepo.LatestOf(ctx, kind, market)
}

// Trigger 手动触发同步，仅管理员。
func (s *SyncService) Trigger(ctx context.Context, operator *Operator, rawKind, rawMarket string) (*entities.SyncRun, error) {
	if err := RequireAdmin(operator); err != nil {
		return nil, err
	}
	triggeredBy := fmt.Sprintf("user:%d", operator.UserID)
	if _, _, err := s.RunSync(ctx, rawKind, rawMarket, triggeredBy); err != nil {
		return nil, err
	}
	return s.LatestOf(ctx, rawKind, rawMarket)
}

func (s *SyncService) publish(ctx context.Context, run *entities.SyncRun) {
	if evts := run.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// skipUntilAfter 丢弃游标及之前的标的，实现断点续传。
func skipUntilAfter(codes []shared_vo.StockCode, cursor string) []shared_vo.StockCode {
	idx := sort.Search(len(codes), func(i int) bool { return codes[i].Symbol > cursor })
	return codes[idx:]
}
