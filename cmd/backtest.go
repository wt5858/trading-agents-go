package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	agent_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	agent_entities "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	agent_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	agent_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/db"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

var (
	backtestFrom    string
	backtestTo      string
	backtestHorizon int
	backtestLimit   int
)

// BacktestCmd 对历史分析做事后评分。
//
// 它不重跑分析、不调模型、不花 token：评的是系统当时真实给出过的建议，
// 对上那之后的实际走势。样本量因此等于历史上真的跑过多少次分析——
// 这个限制是诚实的，重跑一遍得到的是「今天的模型对历史数据怎么看」，
// 那既贵又答非所问。
var BacktestCmd = &cobra.Command{
	Use:   "backtest",
	Short: "对历史分析结论做方向一致率评测",
	Long: `对指定交易日区间内已经跑过的分析做事后评分：
把每次分析给出的方向性建议（买入/加仓 -> 看涨，卖出/减仓 -> 看跌）
对上其后 N 个自然日的实际涨跌，统计方向一致率。

持有与待定不参与评分（它们没有方向），涨跌幅在 ±1% 以内判为横盘。
结果落库到 agent_evaluations，可重复执行。

本工具仅用于学习与研究。一致率不构成任何投资建议，
过往表现不代表未来收益。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return withBacktestInfra(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			window, err := shared_vo.NewDateRange(backtestFrom, backtestTo)
			if err != nil {
				return err
			}

			svc := agent_services.NewEvaluationService(
				agent_repo.NewAnalysisRunRepository(c.Mongo),
				agent_repo.NewEvaluationRepository(c.Mongo),
				stock_repo.NewMarketDataRepository(c.Mongo),
				log,
			)

			eval, err := svc.Evaluate(ctx, agent_services.EvaluateInput{
				Window:      window,
				HorizonDays: backtestHorizon,
				Limit:       backtestLimit,
			})
			if err != nil {
				return err
			}

			printEvaluation(eval.ID, window, eval.HorizonDays, eval.Stats())
			return nil
		})
	},
}

func init() {
	BacktestCmd.Flags().StringVar(&backtestFrom, "from", "", "起始交易日 YYYY-MM-DD，留空取近半年")
	BacktestCmd.Flags().StringVar(&backtestTo, "to", "", "结束交易日 YYYY-MM-DD，留空取今天")
	BacktestCmd.Flags().IntVar(&backtestHorizon, "horizon", 0,
		fmt.Sprintf("前瞻窗口自然日数，默认 %d", agent_entities.EvaluationHorizonDays))
	BacktestCmd.Flags().IntVar(&backtestLimit, "limit", 0, "最多评估多少次运行，默认 1000")
	RootCmd.AddCommand(BacktestCmd)
}

// withBacktestInfra 只连 Mongo 就把活干完。
//
// 与 withMySQL 同一个理由：回测只读 Mongo 里的运行轨迹与行情，
// 走完整装配会平白挂上 MySQL、Redis 和消息队列三个可能失败的依赖，
// 而「回测跑不了」的原因是「RabbitMQ 没起来」毫无道理。
func withBacktestInfra(fn func(ctx context.Context, conns *db.Connections, log *zap.Logger) error) error {
	cfg, log, err := bootstrap()
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	ctx := context.Background()
	conns, err := db.Open(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer func() { _ = conns.Close(ctx) }()

	if conns.Mongo == nil {
		return fmt.Errorf("未启用 MongoDB，回测无法执行")
	}
	return fn(ctx, conns, log)
}

// printEvaluation 把结论打到标准输出。
//
// 样本量与跳过明细和一致率一起打，不是可选项：
// 「一致率 70%」与「一致率 70%，但三分之二样本因缺行情被跳过」
// 是两个完全不同的结论，只报前者就是在骗自己。
func printEvaluation(id string, window shared_vo.DateRange, horizon int, st agent_vo.EvaluationStats) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	fmt.Fprintf(w, "\n评估 ID\t%s\n", id)
	fmt.Fprintf(w, "区间\t%s ~ %s（前瞻 %d 个自然日）\n",
		window.Start.String(), window.End.String(), horizon)
	fmt.Fprintf(w, "扫到运行\t%d\n", st.Total)
	fmt.Fprintf(w, "参与评分\t%d\n", st.Scored)
	fmt.Fprintf(w, "跳过\t%d\n", st.Skipped)

	for _, reason := range []agent_vo.SkipReason{
		agent_vo.SkipNoDirection, agent_vo.SkipNoBasePrice,
		agent_vo.SkipNoForwardPrice, agent_vo.SkipFetchFailed,
	} {
		if n := st.SkipCounts[reason]; n > 0 {
			fmt.Fprintf(w, "  └ %s\t%d\n", reason.DisplayName(), n)
		}
	}
	if st.Truncated {
		fmt.Fprintf(w, "\n注意\t区间内还有更多运行未纳入本次评估，下面的数字基于最早的 %d 条。\n", st.Total)
		fmt.Fprintf(w, "\t用 --limit 调大上限重跑可得到完整样本。\n")
	}

	if st.Scored == 0 {
		fmt.Fprintf(w, "\n没有样本参与评分，一致率无从谈起。\n")
		return
	}

	fmt.Fprintf(w, "命中\t%d\n", st.Hits)
	fmt.Fprintf(w, "方向一致率\t%s\n", decimalx.FormatRatio(st.HitRate))

	fmt.Fprintf(w, "\n按建议动作拆分\n")
	fmt.Fprintf(w, "动作\t样本\t命中\t一致率\n")
	for _, a := range st.ByAction {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n",
			a.Action.DisplayName(), a.Scored, a.Hits, decimalx.FormatRatio(a.HitRate))
	}
	fmt.Fprintf(w, "\n仅供研究，不构成投资建议；过往表现不代表未来收益。\n\n")
}
