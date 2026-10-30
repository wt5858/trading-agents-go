package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	agentvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysisvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	stockvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/di/injectors"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

func newAnalystRunCommand() *cobra.Command {
	var code, market, date, analyst, model string
	cmd := &cobra.Command{Use: "analyst-run", Short: "从本地数据库准备数据，独立运行并保存一位分析师轨迹", RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := shared.NewStockCode(code, shared.Market(market))
		if err != nil {
			return err
		}
		d, err := shared.NewTradeDate(date)
		if err != nil || d.IsZero() {
			return fmt.Errorf("明确的--date必填")
		}
		kind := agentvo.AgentKind(analyst)
		if !kind.IsAnalyst() {
			return fmt.Errorf("无效分析师")
		}
		req, err := analysisvo.NewRequest(c, d, analysisvo.DepthStandard, []string{analyst}, model)
		if err != nil {
			return err
		}
		cfg, log, err := bootstrap()
		if err != nil {
			return err
		}
		defer func() { _ = log.Sync() }()
		deps, err := injectors.CreateBackfillDeps(cmd.Context(), cfg, log)
		if err != nil {
			return err
		}
		defer func() { _ = deps.Conns.Close(context.WithoutCancel(cmd.Context())) }()
		runID := "analyst_" + idx.New()
		record, runErr := deps.Engine.RunAnalyst(cmd.Context(), runID, req, kind)
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			RunID  string             `json:"run_id"`
			Record agentvo.TurnRecord `json:"record"`
		}{runID, record}); err != nil {
			return err
		}
		return runErr
	}}
	f := cmd.Flags()
	f.StringVar(&code, "code", "", "标的代码")
	f.StringVar(&market, "market", "", "CN/HK/US，留空推断")
	f.StringVar(&date, "date", "", "分析日期YYYY-MM-DD")
	f.StringVar(&analyst, "analyst", "market", "market/fundamentals/news/sentiment/sector/index")
	f.StringVar(&model, "model", "", "模型，留空默认")
	_ = cmd.MarkFlagRequired("code")
	_ = cmd.MarkFlagRequired("date")
	return cmd
}
func newContextImportCommand() *cobra.Command {
	var input string
	var dry bool
	cmd := &cobra.Command{Use: "market-context-import", Short: "批量导入有来源及可获得日期的板块/指数证据", RunE: func(cmd *cobra.Command, _ []string) error {
		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer f.Close()
		var items []stockvo.MarketContext
		if err := json.NewDecoder(io.LimitReader(f, 10<<20)).Decode(&items); err != nil {
			return err
		}
		if len(items) == 0 || len(items) > 1000 {
			return fmt.Errorf("导入条数必须为1..1000")
		}
		for _, item := range items {
			if err := item.Validate(); err != nil {
				return err
			}
		}
		if dry {
			fmt.Fprintf(cmd.OutOrStdout(), "validated=%d 未写入数据库\n", len(items))
			return nil
		}
		cfg, log, err := bootstrap()
		if err != nil {
			return err
		}
		defer func() { _ = log.Sync() }()
		conns, err := providers.NewSingletonConnections(cmd.Context(), cfg, log)
		if err != nil {
			return err
		}
		defer func() { _ = conns.Close(context.WithoutCancel(cmd.Context())) }()
		if err := repositories.NewMarketDataRepository(conns.Mongo).SaveMarketContexts(cmd.Context(), items); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "imported=%d\n", len(items))
		return nil
	}}
	cmd.Flags().StringVar(&input, "input", "", "MarketContext数组JSON")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "仅校验")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}
func init() { RootCmd.AddCommand(newAnalystRunCommand(), newContextImportCommand()) }
