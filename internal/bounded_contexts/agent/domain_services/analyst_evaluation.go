package domain_services

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

// AnalystCase is a portable, frozen evaluation input. Synthetic cases must be labelled.
type AnalystCase struct {
	ID        string               `json:"id"`
	Code      string               `json:"code"`
	Market    shared.Market        `json:"market"`
	TradeDate shared.TradeDate     `json:"trade_date"`
	Analyst   vo.AgentKind         `json:"analyst"`
	Model     string               `json:"model,omitempty"`
	Synthetic bool                 `json:"synthetic"`
	Snapshot  entities.MarketBrief `json:"snapshot"`
}

func (c AnalystCase) Turn() (entities.Turn, error) {
	code, err := shared.NewStockCode(c.Code, c.Market)
	if err != nil {
		return entities.Turn{}, err
	}
	if c.ID == "" || c.TradeDate.IsZero() || !c.Analyst.IsAnalyst() {
		return entities.Turn{}, fmt.Errorf("case requires id, explicit date and valid analyst")
	}
	return entities.Turn{Contract: entities.NewCrew().Member(c.Analyst).Contract(), Snapshot: entities.ContextSnapshot{Code: code, TradeDate: c.TradeDate, Model: c.Model, Market: c.Snapshot}}, nil
}

type AnalystSample struct {
	CaseID        string               `json:"case_id"`
	Analyst       vo.AgentKind         `json:"analyst"`
	Repeat        int                  `json:"repeat"`
	Synthetic     bool                 `json:"synthetic"`
	DurationMS    int64                `json:"duration_ms"`
	Agent         entities.TurnResult  `json:"agent"`
	Baseline      *entities.TurnResult `json:"baseline,omitempty"`
	Error         string               `json:"error,omitempty"`
	BaselineError string               `json:"baseline_error,omitempty"`
}
type AnalystEvaluation struct {
	Complete         int             `json:"complete"`
	Partial          int             `json:"partial"`
	Insufficient     int             `json:"insufficient"`
	BaselineFailed   int             `json:"baseline_failed"`
	Version          string          `json:"version"`
	Samples          []AnalystSample `json:"samples"`
	Passed           int             `json:"passed"`
	Failed           int             `json:"failed"`
	TotalTokens      int             `json:"total_tokens"`
	CostUSD          decimal.Decimal `json:"cost_usd"`
	UnknownCostCalls int             `json:"unknown_cost_calls"`
	P50MS            int64           `json:"p50_ms"`
	P95MS            int64           `json:"p95_ms"`
}

// EvaluateAnalysts has bounded fan-out (2 cases) and a parent deadline. Individual
// failures remain samples; cancellation returns partial results and an error.
func (s *RuntimeService) EvaluateAnalysts(ctx context.Context, cases []AnalystCase, repeats int, compare bool) (AnalystEvaluation, error) {
	out := AnalystEvaluation{Version: vo.MarketAgentVersion}
	if len(cases) == 0 || len(cases) > 100 || repeats < 1 || repeats > 10 {
		return out, fmt.Errorf("cases must be 1..100; repeats 1..10")
	}
	type job struct {
		c      AnalystCase
		turn   entities.Turn
		repeat int
	}
	var jobs []job
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ID] {
			return out, fmt.Errorf("duplicate case id: %s", c.ID)
		}
		seen[c.ID] = true
		turn, err := c.Turn()
		if err != nil {
			return out, fmt.Errorf("case %s: %w", c.ID, err)
		}
		for r := 1; r <= repeats; r++ {
			jobs = append(jobs, job{c, turn, r})
		}
	}
	results, runErr := concurrency.Settle(ctx, jobs, 2, func(ctx context.Context, j job) (AnalystSample, error) {
		start := time.Now()
		sample := AnalystSample{CaseID: j.c.ID, Analyst: j.c.Analyst, Repeat: j.repeat, Synthetic: j.c.Synthetic}
		if compare {
			pair, err := s.CompareAnalyst(ctx, j.turn)
			sample.Agent = pair.Agent
			sample.Baseline = &pair.Baseline
			sample.Error = pair.AgentError
			sample.BaselineError = pair.BaselineError
			if err != nil {
				sample.Error = err.Error()
			}
		} else {
			result, err := s.executeMarketAgent(ctx, j.turn)
			sample.Agent = result
			if err != nil {
				sample.Error = err.Error()
			}
		}
		sample.DurationMS = time.Since(start).Milliseconds()
		return sample, nil
	})
	var durations []int64
	for i, r := range results {
		sample := r.Value
		if sample.CaseID == "" {
			sample = AnalystSample{CaseID: jobs[i].c.ID, Analyst: jobs[i].c.Analyst, Repeat: jobs[i].repeat, Synthetic: jobs[i].c.Synthetic, Error: "cancelled before completion"}
		}
		out.Samples = append(out.Samples, sample)
		if sample.Error == "" && strings.TrimSpace(sample.Agent.Content) != "" {
			out.Passed++
			var artifact struct {
				Output string `json:"output"`
			}
			_ = json.Unmarshal([]byte(sample.Agent.AgentArtifact), &artifact)
			var r vo.AnalystReport
			_ = json.Unmarshal([]byte(artifact.Output), &r)
			switch r.Status {
			case "complete":
				out.Complete++
			case "partial":
				out.Partial++
			case "insufficient":
				out.Insufficient++
			}
		} else {
			out.Failed++
		}
		if sample.BaselineError != "" {
			out.BaselineFailed++
		}
		durations = append(durations, sample.DurationMS)
		arms := []entities.TurnResult{sample.Agent}
		if sample.Baseline != nil {
			arms = append(arms, *sample.Baseline)
		}
		for _, arm := range arms {
			out.TotalTokens += arm.Usage.TotalTokens
			out.CostUSD = out.CostUSD.Add(arm.Usage.CostUSD)
			for _, call := range arm.ModelCalls {
				if call.Usage.CostStatus == "" || strings.HasPrefix(call.Usage.CostStatus, "unknown") {
					out.UnknownCostCalls++
				}
			}
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if len(durations) > 0 {
		out.P50MS = durations[(len(durations)-1)/2]
		out.P95MS = durations[(len(durations)*95+99)/100-1]
	}
	return out, runErr
}
