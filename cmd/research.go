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
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/di/injectors"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/llm"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

func newResearchCommand(evaluate bool) *cobra.Command {
	var input, output, question, code, market, date, model, maxCost string
	var mock, dry bool
	limits := vo.DefaultResearchLimits()
	use, short := "research-run", "运行有界Research，保存问题、审查、证据和成本轨迹"
	if evaluate {
		use, short = "research-eval", "在冻结样本上评测Research；契约检查与标注匹配分别统计"
	}
	cmd := &cobra.Command{Use: use, Short: short, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := limits.Validate(); err != nil {
			return err
		}
		budget, err := decimal.NewFromString(maxCost)
		if err != nil || budget.IsNegative() || (!mock && !budget.IsPositive()) {
			return fmt.Errorf("real research requires a positive --max-cost")
		}
		var cases []svc.ResearchCase
		if input != "" {
			if err := readResearchInput(input, evaluate, &cases); err != nil {
				return err
			}
		} else {
			if evaluate || mock {
				return fmt.Errorf("--input required for evaluation/mock")
			}
			d, err := shared.NewTradeDate(date)
			if err != nil {
				return err
			}
			cases = []svc.ResearchCase{{ID: "live", Code: code, Market: shared.Market(market), TradeDate: d, Question: question, Model: model}}
		}
		if len(cases) < 1 || len(cases) > 20 {
			return fmt.Errorf("requires 1..20 research cases")
		}
		seen := map[string]bool{}
		for i := range cases {
			if model != "" {
				cases[i].Model = model
			}
			if !evaluate && question != "" {
				cases[i].Question = question
			}
			if _, err := cases[i].Request(); err != nil {
				return fmt.Errorf("case %s: %w", cases[i].ID, err)
			}
			if seen[cases[i].ID] {
				return fmt.Errorf("duplicate research case id")
			}
			seen[cases[i].ID] = true
		}
		if dry {
			fmt.Fprintf(cmd.OutOrStdout(), "validated=%d mock=%t requestsPerRun=%d supplements=%d (未调用模型)\n", len(cases), mock, limits.MaxRequests, limits.SupplementRounds)
			return nil
		}
		if output == "" {
			return fmt.Errorf("--output required for preserving results and usage")
		}
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		ctx := svc.WithCostBudget(cmd.Context(), budget)
		if input == "" {
			cfg, log, err := bootstrap()
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()
			deps, err := injectors.CreateBackfillDeps(ctx, cfg, log)
			if err != nil {
				return err
			}
			defer func() { _ = deps.Conns.Close(context.WithoutCancel(ctx)) }()
			req, _ := cases[0].Request()
			trace, runErr := deps.Engine.RunResearch(ctx, "research_"+idx.New(), req, cases[0].Question, limits)
			if err := json.NewEncoder(out).Encode(trace); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "runId=%s executionId=%s tokens=%d estimatedCostUSD=%s output=%s\n", trace.RunID, trace.ExecutionID, trace.Usage.TotalTokens, trace.Usage.CostUSD, output)
			return runErr
		}
		runtime, err := researchRuntime(mock, limits.MaxRequests*len(cases))
		if err != nil {
			return err
		}
		if evaluate {
			report, runErr := runtime.EvaluateResearch(ctx, cases, limits)
			if err := json.NewEncoder(out).Encode(report); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "contractPassed=%d failed=%d skipped=%d labelled=%d qualityPassed=%d tokens=%d estimatedCostUSD=%s output=%s\n", report.Passed, report.Failed, report.Skipped, report.Labelled, report.QualityPassed, report.TotalTokens, report.CostUSD, output)
			if runErr != nil {
				return runErr
			}
			if report.Failed > 0 || report.QualityPassed < report.Labelled {
				return fmt.Errorf("research checks failed; partial results saved")
			}
			return nil
		}
		req, _ := cases[0].Request()
		root := entities.NewAnalysisContext("research_"+idx.New(), req)
		root.LoadMarketBrief(cases[0].Snapshot)
		runErr := runtime.Research(ctx, root, cases[0].Question, limits, nil)
		reason := ""
		if runErr != nil {
			reason = runErr.Error()
		}
		trace := root.TraceSnapshot(time.Now(), reason)
		if err := json.NewEncoder(out).Encode(trace); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "status=%s tokens=%d estimatedCostUSD=%s output=%s\n", trace.Research.Status, trace.Usage.TotalTokens, trace.Usage.CostUSD, output)
		return runErr
	}}
	f := cmd.Flags()
	f.StringVar(&input, "input", "", "冻结ResearchCase JSON；eval模式为数组")
	f.StringVar(&output, "output", "", "保存结果的新JSON文件，不覆盖")
	f.StringVar(&maxCost, "max-cost", "0.20", "整次命令美元估算预算；真实调用必须大于0")
	f.IntVar(&limits.MaxRequests, "max-requests", limits.MaxRequests, "每份研究模型请求上限3..64（包含规划、审查与汇总）")
	f.IntVar(&limits.MaxTokens, "max-tokens", limits.MaxTokens, "每份研究总token预算，按保守请求体估算预留")
	f.IntVar(&limits.SupplementRounds, "supplement-rounds", limits.SupplementRounds, "最多补证轮数0..2")
	f.IntVar(&limits.TimeoutSeconds, "timeout-seconds", limits.TimeoutSeconds, "每份研究超时1..900秒")
	f.BoolVar(&mock, "mock", false, "仅验证链路，不调用真实模型")
	f.BoolVar(&dry, "dry-run", false, "校验输入，不调用模型")
	f.StringVar(&model, "model", "", "覆盖样本模型，留空走配置")
	if !evaluate {
		f.StringVar(&question, "question", "", "研究问题，可覆盖冻结样本")
		f.StringVar(&code, "code", "", "从本地数据库研究的标的")
		f.StringVar(&market, "market", "", "CN/HK/US，留空推断")
		f.StringVar(&date, "date", "", "明确的研究日期YYYY-MM-DD")
		cmd.MarkFlagsMutuallyExclusive("input", "code")
		cmd.MarkFlagsMutuallyExclusive("input", "market")
		cmd.MarkFlagsMutuallyExclusive("input", "date")
	}
	return cmd
}
func readResearchInput(path string, array bool, out *[]svc.ResearchCase) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 10<<20 {
		return fmt.Errorf("research input exceeds 10MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, 10<<20))
	d.DisallowUnknownFields()
	if array {
		err = d.Decode(out)
	} else {
		var c svc.ResearchCase
		err = d.Decode(&c)
		*out = []svc.ResearchCase{c}
	}
	if err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("one research JSON value required")
	}
	return nil
}
func researchRuntime(mock bool, cap int) (*svc.RuntimeService, error) {
	router := llm.NewRouter("research-mock")
	if mock {
		router.Register(&llm.CappedClient{Client: llm.ResearchMock{}, Limit: cap}, "research-mock")
	} else {
		cfg, err := config.Load(configFile)
		if err != nil {
			return nil, err
		}
		router = llm.NewRouter(cfg.LLM.DefaultModel)
		capped := &llm.CappedClient{Limit: cap}
		for _, p := range cfg.LLM.EnabledProviders() {
			client := providers.NewLLMClient(p.Kind, p.Name, p.BaseURL, p.APIKey, &http.Client{Timeout: 60 * time.Second})
			router.Register(evalCapAdapter{client, capped}, p.Models...)
		}
		if _, _, err := router.Resolve(""); err != nil {
			return nil, err
		}
	}
	return svc.NewRuntimeService(router, nil, nil, nil, svc.RuntimeConfig{AnalystAgentsEnabled: true}, nil), nil
}
func init() { RootCmd.AddCommand(newResearchCommand(false), newResearchCommand(true)) }
