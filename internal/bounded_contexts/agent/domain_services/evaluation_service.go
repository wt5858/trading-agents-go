package domain_services

import (
	"context"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// evalFanOutLimit 是取前瞻行情的并发上限。
//
// 与数据准备阶段同取 4，理由也一样：每条样本背后是一次 Mongo 区间查询，
// 不设限意味着一次回测会把几百个查询同时压到业务库上。
// 回测是离线任务，多跑几秒没有任何代价，把线上库打慢了才有。
const evalFanOutLimit = 4

// EvaluationService 跑一次回测评估：扫历史运行、对上后续行情、算一致率。
//
// # 它不重跑分析
//
// 评分用的是已经落库的运行轨迹，不会调用引擎、不花一分钱 token。
// 这是刻意的：重跑一遍流水线得到的是「今天的模型对历史数据怎么看」，
// 而那既贵又答非所问——要评的是当时那套系统真实给出过的建议。
// 样本量因此等于历史上真的跑过多少次分析，这个限制是诚实的，
// 比用重跑凑出来的漂亮样本量有意义得多。
type EvaluationService struct {
	runs   *repositories.AnalysisRunRepository
	evals  *repositories.EvaluationRepository
	market MarketReader
	log    *zap.Logger
}

func NewEvaluationService(
	runs *repositories.AnalysisRunRepository,
	evals *repositories.EvaluationRepository,
	market MarketReader,
	log *zap.Logger,
) *EvaluationService {
	if log == nil {
		log = zap.NewNop()
	}
	return &EvaluationService{runs: runs, evals: evals, market: market, log: log}
}

// EvaluateInput 是一次回测的入参形状。
type EvaluateInput struct {
	// Window 是被评估的分析所在的交易日区间。
	Window shared_vo.DateRange
	// HorizonDays 是前瞻窗口的自然日数，0 走默认。
	HorizonDays int
	// Limit 是最多取多少次运行，0 走仓储默认。
	Limit int
}

// Evaluate 跑一次回测并落库。
//
// 顺序：扫运行骨架 -> 并发取每条样本的基准价与前瞻价 -> 逐条评分 -> 结算 -> 落库。
//
// 评分本身是串行的，只有取行情并发：Evaluation 聚合没有内部锁
// （它不像 AnalysisContext 那样天生并发），让多个 goroutine 同时往里塞样本
// 就是一次必现但难复现的 slice 并发写。取数并发、评分串行，两边的代价都最小。
func (s *EvaluationService) Evaluate(ctx context.Context, in EvaluateInput) (*entities.Evaluation, error) {
	if s.runs == nil || s.evals == nil {
		return nil, custom_errors.Unavailable("未配置回测所需的仓储")
	}
	if s.market == nil {
		return nil, custom_errors.Unavailable("未配置行情读取端口")
	}

	eval, err := entities.NewEvaluation(idx.New(), in.Window, in.HorizonDays)
	if err != nil {
		return nil, err
	}

	runs, truncated, err := s.runs.ListSummaries(ctx, in.Window, in.Limit)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, custom_errors.NotFound("区间 %s ~ %s 内没有可评估的分析运行",
			in.Window.Start.String(), in.Window.End.String())
	}
	if truncated {
		// 如实记下来并落进统计。悄悄评一部分再报一个看起来像全量的一致率，
		// 比不做这个功能更糟。
		eval.MarkTruncated()
		s.log.Warn("区间内运行数超过上限，本次只评估了其中一部分",
			zap.Int("evaluated", len(runs)),
			zap.String("window_start", in.Window.Start.String()),
			zap.String("window_end", in.Window.End.String()))
	}

	// 取数并发、容错：某只票缺行情只该让这一条样本被跳过，
	// 不该株连整次回测——而缺行情在回测里是常态而非异常。
	outcomes, err := concurrency.Settle(ctx, runs, evalFanOutLimit,
		func(ctx context.Context, run value_objects.RunSummary) (forwardQuote, error) {
			return s.forwardQuoteOf(ctx, run, eval.HorizonDays)
		})
	if err != nil {
		return nil, custom_errors.Unavailable("回测取数被取消").Wrap(err)
	}

	for i, o := range outcomes {
		if o.Err != nil {
			s.log.Debug("样本取数失败",
				zap.String("symbol", runs[i].Code.FullSymbol()),
				zap.String("trade_date", runs[i].TradeDate.String()),
				zap.Error(o.Err))
			// 取数失败单独归因，不混进「窗口不足」：
			// 后者是「过几天再跑就能评上」，前者是基础设施出了问题。
			eval.Skip(runs[i], value_objects.SkipFetchFailed)
			continue
		}
		if o.Value.incomplete() {
			eval.Skip(runs[i], o.Value.skipReason)
			continue
		}
		eval.Score(runs[i], o.Value.base, o.Value.forward, o.Value.forwardDate)
	}

	eval.Settle()
	if err := s.evals.Save(ctx, eval); err != nil {
		return nil, err
	}
	return eval, nil
}

// forwardQuote 是一条样本的两个价格。
type forwardQuote struct {
	base        decimal.Decimal
	forward     decimal.Decimal
	forwardDate shared_vo.TradeDate
	skipReason  value_objects.SkipReason
}

func (q forwardQuote) incomplete() bool { return q.skipReason != value_objects.SkipNone }

// forwardQuoteOf 取一条样本的基准收盘价与前瞻窗口末日收盘价。
//
// 一次区间查询把两头都取回来，而不是查两次：查两次除了多一倍查询，
// 还会在停牌股上给出两个互相矛盾的答案——「当天没开盘」与「当天有收盘价」
// 取决于两次查询各自怎么处理缺失。
//
// 注意 MarketReader.Klines 是按 trade_date **倒序**返回的
// （market_data_repository.go 的 sort 是 -1），因此最新的一根在 [0]、
// 最早的一根在末尾。基准价取末尾、前瞻价取头部，反了的话
// 收益率会整体变号，而一致率恰好翻成 1 减去真值——一个看起来完全合理的数字。
func (s *EvaluationService) forwardQuoteOf(
	ctx context.Context,
	run value_objects.RunSummary,
	horizonDays int,
) (forwardQuote, error) {
	rng := shared_vo.DateRange{
		Start: run.TradeDate,
		End:   run.TradeDate.AddDays(horizonDays),
	}
	// limit 取 horizonDays+2：窗口内最多这么多根日线（自然日 >= 交易日），
	// 多给两根余量兜住交易所临时调休。
	klines, err := s.market.Klines(ctx, run.Code, stock_vo.PeriodDaily, rng, horizonDays+2)
	if err != nil {
		return forwardQuote{}, err
	}
	if len(klines) == 0 {
		return forwardQuote{skipReason: value_objects.SkipNoBasePrice}, nil
	}
	if len(klines) < 2 {
		// 只有基准这一根：前瞻窗口里还没有行情，通常是刚跑完的分析。
		// 这不是错误，等几天再跑一次回测它就能评上分了。
		return forwardQuote{skipReason: value_objects.SkipNoForwardPrice}, nil
	}

	base := klines[len(klines)-1]
	forward := klines[0]
	if base.Close.IsZero() || base.Close.IsNegative() {
		return forwardQuote{skipReason: value_objects.SkipNoBasePrice}, nil
	}
	// 前瞻价同样要校验，而且这一条比基准价那条更要紧。
	//
	// 停牌股或脏行情把收盘价写成 0 时，收益率算出来是 -100%，
	// 实际方向被判成「跌」，于是任何一条 sell/reduce 建议都白捡一个命中。
	// 这不是「少评一条样本」，是往一致率里静默灌假命中——而一致率正是
	// 这整套评测唯一的产出。
	if forward.Close.IsZero() || forward.Close.IsNegative() {
		return forwardQuote{skipReason: value_objects.SkipNoForwardPrice}, nil
	}
	return forwardQuote{
		base:        base.Close,
		forward:     forward.Close,
		forwardDate: forward.TradeDate,
	}, nil
}
