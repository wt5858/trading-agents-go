package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	agent_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/di/injectors"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

var (
	backfillSymbols   string
	backfillFrom      string
	backfillTo        string
	backfillEvery     int
	backfillDepth     int
	backfillAnalysts  string
	backfillModel     string
	backfillMaxPerRun string
	backfillMaxTotal  string
	backfillDryRun    bool
	backfillRedo      bool
)

// backfillCmd 对历史交易日批量补跑分析，为评测积累样本。
//
// # 它为什么存在
//
// 评测的样本量等于历史上真跑过多少次分析，而交互式使用一天也积累不了几条。
// 靠日历等下去是以月为单位的——而前瞻行情**此刻就在库里**：
// 对 2026-03-02 跑一次分析，评它之后 7 天的涨跌，那 7 天早就过完了。
// 因此样本积累的真正瓶颈不是时间，是预算。这个命令把那个瓶颈显式化：
// 先用 --dry-run 看清要跑多少格、按历史均价大概花多少，再决定放不放量。
//
// # 为什么不走任务队列
//
// 理由写在 injectors.CreateBackfillDeps 上：占并发名额会把线上用户挤掉，
// 而回填是可以慢慢跑的，有人正等着看的那次不行。
var backfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "对历史交易日批量补跑分析，为评测积累样本",
	Long: `按「标的 × 交易日」的网格批量补跑分析，产出的运行轨迹供 backtest 评测使用。

交易日取自本地 K 线里**真实存在**的那些，不按日历推算：
节假日与停牌日跑出来的只能是缺数据的分析，既花钱又给样本掺噪声。

强烈建议先用 --dry-run 看清规模与预计花费。首次使用时库里没有历史成本，
估算会明确说明这一点——此时应当先跑一个小批（比如 2 只票 × 5 个交易日）做成本校准。

断点续跑是默认行为：同一格的 runID 是确定性的，已经成功跑过的会被跳过，
用 --redo 可以强制重跑。

本工具仅用于学习与研究，产出不构成任何投资建议。`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runBackfill(cmd.Context())
	},
}

func init() {
	f := backfillCmd.Flags()
	f.StringVar(&backfillSymbols, "symbols", "", "标的代码，逗号分隔，例如 600519,000001（必填）")
	f.StringVar(&backfillFrom, "from", "", "起始交易日 YYYY-MM-DD（必填）")
	f.StringVar(&backfillTo, "to", "", "结束交易日 YYYY-MM-DD（必填）")
	f.IntVar(&backfillEvery, "every", 5, "每隔几个交易日取一个，1 表示每个交易日都跑")
	f.IntVar(&backfillDepth, "depth", 3, "研究深度 1-5")
	f.StringVar(&backfillAnalysts, "analysts", "",
		"分析师 ID，逗号分隔，留空用该深度的默认阵容。做对照实验时务必显式指定，"+
			"否则不同深度会连分析师人数一起变，得到的不是单变量对照")
	f.StringVar(&backfillModel, "model", "", "指定模型，留空走默认路由")
	f.StringVar(&backfillMaxPerRun, "max-cost-per-run", "", "单次分析的成本上限（美元），留空不限")
	f.StringVar(&backfillMaxTotal, "max-total-cost", "", "整批的成本上限（美元），达到即停，留空不限")
	f.BoolVar(&backfillDryRun, "dry-run", false, "只估算规模与花费，不真的跑")
	f.BoolVar(&backfillRedo, "redo", false, "强制重跑已经成功跑过的格子")
	RootCmd.AddCommand(backfillCmd)
}

// backfillPlan 是一次回填的执行计划。
type backfillPlan struct {
	codes   []shared_vo.StockCode
	dates   []shared_vo.TradeDate
	cells   []backfillCell
	skipped int
}

type backfillCell struct {
	runID string
	code  shared_vo.StockCode
	date  shared_vo.TradeDate
}

func runBackfill(ctx context.Context) error {
	if strings.TrimSpace(backfillSymbols) == "" {
		return fmt.Errorf("--symbols 不能为空")
	}
	window, err := shared_vo.NewDateRange(backfillFrom, backfillTo)
	if err != nil {
		return err
	}
	depth := analysis_vo.Depth(backfillDepth)
	if !depth.Valid() {
		return fmt.Errorf("--depth 必须在 1 到 5 之间，收到 %d", backfillDepth)
	}
	maxPerRun, err := parseUSD(backfillMaxPerRun, "--max-cost-per-run")
	if err != nil {
		return err
	}
	maxTotal, err := parseUSD(backfillMaxTotal, "--max-total-cost")
	if err != nil {
		return err
	}

	cfg, log, err := bootstrap()
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	if ctx == nil {
		ctx = context.Background()
	}
	// Ctrl-C 之后不硬杀：正在跑的那一格已经花过钱了，让它把轨迹写完再退出，
	// 否则那笔钱换不回任何样本。
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := injectors.CreateBackfillDeps(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("装配回填依赖失败: %w", err)
	}
	defer func() { _ = deps.Conns.Close(context.WithoutCancel(ctx)) }()

	plan, err := buildBackfillPlan(ctx, deps, window, depth)
	if err != nil {
		return err
	}
	if len(plan.cells) == 0 {
		fmt.Println("没有需要跑的格子（可能全部已跑过，或区间内没有交易日）。")
		return nil
	}

	stats, err := deps.Runs.RunCostStatsOf(ctx, depth.Int())
	if err != nil {
		return err
	}
	printBackfillPlan(plan, depth, stats, maxTotal)

	if backfillDryRun {
		fmt.Println("\n--dry-run：未执行任何分析。")
		return nil
	}
	return executeBackfill(ctx, deps, plan, depth, maxPerRun, maxTotal)
}

// buildBackfillPlan 组网格。
func buildBackfillPlan(
	ctx context.Context,
	deps *providers.BackfillDeps,
	window shared_vo.DateRange,
	depth analysis_vo.Depth,
) (backfillPlan, error) {
	var plan backfillPlan
	for _, raw := range strings.Split(backfillSymbols, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		code, err := shared_vo.NewStockCode(raw, shared_vo.MarketCN)
		if err != nil {
			return plan, fmt.Errorf("标的 %s 非法: %w", raw, err)
		}
		plan.codes = append(plan.codes, code)
	}
	if len(plan.codes) == 0 {
		return plan, fmt.Errorf("--symbols 解析后为空")
	}

	dates, err := tradingDaysOf(ctx, deps, plan.codes[0], window)
	if err != nil {
		return plan, err
	}
	if len(dates) == 0 {
		return plan, fmt.Errorf("标的 %s 在 %s ~ %s 之间没有任何 K 线，无法确定交易日",
			plan.codes[0].FullSymbol(), window.Start.String(), window.End.String())
	}
	plan.dates = sampleEvery(dates, backfillEvery)

	ids := make([]string, 0, len(plan.codes)*len(plan.dates))
	cells := make([]backfillCell, 0, cap(ids))
	for _, code := range plan.codes {
		for _, d := range plan.dates {
			id := backfillRunID(code, d, depth)
			ids = append(ids, id)
			cells = append(cells, backfillCell{runID: id, code: code, date: d})
		}
	}

	if backfillRedo {
		plan.cells = cells
		return plan, nil
	}
	existing, err := deps.Runs.ExistingRunIDs(ctx, ids)
	if err != nil {
		return plan, err
	}
	for _, c := range cells {
		if _, ok := existing[c.runID]; ok {
			plan.skipped++
			continue
		}
		plan.cells = append(plan.cells, c)
	}
	return plan, nil
}

// sampleEvery 每隔 n 个取一个，永远保留第一个。
//
// every <= 0 时退化为「全取」而不是报错：它来自命令行，一个手滑的 0
// 不该让整条命令失败，而「一个都不取」在任何语境下都不是用户的意图。
//
// 抽样是必要的：相邻交易日的分析结论高度相关（同一批新闻、几乎相同的技术形态），
// 每个交易日都跑会让样本量虚高而有效信息不增，白花的是等比例的钱。
func sampleEvery(dates []shared_vo.TradeDate, every int) []shared_vo.TradeDate {
	if every < 1 {
		every = 1
	}
	out := make([]shared_vo.TradeDate, 0, len(dates)/every+1)
	for i := 0; i < len(dates); i += every {
		out = append(out, dates[i])
	}
	return out
}

// backfillRunID 是一格的确定性 ID。
//
// 确定性是断点续跑的全部基础：轨迹落库走的是按 _id 的 upsert，
// 因此同一格重跑只会覆盖自己，不会在集合里堆出两份无从分辨的记录。
// 深度进 ID 则是为了让对照实验的不同臂各自独立——
// 少了它，depth 1 的那一轮会把 depth 3 的结果原地覆盖掉，
// 而两份数据都还在的时候你根本不会发现。
func backfillRunID(code shared_vo.StockCode, d shared_vo.TradeDate, depth analysis_vo.Depth) string {
	return fmt.Sprintf("bf_%s_%s_d%d", code.Symbol, d.Compact(), depth.Int())
}

// tradingDaysOf 用一只基准标的的日线推出区间内的真实交易日。
//
// 取一只而不是取并集：并集会把个别标的的停牌日混进来，
// 而那些日子对其他标的是正常交易日、对它自己却没有数据。
// 用一只流动性好的票当日历，是这里唯一简单且不会错的做法——
// 代价是它自己停牌的那几天会被漏掉，那比多跑一批缺数据的分析划算。
func tradingDaysOf(
	ctx context.Context,
	deps *providers.BackfillDeps,
	code shared_vo.StockCode,
	window shared_vo.DateRange,
) ([]shared_vo.TradeDate, error) {
	// limit 给足：一年约 244 个交易日，多取无害，少取会静默截断区间。
	klines, err := deps.Market.Klines(ctx, code, stock_vo.PeriodDaily, window, 5000)
	if err != nil {
		return nil, err
	}
	// Klines 按 trade_date 倒序返回，这里要正序。
	out := make([]shared_vo.TradeDate, 0, len(klines))
	for i := len(klines) - 1; i >= 0; i-- {
		out = append(out, klines[i].TradeDate)
	}
	return out, nil
}

func printBackfillPlan(
	plan backfillPlan,
	depth analysis_vo.Depth,
	stats agent_repo.RunCostStats,
	maxTotal decimal.Decimal,
) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	fmt.Fprintf(w, "\n标的数\t%d\n", len(plan.codes))
	fmt.Fprintf(w, "交易日数\t%d（每 %d 个交易日取一个）\n", len(plan.dates), backfillEvery)
	fmt.Fprintf(w, "研究深度\t%d\n", depth.Int())
	fmt.Fprintf(w, "待跑格子\t%d\n", len(plan.cells))
	if plan.skipped > 0 {
		fmt.Fprintf(w, "已跑过跳过\t%d（--redo 可强制重跑）\n", plan.skipped)
	}

	if stats.Samples == 0 {
		fmt.Fprintf(w, "\n预计花费\t无法估算——库里还没有 depth %d 的历史运行。\n", depth.Int())
		fmt.Fprintf(w, "\t建议先跑一个小批（例如 2 只票 × 5 个交易日）做成本校准，\n")
		fmt.Fprintf(w, "\t拿到真实单价之后再决定放量规模。\n")
		return
	}
	n := decimal.NewFromInt(int64(len(plan.cells)))
	fmt.Fprintf(w, "\n历史单次成本\t均值 $%s，最高 $%s（基于 %d 条 depth %d 的运行）\n",
		stats.AvgUSD.StringFixed(4), stats.MaxUSD.StringFixed(4), stats.Samples, depth.Int())
	fmt.Fprintf(w, "预计总花费\t约 $%s（按均值）\n", stats.AvgUSD.Mul(n).StringFixed(2))
	fmt.Fprintf(w, "最坏情况\t约 $%s（按历史最高单价）\n", stats.MaxUSD.Mul(n).StringFixed(2))
	if stats.Samples < 10 {
		fmt.Fprintf(w, "注意\t样本只有 %d 条，这个均值不足以作为预算依据。\n", stats.Samples)
	}
	if maxTotal.GreaterThan(decimal.Zero) {
		fmt.Fprintf(w, "整批上限\t$%s，达到即停\n", maxTotal.StringFixed(2))
	} else {
		fmt.Fprintf(w, "整批上限\t未设置（--max-total-cost），将一直跑到全部格子结束\n")
	}
}

func parseUSD(raw, flag string) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.Zero, nil
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s 不是合法金额: %w", flag, err)
	}
	if v.LessThan(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("%s 不能为负", flag)
	}
	return v, nil
}

func backfillAnalystList() []string {
	if strings.TrimSpace(backfillAnalysts) == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(backfillAnalysts, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func executeBackfill(
	ctx context.Context,
	deps *providers.BackfillDeps,
	plan backfillPlan,
	depth analysis_vo.Depth,
	maxPerRun, maxTotal decimal.Decimal,
) error {
	fmt.Printf("\n开始回填 %d 格。Ctrl-C 会在当前这一格跑完之后停下。\n\n", len(plan.cells))

	// 单次上限作用在引擎的一个副本上，不碰 Wire 造出来的那个共享实例。
	engine := deps.Engine
	if maxPerRun.GreaterThan(decimal.Zero) {
		engine = engine.WithCostCeiling(maxPerRun)
		fmt.Printf("单次成本上限 $%s：超出后该次分析的剩余阶段不再执行。\n\n", maxPerRun.StringFixed(4))
	}

	var (
		spent  decimal.Decimal
		ok     int
		failed int
	)
	started := time.Now()

	for i, cell := range plan.cells {
		if err := ctx.Err(); err != nil {
			fmt.Printf("\n已中断。\n")
			break
		}
		// 总预算在每一格之前判。判在后面（跑完再看）等于每次都允许超一格，
		// 而一格就是几十次模型调用。
		if maxTotal.GreaterThan(decimal.Zero) && spent.GreaterThanOrEqual(maxTotal) {
			fmt.Printf("\n已达整批成本上限 $%s，停止。剩余 %d 格未跑。\n",
				maxTotal.StringFixed(2), len(plan.cells)-i)
			break
		}

		req, err := analysis_vo.NewRequest(cell.code, cell.date, depth, backfillAnalystList(), backfillModel)
		if err != nil {
			return fmt.Errorf("构造请求失败: %w", err)
		}

		cellStart := time.Now()
		result, err := engine.Run(ctx, cell.runID, req, nil)
		switch {
		case err != nil:
			failed++
			fmt.Printf("[%d/%d] %s %s  失败: %v\n",
				i+1, len(plan.cells), cell.code.FullSymbol(), cell.date.String(), err)
		default:
			ok++
			spent = spent.Add(result.Usage.CostUSD)
			fmt.Printf("[%d/%d] %s %s  %s  $%s  %s\n",
				i+1, len(plan.cells), cell.code.FullSymbol(), cell.date.String(),
				result.Decision.Action.DisplayName(),
				result.Usage.CostUSD.StringFixed(4),
				time.Since(cellStart).Round(time.Second))
		}
	}

	fmt.Printf("\n完成 %d，失败 %d，实际花费 $%s，总耗时 %s。\n",
		ok, failed, spent.StringFixed(4), time.Since(started).Round(time.Second))
	fmt.Printf("用 `backtest --from %s --to %s` 对这批样本评分。\n",
		plan.dates[0].String(), plan.dates[len(plan.dates)-1].String())
	return nil
}
