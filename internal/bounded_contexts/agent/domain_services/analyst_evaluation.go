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
	Expected  *vo.AnalystExpectation `json:"expected,omitempty"`
	ID        string                 `json:"id"`
	Code      string                 `json:"code"`
	Market    shared.Market          `json:"market"`
	TradeDate shared.TradeDate       `json:"trade_date"`
	Analyst   vo.AgentKind           `json:"analyst"`
	Model     string                 `json:"model,omitempty"`
	Synthetic bool                   `json:"synthetic"`
	Snapshot  entities.MarketBrief   `json:"snapshot"`
}

func (c AnalystCase) Turn() (entities.Turn, error) {
	if err := c.Expected.Validate(); err != nil {
		return entities.Turn{}, err
	}
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
	AgentScore         vo.AnalystScore      `json:"agent_score"`
	BaselineScore      vo.AnalystScore      `json:"baseline_score"`
	AgentDurationMS    int64                `json:"agent_duration_ms"`
	BaselineDurationMS int64                `json:"baseline_duration_ms"`
	ArmBudgetUSD       decimal.Decimal      `json:"arm_budget_usd"`
	CaseID             string               `json:"case_id"`
	Analyst            vo.AgentKind         `json:"analyst"`
	Repeat             int                  `json:"repeat"`
	Synthetic          bool                 `json:"synthetic"`
	DurationMS         int64                `json:"duration_ms"`
	Agent              entities.TurnResult  `json:"agent"`
	Baseline           *entities.TurnResult `json:"baseline,omitempty"`
	Error              string               `json:"error,omitempty"`
	BaselineError      string               `json:"baseline_error,omitempty"`
}
type AnalystEvaluation struct {
	Skipped               int             `json:"skipped"`
	Labelled              int             `json:"labelled"`
	QualityPassed         int             `json:"quality_passed"`
	BaselineQualityPassed int             `json:"baseline_quality_passed"`
	AgentP50MS            int64           `json:"agent_p50_ms"`
	AgentP95MS            int64           `json:"agent_p95_ms"`
	BaselineP50MS         int64           `json:"baseline_p50_ms"`
	BaselineP95MS         int64           `json:"baseline_p95_ms"`
	Complete              int             `json:"complete"`
	Partial               int             `json:"partial"`
	Insufficient          int             `json:"insufficient"`
	BaselineFailed        int             `json:"baseline_failed"`
	Version               string          `json:"version"`
	Samples               []AnalystSample `json:"samples"`
	Passed                int             `json:"passed"`
	Failed                int             `json:"failed"`
	TotalTokens           int             `json:"total_tokens"`
	CostUSD               decimal.Decimal `json:"cost_usd"`
	UnknownCostCalls      int             `json:"unknown_cost_calls"`
	P50MS                 int64           `json:"p50_ms"`
	P95MS                 int64           `json:"p95_ms"`
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
	if compare {
		if budget, ok := ctx.Value(costBudgetKey{}).(*entities.CostBudget); ok {
			allowance := budget.Limit().DivRound(decimal.NewFromInt(int64(2*len(jobs))), 8)
			if !allowance.IsPositive() {
				return out, fmt.Errorf("budget too small for paired allocation")
			}
			ctx = context.WithValue(ctx, pairBudgetKey{}, allowance)
		}
	}
	results, runErr := concurrency.Settle(ctx, jobs, 2, func(ctx context.Context, j job) (AnalystSample, error) {
		start := time.Now()
		sample := AnalystSample{CaseID: j.c.ID, Analyst: j.c.Analyst, Repeat: j.repeat, Synthetic: j.c.Synthetic}
		if compare {
			pair, err := s.CompareAnalyst(ctx, j.turn)
			sample.AgentDurationMS = pair.AgentDurationMS
			sample.BaselineDurationMS = pair.BaselineDurationMS
			sample.ArmBudgetUSD = pair.ArmBudgetUSD
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
			sample.AgentDurationMS = time.Since(start).Milliseconds()
			if err != nil {
				sample.Error = err.Error()
			}
		}
		sample.AgentScore = j.c.Expected.Score(sample.Agent.AnalystReport, sample.Error)
		if sample.Agent.Skipped {
			sample.AgentScore = j.c.Expected.ScoreSkipped(sample.Error)
		}
		if sample.Baseline != nil {
			sample.BaselineScore = j.c.Expected.Score(sample.Baseline.AnalystReport, sample.BaselineError)
			if sample.Baseline.Skipped {
				sample.BaselineScore = j.c.Expected.ScoreSkipped(sample.BaselineError)
			}
		}
		sample.DurationMS = time.Since(start).Milliseconds()
		return sample, nil
	})
	if len(results) == 0 && runErr != nil {
		results = make([]concurrency.Outcome[AnalystSample], len(jobs))
		for i := range results {
			results[i].Err = runErr
		}
	}
	var durations, agentDurations, baselineDurations []int64
	for i, r := range results {
		sample := r.Value
		if sample.CaseID == "" {
			sample = AnalystSample{CaseID: jobs[i].c.ID, Analyst: jobs[i].c.Analyst, Repeat: jobs[i].repeat, Synthetic: jobs[i].c.Synthetic, Error: "cancelled before completion"}
			if r.Err != nil {
				sample.Error = r.Err.Error()
			}
			sample.AgentScore = jobs[i].c.Expected.Score(nil, sample.Error)
		}
		out.Samples = append(out.Samples, sample)
		if sample.AgentScore.Labelled {
			out.Labelled++
			if sample.AgentScore.Passed {
				out.QualityPassed++
			}
			if sample.BaselineScore.Passed {
				out.BaselineQualityPassed++
			}
		}
		if len(sample.Agent.ModelCalls) > 0 {
			agentDurations = append(agentDurations, sample.AgentDurationMS)
		}
		if sample.Baseline != nil && len(sample.Baseline.ModelCalls) > 0 {
			baselineDurations = append(baselineDurations, sample.BaselineDurationMS)
		}
		if sample.Error == "" && sample.Agent.Skipped {
			out.Skipped++
		} else if sample.Error == "" && strings.TrimSpace(sample.Agent.Content) != "" {
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
	out.AgentP50MS, out.AgentP95MS = latencyPercentiles(agentDurations)
	out.BaselineP50MS, out.BaselineP95MS = latencyPercentiles(baselineDurations)
	return out, runErr
}

func latencyPercentiles(values []int64) (int64, int64) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[(len(values)-1)/2], values[(len(values)*95+99)/100-1]
}
