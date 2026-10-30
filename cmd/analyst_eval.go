package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
	"github.com/wt5858/trading-agents-go/config"
	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	"github.com/wt5858/trading-agents-go/internal/helpers/llm"
)

func newAnalystEvalCommand() *cobra.Command {
	var input, output, maxCost string
	var repeat, limit, maxRequests int
	var mock, dryRun, compare bool
	cmd := &cobra.Command{Use: "analyst-eval", Short: "在冻结JSON样本上独立运行或对照评测分析师Agent", RunE: func(cmd *cobra.Command, _ []string) error {
		file, err := os.Open(input)
		if err != nil {
			return err
		}
		defer file.Close()
		var cases []svc.AnalystCase
		if err := json.NewDecoder(io.LimitReader(file, 10<<20)).Decode(&cases); err != nil {
			return err
		}
		if limit > 0 && limit < len(cases) {
			cases = cases[:limit]
		}
		if limit < 0 || len(cases) == 0 || len(cases) > 100 || repeat < 1 || repeat > 10 || maxRequests < 1 || maxRequests > 1000 {
			return fmt.Errorf("invalid cases/repeat/request limit")
		}
		for _, c := range cases {
			if _, err := c.Turn(); err != nil {
				return fmt.Errorf("case %s: %w", c.ID, err)
			}
		}
		budget, err := decimal.NewFromString(maxCost)
		if err != nil || budget.IsNegative() {
			return fmt.Errorf("invalid --max-cost")
		}
		if dryRun {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "cases=%d repeats=%d compare=%t maximumRequests=%d mock=%t (未调用模型)\n", len(cases), repeat, compare, maxRequests, mock)
			return err
		}
		if output == "" {
			return fmt.Errorf("--output is required for recording usage and failures")
		}
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		router := llm.NewRouter("evaluation-mock")
		if mock {
			router.Register(&llm.CappedClient{Client: llm.EvaluationMock{}, Limit: maxRequests}, "evaluation-mock")
		} else {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			router = llm.NewRouter(cfg.LLM.DefaultModel)
			// Share one attempt cap across providers; clients use bounded HTTP timeouts.
			capped := &llm.CappedClient{Limit: maxRequests}

			for _, p := range cfg.LLM.EnabledProviders() {
				client := providers.NewLLMClient(p.Kind, p.Name, p.BaseURL, p.APIKey, &http.Client{Timeout: 60 * time.Second})
				router.Register(evalCapAdapter{client, capped}, p.Models...)
			}

			if _, _, err := router.Resolve(""); err != nil {
				return fmt.Errorf("no configured evaluation model: %w", err)
			}
		}
		runtime := svc.NewRuntimeService(router, nil, nil, nil, svc.RuntimeConfig{AnalystAgentsEnabled: true}, nil)
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		ctx = svc.WithCostBudget(ctx, budget)
		report, runErr := runtime.EvaluateAnalysts(ctx, cases, repeat, compare)
		if err := json.NewEncoder(out).Encode(report); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "passed=%d failed=%d tokens=%d estimatedCostUSD=%s unknownCostCalls=%d output=%s\n", report.Passed, report.Failed, report.TotalTokens, report.CostUSD.String(), report.UnknownCostCalls, output)
		if runErr != nil {
			return runErr
		}
		if report.Failed > 0 {
			return fmt.Errorf("%d agent cases failed; partial results saved", report.Failed)
		}
		return nil
	}}
	f := cmd.Flags()
	f.StringVar(&maxCost, "max-cost", "0", "整批美元估算预算，0不限制；未知价格在启用预算时拒绝调用")
	f.StringVar(&input, "input", "", "冻结样本JSON文件")
	f.StringVar(&output, "output", "", "保存报告的新JSON文件，不覆盖已有文件")
	f.IntVar(&repeat, "repeat", 1, "重复次数1..10")
	f.IntVar(&limit, "limit", 0, "最多执行前N个样本")
	f.IntVar(&maxRequests, "max-requests", 48, "整批模型请求次数上限")
	f.BoolVar(&mock, "mock", false, "仅验证链路，不调用真实模型")
	f.BoolVar(&dryRun, "dry-run", false, "校验样本，不调用模型")
	f.BoolVar(&compare, "compare", false, "配对比较内联数据基线与工具Agent")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}

// Adapters share the attempt cap but preserve each client's provider identity.
type evalCapAdapter struct {
	svc.LLMClient
	cap *llm.CappedClient
}

func (a evalCapAdapter) Complete(ctx context.Context, r vo.CompletionRequest) (*vo.CompletionResult, error) {
	if err := a.cap.Claim(); err != nil {
		return nil, err
	}
	return a.LLMClient.Complete(ctx, r)
}
func init() { RootCmd.AddCommand(newAnalystEvalCommand()) }

func (a evalCapAdapter) EstimateCost(r vo.CompletionRequest) (decimal.Decimal, bool) {
	if e, ok := a.LLMClient.(interface {
		EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool)
	}); ok {
		return e.EstimateCost(r)
	}
	return decimal.Zero, false
}
