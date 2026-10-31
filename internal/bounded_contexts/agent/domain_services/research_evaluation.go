package domain_services

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysisvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

type ResearchCase struct {
	ID        string                  `json:"id"`
	Question  string                  `json:"question"`
	Code      string                  `json:"code"`
	Market    shared.Market           `json:"market"`
	TradeDate shared.TradeDate        `json:"trade_date"`
	Model     string                  `json:"model,omitempty"`
	Synthetic bool                    `json:"synthetic"`
	Snapshot  entities.MarketBrief    `json:"snapshot"`
	Expected  *vo.ResearchExpectation `json:"expected,omitempty"`
}

func (c ResearchCase) Request() (analysisvo.Request, error) {
	if c.ID == "" {
		return analysisvo.Request{}, fmt.Errorf("research case id required")
	}
	if err := vo.ValidateResearchQuestion(c.Question); err != nil {
		return analysisvo.Request{}, err
	}
	if c.TradeDate.IsZero() {
		return analysisvo.Request{}, fmt.Errorf("explicit research date required")
	}
	if err := c.Expected.Validate(); err != nil {
		return analysisvo.Request{}, err
	}
	code, err := shared.NewStockCode(c.Code, c.Market)
	if err != nil {
		return analysisvo.Request{}, err
	}
	return analysisvo.NewRequest(code, c.TradeDate, analysisvo.DepthStandard, nil, c.Model)
}

type ResearchSample struct {
	CaseID         string      `json:"case_id"`
	Synthetic      bool        `json:"synthetic"`
	Trace          vo.RunTrace `json:"trace"`
	ContractPassed bool        `json:"contract_passed"`
	Labelled       bool        `json:"labelled"`
	QualityPassed  bool        `json:"quality_passed"`
	Failures       []string    `json:"failures,omitempty"`
	Error          string      `json:"error,omitempty"`
}
type ResearchEvaluation struct {
	Skipped          int              `json:"skipped"`
	Version          string           `json:"version"`
	Samples          []ResearchSample `json:"samples"`
	Passed           int              `json:"passed"`
	Failed           int              `json:"failed"`
	Labelled         int              `json:"labelled"`
	QualityPassed    int              `json:"quality_passed"`
	TotalTokens      int              `json:"total_tokens"`
	CostUSD          decimal.Decimal  `json:"cost_usd"`
	UnknownCostCalls int              `json:"unknown_cost_calls"`
}

// EvaluateResearch reports protocol checks separately from literal labelled checks.
// Cases run sequentially so one evaluation never multiplies the research fan-out.
func (s *RuntimeService) EvaluateResearch(ctx context.Context, cases []ResearchCase, limits vo.ResearchLimits) (ResearchEvaluation, error) {
	out := ResearchEvaluation{Version: vo.ResearchVersion}
	if err := limits.Validate(); err != nil {
		return out, err
	}
	if len(cases) < 1 || len(cases) > 20 {
		return out, fmt.Errorf("research evaluation requires 1..20 cases")
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if _, err := c.Request(); err != nil {
			return out, err
		}
		if seen[c.ID] {
			return out, fmt.Errorf("duplicate research case: %s", c.ID)
		}
		seen[c.ID] = true
	}
	for _, c := range cases {
		req, _ := c.Request()
		root := entities.NewAnalysisContext("research_eval_"+c.ID, req)
		root.LoadMarketBrief(c.Snapshot)
		err := s.Research(ctx, root, c.Question, limits, nil)
		reason := ""
		if err != nil {
			reason = err.Error()
		}
		trace := root.TraceSnapshot(time.Now(), reason)
		sample := ResearchSample{CaseID: c.ID, Synthetic: c.Synthetic, Trace: trace, ContractPassed: err == nil, Labelled: c.Expected != nil}
		if err != nil {
			sample.Error = err.Error()
		}
		if trace.Research == nil || (trace.Research.Report == "" && trace.Research.Status != "skipped") {
			sample.ContractPassed = false
		}
		if sample.ContractPassed && trace.Research.Status == "skipped" {
			out.Skipped++
		} else if sample.ContractPassed {
			out.Passed++
		} else {
			out.Failed++
		}
		if sample.Labelled {
			out.Labelled++
			sample.Failures = c.Expected.Check(trace.Research)
			sample.QualityPassed = sample.ContractPassed && len(sample.Failures) == 0
			if sample.QualityPassed {
				out.QualityPassed++
			}
		}
		out.TotalTokens += trace.Usage.TotalTokens
		out.CostUSD = out.CostUSD.Add(trace.Usage.CostUSD)
		for _, turn := range trace.Turns {
			for _, call := range turn.ModelCalls {
				if call.Usage.CostStatus != "estimated" && call.Usage.CostStatus != "mock" {
					out.UnknownCostCalls++
				}
			}
		}
		out.Samples = append(out.Samples, sample)
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, nil
}
