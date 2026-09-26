package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	agent_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/di/injectors"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

var (
	compareFrom     string
	compareTo       string
	compareDepth    int
	compareLimit    int
	compareMaxTotal string
	compareDryRun   bool
)

// compareCmd 度量多智能体编排相对一次直答的边际价值。
//
// # 为什么是「重合率」而不是「谁更准」
//
// 「谁更准」要拿市场当裁判，而在可达的样本量（几十到几百条）上，
// 两边真实差 5 个百分点也测不出统计显著——无论多智能体是强还是弱，
// 你都会得到「不显著」。用一个输出与输入几乎无关的实验去裁决
// 五千行代码的生死，不是科学。
//
// 重合率绕开了这个死结：它是**配对**设计，市场噪声作为共同项被消掉，
// 不需要 ground truth，n=50 就有可用的区间。它回答的是一个更诚实、
// 也更可答的问题——十四位成员与一次直答，在多大比例的标的上给出了不同的决策。
// 重合率高，说明编排的边际价值上界就那么大；重合率低，说明它确实在做不同的事，
// 那些不一致的样本才值得继续追问谁对。
var compareCmd = &cobra.Command{
	Use:   "compare",
	Short: "对照实验：十四人编排 vs 一次直答，配对比较决策方向",
	Long: `把已经跑过的完整分析（实验组）与一位独立分析师的一次直答（对照组）配对比较。

对照组看到的是**同一份** MarketBrief——这条约束由代码结构保证
（两边都走 EngineService.collect），而不是靠两处逻辑保持同步。
它一旦破坏，测出来的差异里就混进了「谁拿到的数据更好」。

成本只有对照组那一份：实验组的结论直接从已落库的轨迹里读，不重跑。
因此请先用 backfill 积累实验组样本，再跑本命令。

断点续跑是默认行为：已经比过的格子会被跳过。

本工具仅用于学习与研究，产出不构成任何投资建议。`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runCompare(cmd.Context())
	},
}

func init() {
	f := compareCmd.Flags()
	f.StringVar(&compareFrom, "from", "", "起始交易日 YYYY-MM-DD，留空取近半年")
	f.StringVar(&compareTo, "to", "", "结束交易日 YYYY-MM-DD，留空取今天")
	f.IntVar(&compareDepth, "depth", 3, "实验组的研究深度，用于定位 backfill 产出的轨迹")
	f.IntVar(&compareLimit, "limit", 0, "最多比较多少格，0 表示不限")
	f.StringVar(&compareMaxTotal, "max-total-cost", "", "整批成本上限（美元），达到即停")
	f.BoolVar(&compareDryRun, "dry-run", false, "只报规模与预计花费，不真的跑")
	RootCmd.AddCommand(compareCmd)
}

// comparePair 是一格配对。
type comparePair struct {
	code      shared_vo.StockCode
	tradeDate shared_vo.TradeDate
	// crew 是已落库的实验组决策。
	crew analysis_vo.Decision
	// soloRunID 是对照组这一格的确定性 ID。
	soloRunID string
	// done 表示对照组已经跑过，本次只需读回来比。
	done bool
	solo analysis_vo.Decision
}

// soloRunID 是对照组一格的确定性 ID，与 backfill 的 bf_ 前缀分处两个命名空间。
//
// 分开是必须的：两者都落在 agent_runs 集合里，共用一个 ID 会让对照组
// 直接覆盖掉实验组的轨迹——而两份数据都还在的时候你根本不会发现，
// 最后拿到的是一份看起来正常、实际自己和自己比的数据集。
func soloRunID(code shared_vo.StockCode, d shared_vo.TradeDate) string {
	return fmt.Sprintf("solo_%s_%s", code.Symbol, d.Compact())
}

func runCompare(ctx context.Context) error {
	window, err := shared_vo.NewDateRange(compareFrom, compareTo)
	if err != nil {
		return err
	}
	maxTotal, err := parseUSD(compareMaxTotal, "--max-total-cost")
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
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := injectors.CreateBackfillDeps(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("装配依赖失败: %w", err)
	}
	defer func() { _ = deps.Conns.Close(context.WithoutCancel(ctx)) }()

	pairs, err := buildComparePairs(ctx, deps, window)
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		fmt.Printf("区间内没有 depth %d 的实验组轨迹。请先用 backfill 积累样本。\n", compareDepth)
		return nil
	}

	todo := 0
	for _, p := range pairs {
		if !p.done {
			todo++
		}
	}
	fmt.Printf("\n配对总数\t%d\n对照组待跑\t%d（其余已跑过，直接读回来比）\n", len(pairs), todo)
	if compareDryRun {
		fmt.Println("\n--dry-run：未执行任何分析。")
		return nil
	}

	pairs, spent := runSoloArm(ctx, deps, pairs, maxTotal)
	printComparison(pairs, spent)
	return nil
}

// buildComparePairs 扫出实验组轨迹，并配上对照组的现状。
func buildComparePairs(
	ctx context.Context,
	deps *providers.BackfillDeps,
	window shared_vo.DateRange,
) ([]comparePair, error) {
	summaries, truncated, err := deps.Runs.ListSummaries(ctx, window, 0)
	if err != nil {
		return nil, err
	}
	if truncated {
		fmt.Println("注意：区间内还有更多运行未纳入本次比较，用更窄的区间重跑可得完整样本。")
	}

	// 按前缀分流。实验组来自 backfill（bf_ 前缀），对照组是本命令自己产出的。
	// 不按前缀分的话，对照组的轨迹会被当成实验组再配一次对——自己和自己比，
	// 重合率必然 100%，而那个数字看起来完全正常。
	solo := make(map[string]analysis_vo.Decision, len(summaries))
	var crew []comparePair
	for _, s := range summaries {
		switch {
		case strings.HasPrefix(s.RunID, "solo_"):
			solo[s.RunID] = s.Decision
		case strings.HasPrefix(s.RunID, "bf_") &&
			strings.HasSuffix(s.RunID, fmt.Sprintf("_d%d", compareDepth)):
			crew = append(crew, comparePair{
				code:      s.Code,
				tradeDate: s.TradeDate,
				crew:      s.Decision,
				soloRunID: soloRunID(s.Code, s.TradeDate),
			})
		}
	}

	for i := range crew {
		if d, ok := solo[crew[i].soloRunID]; ok {
			crew[i].done = true
			crew[i].solo = d
		}
	}
	if compareLimit > 0 && len(crew) > compareLimit {
		crew = crew[:compareLimit]
	}
	return crew, nil
}

// runSoloArm 把还没跑过的对照组补齐。
func runSoloArm(
	ctx context.Context,
	deps *providers.BackfillDeps,
	pairs []comparePair,
	maxTotal decimal.Decimal,
) ([]comparePair, decimal.Decimal) {
	var spent decimal.Decimal
	for i := range pairs {
		if pairs[i].done {
			continue
		}
		if err := ctx.Err(); err != nil {
			fmt.Println("\n已中断。")
			break
		}
		if maxTotal.GreaterThan(decimal.Zero) && spent.GreaterThanOrEqual(maxTotal) {
			fmt.Printf("\n已达成本上限 $%s，停止。\n", maxTotal.StringFixed(2))
			break
		}

		req, err := analysis_vo.NewRequest(pairs[i].code, pairs[i].tradeDate,
			analysis_vo.DepthStandard, nil, "")
		if err != nil {
			fmt.Printf("构造请求失败 %s %s: %v\n",
				pairs[i].code.FullSymbol(), pairs[i].tradeDate.String(), err)
			continue
		}
		res, err := deps.Engine.RunSolo(ctx, pairs[i].soloRunID, req)
		if err != nil {
			fmt.Printf("对照组失败 %s %s: %v\n",
				pairs[i].code.FullSymbol(), pairs[i].tradeDate.String(), err)
			continue
		}
		pairs[i].done = true
		pairs[i].solo = res.Decision
		spent = spent.Add(res.Usage.CostUSD)
		fmt.Printf("%s %s  实验组=%s  对照组=%s  $%s\n",
			pairs[i].code.FullSymbol(), pairs[i].tradeDate.String(),
			pairs[i].crew.Action.DisplayName(), res.Decision.Action.DisplayName(),
			res.Usage.CostUSD.StringFixed(4))
	}
	return pairs, spent
}

// comparisonTally 是配对比较的汇总计数。
type comparisonTally struct {
	Compared   int
	SameAction int
	SameDir    int
	// BothNoDir 是两边都给了「持有/待定」的格子。
	//
	// 它必须单独报出来，不能埋在 SameDir 里：两边都说「看不清」确实是一种一致，
	// 但它与「两边都说买入」的含义天差地别。只报总一致率的话，
	// 一批双方都弃权的样本会把重合率推高，读起来却像是两种方法高度吻合。
	BothNoDir   int
	CrewDirOnly int
	SoloDirOnly int
}

// tallyPairs 汇总已完成的配对。
func tallyPairs(pairs []comparePair) comparisonTally {
	var t comparisonTally
	for _, p := range pairs {
		if !p.done {
			continue
		}
		t.Compared++
		if p.crew.Action == p.solo.Action {
			t.SameAction++
		}
		cd := agent_vo.DirectionOfAction(p.crew.Action)
		sd := agent_vo.DirectionOfAction(p.solo.Action)
		switch {
		case cd == sd && cd == agent_vo.DirectionNone:
			t.BothNoDir++
			t.SameDir++
		case cd == sd:
			t.SameDir++
		case cd == agent_vo.DirectionNone:
			t.SoloDirOnly++
		case sd == agent_vo.DirectionNone:
			t.CrewDirOnly++
		}
	}
	return t
}

func printComparison(pairs []comparePair, spent decimal.Decimal) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	t := tallyPairs(pairs)
	compared := t.Compared
	sameAction := t.SameAction
	sameDir := t.SameDir
	bothNoDir := t.BothNoDir
	crewDirOnly := t.CrewDirOnly
	soloDirOnly := t.SoloDirOnly

	fmt.Fprintf(w, "\n配对样本\t%d\n", compared)
	if compared == 0 {
		fmt.Fprintf(w, "没有可比对的样本。\n")
		return
	}

	actionRate := ratioOf(sameAction, compared)
	dirRate := ratioOf(sameDir, compared)
	actionCI := agent_vo.WilsonInterval(sameAction, compared)
	dirCI := agent_vo.WilsonInterval(sameDir, compared)

	fmt.Fprintf(w, "动作完全一致\t%d\t%s\t95%% CI [%s, %s]\n",
		sameAction, decimalx.FormatRatio(actionRate),
		decimalx.FormatRatio(actionCI.Lower), decimalx.FormatRatio(actionCI.Upper))
	fmt.Fprintf(w, "方向一致\t%d\t%s\t95%% CI [%s, %s]\n",
		sameDir, decimalx.FormatRatio(dirRate),
		decimalx.FormatRatio(dirCI.Lower), decimalx.FormatRatio(dirCI.Upper))
	fmt.Fprintf(w, "  其中两边都未给方向\t%d\n", bothNoDir)
	fmt.Fprintf(w, "仅实验组给了方向\t%d\n", crewDirOnly)
	fmt.Fprintf(w, "仅对照组给了方向\t%d\n", soloDirOnly)
	fmt.Fprintf(w, "\n对照组本次花费\t$%s\n", spent.StringFixed(4))

	fmt.Fprintf(w, "\n怎么读这个数字：\n")
	fmt.Fprintf(w, "  方向一致率高，说明十四人编排与一次直答在大多数标的上得出同样的结论，\n")
	fmt.Fprintf(w, "  编排的边际价值上界就在那些不一致的样本里——差异只可能存在于它们之中。\n")
	fmt.Fprintf(w, "  一致率低则说明两者确实在做不同的事，此时才值得继续追问「谁对」。\n")
	fmt.Fprintf(w, "  注意：本实验不回答谁更准，它是配对设计，市场噪声被当作共同项消掉了。\n")
	fmt.Fprintf(w, "\n仅供研究，不构成投资建议；过往表现不代表未来收益。\n\n")
}

func ratioOf(hits, total int) decimal.Decimal {
	if total == 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(hits)).DivRound(decimal.NewFromInt(int64(total)), 4)
}
