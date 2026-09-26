package domain_services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// EngineConfig 是数据准备阶段的取数参数。
type EngineConfig struct {
	// KlineLookbackDays 是计算技术指标所需的 K 线回看天数。
	// 60 日均线要 60 个交易日，加上停牌与节假日，250 个自然日是一个安全的下限。
	KlineLookbackDays int
	KlineLimit        int
	NewsLookbackDays  int
	NewsLimit         int
	SocialLimit       int
	FinancialLimit    int
	// IndicatorTTL 是「当日指标快照」的保鲜期。
	// 历史交易日的指标一经收盘即为定值，永不过期；只有当天的快照会随盘中价格变化。
	IndicatorTTL time.Duration
	// DataFanOutLimit 是数据准备阶段并行取数的上限。
	DataFanOutLimit int
	// MaxCostUSD 是单次分析允许花掉的上限，零值或负数表示不设限。
	//
	// 它是软护栏：检查发生在每位成员开跑之前，并行阶段因此可能超出一个批次的量。
	// 要挡的是「一批无人值守的回测把预算烧穿一个数量级」，不是精确停在某个数字上。
	// 详见 entities.Orchestrator.costCeilingUSD。
	//
	// 默认不设限是刻意的：交互式提交的单次分析有人盯着，
	// 而真正需要护栏的批量回测会显式传一个值——反过来给一个「合理默认」，
	// 只会让某天某次正常的深度分析在最后一个阶段前被静默砍掉。
	MaxCostUSD decimal.Decimal
	// MemoryEnabled 决定要不要把「本系统对该标的的历史战绩」喂回提示词。
	//
	// **默认关闭，而且应当一直关着，直到有一个数字说明它是好是坏。**
	//
	// 把「你上次看多、其后跌了 8%」告诉模型是一个会改变结论的干预，不是一条中性的补充数据。
	// 在单只标的只有三五条历史记录的量级上，它很可能有害：那点样本不构成任何证据，
	// 而模型会把它当成强信号过度修正，表现为结论开始跟着最近一次的对错摇摆。
	// 因此它的正确用法是作为一个**独立的对照臂**跑一遍配对实验，
	// 拿到「开与不开，一致率差多少、区间是否重叠」之后再决定，
	// 而不是默认打开、悄悄改变所有人的结论。
	MemoryEnabled bool
}

const (
	defaultKlineLookbackDays = 250
	defaultEngineKlineLimit  = 250
	defaultNewsLookbackDays  = 30
	defaultEngineNewsLimit   = 15
	defaultEngineSocialLimit = 30
	defaultEngineFinLimit    = 8
	defaultIndicatorTTL      = 30 * time.Minute

	// saveRunTimeout 是落库运行轨迹的超时。
	//
	// 它必须是一个独立的短超时：这一步常常发生在业务 ctx 已经超时之后
	// （一次跑挂的分析），沿用原 ctx 会让它必然失败。给一个短上限则是因为
	// 轨迹写不进去也不该拖住 worker——这时候 worker 还欠着一次任务状态的收尾。
	saveRunTimeout = 5 * time.Second

	// defaultDataFanOut 是数据准备阶段的并发上限。
	//
	// 这一阶段是四个彼此独立的 Mongo 查询（行情/财务/资讯/舆情），
	// 4 就是全并行。它发生在任何智能体开跑之前，不会和分析师阶段的扇出叠加，
	// 因此不需要像分析师那样压低。
	defaultDataFanOut = 4
)

func (c EngineConfig) normalized() EngineConfig {
	if c.KlineLookbackDays <= 0 {
		c.KlineLookbackDays = defaultKlineLookbackDays
	}
	if c.KlineLimit <= 0 {
		c.KlineLimit = defaultEngineKlineLimit
	}
	if c.NewsLookbackDays <= 0 {
		c.NewsLookbackDays = defaultNewsLookbackDays
	}
	if c.NewsLimit <= 0 {
		c.NewsLimit = defaultEngineNewsLimit
	}
	if c.SocialLimit <= 0 {
		c.SocialLimit = defaultEngineSocialLimit
	}
	if c.FinancialLimit <= 0 {
		c.FinancialLimit = defaultEngineFinLimit
	}
	if c.IndicatorTTL <= 0 {
		c.IndicatorTTL = defaultIndicatorTTL
	}
	if c.DataFanOutLimit <= 0 {
		c.DataFanOutLimit = defaultDataFanOut
	}
	return c
}

// EngineService 实现 analysis 上下文声明的 Engine 端口。
//
// 它是本上下文唯一的对外用例入口：收一个 Request，跑完五个阶段，回一个 Result。
// 全程不碰 Task 聚合——任务状态机是 analysis 上下文的事，引擎只负责
// 「给定输入算出结论」这一件事。
type EngineService struct {
	crew       entities.Crew
	runtime    entities.Runtime
	market     MarketReader
	backfill   MarketBackfiller
	indicators *repositories.IndicatorRepository
	runs       *repositories.AnalysisRunRepository
	// evals 只服务于 MemoryEnabled 时的战绩回灌，可为 nil。
	evals     *repositories.EvaluationRepository
	publisher domain_event.Publisher
	log       *zap.Logger
	cfg       EngineConfig
}

var _ analysis_services.Engine = (*EngineService)(nil)

func NewEngineService(
	runtime entities.Runtime,
	market MarketReader,
	backfill MarketBackfiller,
	indicators *repositories.IndicatorRepository,
	runs *repositories.AnalysisRunRepository,
	evals *repositories.EvaluationRepository,
	publisher domain_event.Publisher,
	log *zap.Logger,
	cfg EngineConfig,
) *EngineService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &EngineService{
		crew:       entities.NewCrew(),
		runtime:    runtime,
		market:     market,
		backfill:   backfill,
		indicators: indicators,
		runs:       runs,
		evals:      evals,
		publisher:  publisher,
		log:        log,
		cfg:        cfg.normalized(),
	}
}

// WithCostCeiling 返回一个成本上限被覆盖的副本。
//
// 必须返回副本而不是就地修改：同一个 EngineService 实例由 HTTP 提交、队列消费者
// 和回填命令共享（Wire 造的是单例），就地改会让一次回填设的紧预算
// 泄漏到线上用户的分析里，表现为「某天开始所有深度分析都在风控阶段前被砍掉」。
//
// 浅拷贝是安全的：本结构体没有锁，字段要么是不可变值（cfg、crew），
// 要么是本来就被共享的指针与接口（仓储、运行时、日志）——
// 副本与原件指向同一批依赖，这正是期望的行为。
func (s *EngineService) WithCostCeiling(limit decimal.Decimal) *EngineService {
	clone := *s
	clone.cfg.MaxCostUSD = limit
	return &clone
}

// Run 执行一次完整分析。
//
// 五个阶段：数据准备 -> 六位分析师并行 -> 多空辩论串行 -> 交易员 -> 风控辩论并行 + 终裁。
// 阶段顺序与失败语义全在 entities.Orchestrator 里，本方法只负责准备数据、
// 装配编排器、把产出翻译成 analysis 上下文的 Result。
//
// 进度键与 analysis/value_objects/progress.go 的 NewProgress 逐一对应：
// prepare / analyst:<id> / debate:bull|bear|manager / trade /
// risk:aggressive|conservative|neutral|manager / report。
// 这里只负责首尾两个（prepare 与 report），中间的由编排器按成员契约汇报。
func (s *EngineService) Run(
	ctx context.Context,
	runID string,
	req analysis_vo.Request,
	reporter analysis_services.ProgressReporter,
) (*analysis_vo.Result, error) {
	if s.runtime == nil {
		return nil, custom_errors.Internal("分析引擎未配置运行时")
	}
	sink := progressSinkOf(reporter)
	started := time.Now()

	ac := entities.NewAnalysisContext(runID, req)

	// ---- 阶段一：数据准备 ----
	brief, err := s.collect(ctx, req)
	if err != nil {
		sink.StepFailed(value_objects.StepPrepare.String(), custom_errors.MessageOf(err))
		// 这一步失败时一位成员都还没发言，轨迹里只有一句「为什么没跑起来」。
		// 照样落库：排查「这只票的分析总是失败」时，第一个要区分的就是
		// 「数据没备齐」与「模型挂了」，而后者的轨迹里是有发言记录的。
		s.saveRun(ctx, ac, err)
		return nil, err
	}
	ac.LoadMarketBrief(brief)
	sink.Step(value_objects.StepPrepare.String(), prepareDetail(brief))
	dataPhase := entities.StageOutcome{
		Phase:    value_objects.PhaseDataCollection,
		Duration: time.Since(started),
	}

	// ---- 阶段二到五：交给编排器 ----
	plan, err := entities.NewPlan(req, s.crew)
	if err != nil {
		s.saveRun(ctx, ac, err)
		return nil, err
	}
	outcomes, runErr := entities.NewOrchestrator(plan, sink).
		WithCostCeiling(s.cfg.MaxCostUSD).
		Run(ctx, s.runtime, ac)

	// 事件先发、轨迹紧随：无论成败，已经发生的消耗与失败都是既成事实，
	// 计费与监控不该因为整体失败而丢掉这些记录。
	// 失败那次的轨迹恰恰是最值得留下的——成功的分析没人会去回放。
	s.publish(ctx, ac)
	s.saveRun(ctx, ac, runErr)

	if runErr != nil {
		return nil, runErr
	}

	// ---- 收尾：装配结果 ----
	decision := ac.FinalDecision().Decision
	reports := ac.Reports()
	usage := ac.Usage()

	phases := make([]analysis_vo.PhaseOutcome, 0, len(outcomes)+1)
	phases = append(phases, toPhaseOutcome(dataPhase))
	phases = append(phases, mergePhases(outcomes)...)

	sink.Step(value_objects.StepReport.String(),
		fmt.Sprintf("已汇总 %d 份报告", len(reports)))

	result := analysis_vo.NewResult(req.Code, req.TradeDate, decision, reports,
		analysis_vo.TokenUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			Calls:            usage.Calls,
			CostUSD:          usage.CostUSD,
		}, phases)
	return &result, nil
}

// RunSolo 跑对照组：一位独立分析师，看**同一份** MarketBrief，一次给出决策。
//
// # 这个方法存在的唯一目的
//
// 回答「十四位成员的分工、辩论与终裁，相对一次直答到底多值多少」。
// 它与 Run 配对使用：同一个标的、同一个交易日各跑一次，比较两边的决策方向。
//
// # 为什么必须复用 collect 而不是另写一条取数路径
//
// 这是整个实验唯一的承重约束：两边看到的素材必须逐字节相同。
// 一旦对照组走自己的取数逻辑（哪怕只是回看天数差几天），
// 测出来的差异里就混进了「谁拿到的数据更好」，而那个变量的影响
// 大概率比编排本身还大——于是这个实验测的就不再是它声称要测的东西。
// 复用 collect 让这条约束由代码结构保证，不依赖两处逻辑保持同步。
//
// # 为什么不汇报进度
//
// 对照组不是主流程的一步，NewProgress 里没有它的格子。
// 传进来的 reporter 会被忽略，理由见 value_objects.StepSolo。
func (s *EngineService) RunSolo(
	ctx context.Context,
	runID string,
	req analysis_vo.Request,
) (*analysis_vo.Result, error) {
	if s.runtime == nil {
		return nil, custom_errors.Internal("分析引擎未配置运行时")
	}
	started := time.Now()
	ac := entities.NewAnalysisContext(runID, req)

	brief, err := s.collect(ctx, req)
	if err != nil {
		s.saveRun(ctx, ac, err)
		return nil, err
	}
	ac.LoadMarketBrief(brief)
	dataPhase := entities.StageOutcome{
		Phase:    value_objects.PhaseDataCollection,
		Duration: time.Since(started),
	}

	// 单成员的「编排」：一个阶段、一位成员。
	//
	// 走编排器而不是直接调 Runtime.Execute，是为了让对照组与实验组共用
	// 同一套失败语义、成本护栏与轨迹记账——否则两边的成本统计口径会悄悄分叉，
	// 而这个实验要报的恰恰是成本倍数。
	solo := entities.NewSoloAnalyst()
	plan := entities.Plan{Stages: []entities.Stage{{
		Phase:      value_objects.PhaseTrading,
		Mode:       value_objects.ModeSequential,
		Members:    []entities.Agent{solo},
		MinSuccess: 1,
	}}}

	outcomes, runErr := entities.NewOrchestrator(plan, nil).
		WithCostCeiling(s.cfg.MaxCostUSD).
		Run(ctx, s.runtime, ac)

	s.publish(ctx, ac)
	s.saveRun(ctx, ac, runErr)
	if runErr != nil {
		return nil, runErr
	}

	usage := ac.Usage()
	phases := make([]analysis_vo.PhaseOutcome, 0, len(outcomes)+1)
	phases = append(phases, toPhaseOutcome(dataPhase))
	phases = append(phases, mergePhases(outcomes)...)

	result := analysis_vo.NewResult(req.Code, req.TradeDate,
		ac.FinalDecision().Decision, ac.Reports(),
		analysis_vo.TokenUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			Calls:            usage.Calls,
			CostUSD:          usage.CostUSD,
		}, phases)
	return &result, nil
}

// saveRun 落库本次运行的轨迹。cause 为 nil 表示正常收尾。
//
// 落库失败只记日志、不改变本次分析的成败：轨迹是观测数据，
// 让一次已经算出结论的分析因为「日志没写进去」而对用户报错，
// 是把观测手段的可用性绑在了业务路径上。反过来也成立——
// 分析失败了轨迹照样要写，那是唯一一份能解释失败原因的东西。
func (s *EngineService) saveRun(ctx context.Context, ac *entities.AnalysisContext, cause error) {
	if s.runs == nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = custom_errors.MessageOf(cause)
	}
	// 用一个独立的 ctx：走到这里时业务 ctx 常常已经因为超时或取消而失效，
	// 而那正是最需要留下轨迹的一次运行。沿用它等于「跑挂的分析一律没有轨迹」。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveRunTimeout)
	defer cancel()

	if err := s.runs.Save(saveCtx, ac, time.Now(), reason); err != nil {
		s.log.Warn("保存分析轨迹失败",
			zap.String("run_id", ac.RunID()),
			zap.String("symbol", ac.Code().FullSymbol()),
			zap.Error(err))
	}
}

// ---------------------------------------------------------------------------
// 数据准备
// ---------------------------------------------------------------------------

// collect 备齐一次分析所需的全部素材。
//
// 它是整条流水线上唯一允许触发取数的地方。十四位成员的工具调用只读本地数据，
// 原因是工具调用发生在并发扇出里——在那里回源，一次分析能打出几十个外部请求，
// 而这些数据本来一次就能取全。
func (s *EngineService) collect(ctx context.Context, req analysis_vo.Request) (entities.MarketBrief, error) {
	brief := entities.MarketBrief{}
	period := stock_vo.PeriodDaily

	// K 线与指标必须串行：指标是从 K 线算出来的。
	klines, err := s.loadKlines(ctx, req, period)
	if err != nil {
		return brief, err
	}
	brief.Klines = klines

	snapshot, err := s.resolveIndicators(ctx, req, period, klines)
	if err != nil {
		// 指标缺失不阻断分析：技术面分析师会如实报告「指标不可用」，
		// 而基本面、新闻面、情绪面的结论完全不受影响。
		s.log.Warn("技术指标不可用",
			zap.String("symbol", req.Code.FullSymbol()), zap.Error(err))
		brief.Missing = append(brief.Missing, "技术指标")
	} else {
		brief.Indicators = snapshot.Indicators
	}

	// 其余四类数据彼此独立，并行取回。
	//
	// 用 Settle 而不是 Map：这四项没有一项是分析的必要条件——
	// 没有社交舆情只是让情绪分析师少一个视角，不该让整次分析失败。
	// 有并发上限：这一步跑在业务库上，批量分析时几十个任务同时进这一步，
	// 不设限会把连接池打满。
	var (
		quote      *stock_vo.Quote
		financials []stock_vo.Financial
		news       []stock_vo.News
		social     []stock_vo.SocialPost
	)
	newsRange := lookbackRange(req.TradeDate, s.cfg.NewsLookbackDays)

	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"行情快照", func(ctx context.Context) error {
			// 与上面的 newsRange 同一条纪律：右端锁在 req.TradeDate，不用今天。
			q, err := s.market.QuoteAsOf(ctx, req.Code, req.TradeDate)
			if err != nil {
				return err
			}
			quote = q
			return nil
		}},
		{"财务数据", func(ctx context.Context) error {
			// 同一条纪律：只取分析交易日当天已经公布的财报。
			items, err := s.market.Financials(ctx, req.Code, req.TradeDate, s.cfg.FinancialLimit)
			financials = items
			return err
		}},
		{"资讯", func(ctx context.Context) error {
			items, err := s.market.News(ctx, req.Code, newsRange, s.cfg.NewsLimit)
			news = items
			return err
		}},
		{"社交舆情", func(ctx context.Context) error {
			items, err := s.market.SocialPosts(ctx, req.Code, newsRange, s.cfg.SocialLimit)
			social = items
			return err
		}},
	}

	outcomes, err := concurrency.Settle(ctx, steps, s.cfg.DataFanOutLimit,
		func(ctx context.Context, st struct {
			name string
			run  func(context.Context) error
		}) (string, error) {
			return st.name, st.run(ctx)
		})
	if err != nil {
		// Settle 只在父 ctx 被取消时返回错误。
		return brief, custom_errors.Unavailable("数据准备被取消").Wrap(err)
	}
	for i, o := range outcomes {
		if o.Err != nil {
			s.log.Debug("数据准备部分失败",
				zap.String("part", steps[i].name), zap.Error(o.Err))
			brief.Missing = append(brief.Missing, steps[i].name)
		}
	}

	if quote != nil {
		brief.Quote = *quote
	}
	brief.Financials = financials
	brief.News = news
	brief.Social = social
	brief.TrackRecord = s.trackRecordOf(ctx, req)

	// 一份素材都没有时没有分析的必要：让十四位成员对着空白轮流发言，
	// 只会产出十四份措辞漂亮的臆测。
	if len(brief.Klines) == 0 && quote == nil && len(financials) == 0 && len(news) == 0 {
		return brief, custom_errors.Unavailable("股票(%s) 在 %s 没有任何可用数据",
			req.Code.FullSymbol(), req.TradeDate.String())
	}
	return brief, nil
}

// trackRecordOf 渲染「本系统对该标的的历史战绩」，未开启时返回空串。
//
// 查询失败只记日志、返回空串，绝不让整次分析失败：这是一条可选的补充素材，
// 让一次已经备齐全部行情的分析因为「战绩查不到」而报错，
// 是把一个实验性功能的可用性绑在了主流程上。
//
// 只统计**严格早于**本次交易日的记录。这条过滤是这个功能的成立前提：
// 少了它，对历史交易日跑回填时，模型会看到那一天之后才产生的评分结果——
// 一个不会报错、只会让回测结果准得可疑的未来函数。
func (s *EngineService) trackRecordOf(ctx context.Context, req analysis_vo.Request) string {
	if !s.cfg.MemoryEnabled || s.evals == nil {
		return ""
	}
	rec, err := s.evals.TrackRecordOf(ctx, req.Code.Symbol)
	if err != nil {
		s.log.Warn("查询历史战绩失败", zap.String("symbol", req.Code.FullSymbol()), zap.Error(err))
		return ""
	}
	return renderTrackRecord(rec, req.TradeDate.String(), trackRecordShowLimit)
}

// renderTrackRecord 过滤并渲染战绩，是这个功能里唯一有判断的一段。
//
// 与查询分开是为了能被测试直接钉住：下面那条 cutoff 过滤是整个功能的成立前提，
// 而它的失效不会报错、只会让回测结果准得可疑。
//
// cutoff 为空串表示实时分析（请求没带交易日），此时全部历史都是过去，不需要过滤。
func renderTrackRecord(rec repositories.TrackRecord, cutoff string, limit int) string {
	var (
		sb     strings.Builder
		scored int
		hits   int
		shown  int
	)
	for _, sample := range rec.Samples {
		// 交易日是定长 ISO 串，字典序即时间序。
		//
		// 用 >= 而不是 >：本次交易日当天的那条记录同样要排除。
		// 它评的正是「这一天的建议后来怎么样」——把它喂回给正在做这一天决策的模型，
		// 等于直接把答案告诉它。这是这段代码里最容易写错、也最难发现的一个字符。
		if cutoff != "" && sample.TradeDate >= cutoff {
			continue
		}
		scored++
		if sample.Hit {
			hits++
		}
		if shown >= limit {
			continue
		}
		shown++
		verdict := "未兑现"
		if sample.Hit {
			verdict = "兑现"
		}
		fmt.Fprintf(&sb, "- %s 建议「%s」，其后 %s%%（%s）\n",
			sample.TradeDate, sample.Action, sample.ReturnPct.StringFixed(2), verdict)
	}
	if scored == 0 {
		return ""
	}
	return fmt.Sprintf("累计 %d 次已评分建议，方向判对 %d 次。最近几次：\n%s",
		scored, hits, strings.TrimRight(sb.String(), "\n"))
}

// trackRecordShowLimit 是写进提示词的战绩条数上限。
// 战绩会内联进十四位成员的每一份提示词，全量列出会挤占本该留给行情的上下文。
const trackRecordShowLimit = 8

// loadKlines 取 K 线，本地为空时走一次回源。
//
// 回源只发生在这里、只发生一次，且在任何扇出开始之前。
func (s *EngineService) loadKlines(ctx context.Context, req analysis_vo.Request, period stock_vo.Period) ([]stock_vo.Kline, error) {
	if s.market == nil {
		return nil, custom_errors.Internal("分析引擎未配置行情读取端口")
	}
	end := req.TradeDate.OrToday()
	rng := shared_vo.DateRange{Start: end.AddDays(-s.cfg.KlineLookbackDays), End: end}

	klines, err := s.market.Klines(ctx, req.Code, period, rng, s.cfg.KlineLimit)
	if err != nil {
		return nil, err
	}
	if len(klines) > 0 || s.backfill == nil {
		return klines, nil
	}

	fetched, err := s.backfill.Klines(ctx, stock_services.KlineQuery{
		Code:   req.Code.Symbol,
		Market: string(req.Code.Market),
		Period: period.String(),
		Start:  rng.Start.String(),
		End:    rng.End.String(),
		Limit:  s.cfg.KlineLimit,
	})
	if err != nil {
		// 回源失败不算致命：没有 K 线只是让技术面缺席。
		s.log.Warn("K 线回源失败", zap.String("symbol", req.Code.FullSymbol()), zap.Error(err))
		return nil, nil
	}
	return fetched, nil
}

// resolveIndicators 取得本次分析要用的技术指标快照。
//
// # 这是「派生量必须落库、读路径不得重算」这条规则的落点
//
// 顺序是固定的：先读；读不到（或当日快照已过保鲜期）才算；算完立刻落库；
// 然后**重新从库里读一遍**，用读回来的那一份。
//
// 最后这一步的重读看起来多余，其实是整条规则的关键：
//   - 它保证下游用到的数值与库里的字节完全一致。工具 get_technical_indicators
//     读的是库，提示词里的指标段如果用的是内存中刚算出来的值，
//     两者一旦有任何偏差（比如并发的另一个任务同时写入了一份不同窗口的结果），
//     同一份报告里就会出现两个不同的 MA20。
//   - 它让「以库为准」成为一条没有例外的规则，而不是一条有默认分支的建议。
func (s *EngineService) resolveIndicators(
	ctx context.Context,
	req analysis_vo.Request,
	period stock_vo.Period,
	klines []stock_vo.Kline,
) (*entities.IndicatorSnapshot, error) {
	if s.indicators == nil {
		return nil, custom_errors.Unavailable("未配置技术指标仓储")
	}

	existing, err := s.indicators.Find(ctx, req.Code, period, req.TradeDate)
	if err == nil && !existing.Stale(time.Now(), s.cfg.IndicatorTTL) {
		return existing, nil
	}
	if err != nil && custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
		return nil, err
	}
	if len(klines) == 0 {
		return nil, custom_errors.Unavailable("没有 K 线数据，无法计算技术指标")
	}

	source := ""
	if len(klines) > 0 {
		source = klines[0].Source
	}
	computed, err := entities.ComputeIndicatorSnapshot(req.Code, req.TradeDate, period, klines, source)
	if err != nil {
		return nil, err
	}
	if err := s.indicators.Save(ctx, []*entities.IndicatorSnapshot{computed}); err != nil {
		return nil, err
	}
	// 指标快照自己也带领域事件（首次计算），一并发出去。
	if evts := computed.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}

	// 读回落库的那一份，之后全系统看到的都是它。
	stored, err := s.indicators.Find(ctx, req.Code, period, req.TradeDate)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// ---------------------------------------------------------------------------
// 查询用例（供 application/http_handlers 使用）
// ---------------------------------------------------------------------------

// CrewProfile 是一位成员的对外画像。
type CrewProfile struct {
	Kind        string   `json:"kind"`
	DisplayName string   `json:"displayName"`
	Layer       string   `json:"layer"`
	Phase       string   `json:"phase"`
	Step        string   `json:"step"`
	Tools       []string `json:"tools"`
	Policy      string   `json:"policy"`
}

// Roster 返回全体成员的画像，供前端画流程图与工具授权表。
func (s *EngineService) Roster() []CrewProfile {
	out := make([]CrewProfile, 0, len(value_objects.AllKinds()))
	for _, kind := range value_objects.AllKinds() {
		m := s.crew.Member(kind)
		if m == nil {
			continue
		}
		c := m.Contract()
		tools := make([]string, 0, c.Access.Len())
		for _, n := range c.Access.Names() {
			tools = append(tools, n.String())
		}
		out = append(out, CrewProfile{
			Kind:        c.Kind.String(),
			DisplayName: c.DisplayName,
			Layer:       c.Layer.DisplayName(),
			Phase:       c.Phase.DisplayName(),
			Step:        c.Step.String(),
			Tools:       tools,
			Policy:      c.Policy.String(),
		})
	}
	return out
}

// Indicators 读取已落库的技术指标快照。
//
// 纯读：查不到就是查不到，绝不在这条路径上现算一份——
// 现算出来的值不会被落库，于是同一只票同一天会出现两个不同的 MA20，
// 一个在报告里，一个在这个接口的响应里。
func (s *EngineService) Indicators(
	ctx context.Context,
	rawCode, rawMarket, rawPeriod, rawDate string,
) (*entities.IndicatorSnapshot, error) {
	if s.indicators == nil {
		return nil, custom_errors.Unavailable("未配置技术指标仓储")
	}
	code, err := shared_vo.NewStockCode(rawCode, shared_vo.Market(rawMarket))
	if err != nil {
		return nil, err
	}
	period, err := stock_vo.NewPeriod(rawPeriod)
	if err != nil {
		return nil, err
	}
	date, err := shared_vo.NewTradeDate(rawDate)
	if err != nil {
		return nil, err
	}
	return s.indicators.LatestNotAfter(ctx, code, period, date.OrToday())
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// progressSinkOf 把 analysis 的 ProgressReporter 适配成实体层的 ProgressSink。
//
// 两个接口的方法签名逐字相同，因此这里只是一次类型转换，没有任何 key 改写的余地——
// 这正是当初让两边签名保持一致的原因：任何一层「顺手」翻译 key 的机会，
// 都是一次进度条静默卡死的机会。
func progressSinkOf(reporter analysis_services.ProgressReporter) entities.ProgressSink {
	if reporter == nil {
		return entities.NoopProgressSink()
	}
	return reporter
}

func prepareDetail(b entities.MarketBrief) string {
	detail := fmt.Sprintf("K线 %d 根、资讯 %d 条、舆情 %d 条", len(b.Klines), len(b.News), len(b.Social))
	if b.HasIndicators() {
		detail += "、技术指标已就绪"
	}
	if len(b.Missing) > 0 {
		detail += "；缺失: " + formatMissing(b.Missing)
	}
	return detail
}

func toPhaseOutcome(o entities.StageOutcome) analysis_vo.PhaseOutcome {
	return analysis_vo.PhaseOutcome{
		Phase:  o.Phase.String(),
		Agents: kindStrings(o.Agents),
		// 毫秒转秒用 decimal 而不是 Duration.Seconds()：后者返回 float64，
		// 阶段耗时随后要在 mergePhases 里相加，用浮点累加会让同一批阶段
		// 在不同合并顺序下得到不同的总时长。
		DurationS: decimal.NewFromInt(o.Duration.Milliseconds()).DivRound(thousand, 3),
		Failed:    kindStrings(o.Failed),
	}
}

// mergePhases 把阶段结果按 Phase 合并。
//
// 风控阶段在计划里被拆成两段（三位辩手并行 + 经理终裁），但对外只是一个阶段。
// 不合并的话 Result.Phases 里会出现两条 phase="risk" 的记录，
// 前端按阶段名索引时会静默丢掉一条。
func mergePhases(outcomes []entities.StageOutcome) []analysis_vo.PhaseOutcome {
	out := make([]analysis_vo.PhaseOutcome, 0, len(outcomes))
	index := make(map[string]int, len(outcomes))
	for _, o := range outcomes {
		po := toPhaseOutcome(o)
		if i, ok := index[po.Phase]; ok {
			out[i].Agents = append(out[i].Agents, po.Agents...)
			out[i].Failed = append(out[i].Failed, po.Failed...)
			out[i].DurationS = out[i].DurationS.Add(po.DurationS)
			continue
		}
		index[po.Phase] = len(out)
		out = append(out, po)
	}
	return out
}

func kindStrings(kinds []value_objects.AgentKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.String())
	}
	return out
}

func (s *EngineService) publish(ctx context.Context, ac *entities.AnalysisContext) {
	if evts := ac.DrainEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// thousand 用于毫秒转秒。
var thousand = decimal.NewFromInt(1000)
