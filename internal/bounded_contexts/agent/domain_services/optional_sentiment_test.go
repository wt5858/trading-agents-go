package domain_services

import (
	"context"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

func TestOptionalSentimentRuntimeAndPairedEvaluationDoNotCallModel(t *testing.T) {
	turn := testTurn(t, entities.NewSentimentAnalyst())
	runtime := NewRuntimeService(nil, nil, nil, nil, RuntimeConfig{}, nil)
	result, err := runtime.Execute(context.Background(), turn)
	if err != nil || !result.Skipped || result.Content != "" || result.Usage.Calls != 0 {
		t.Fatalf("no-data runtime did not skip: %+v %v", result, err)
	}
	pair, err := runtime.CompareAnalyst(context.Background(), turn)
	if err != nil || !pair.Agent.Skipped || !pair.Baseline.Skipped {
		t.Fatalf("paired baseline wasted model calls: %+v %v", pair, err)
	}
	c := AnalystCase{ID: "optional", Code: turn.Snapshot.Code.Symbol, Market: turn.Snapshot.Code.Market, TradeDate: turn.Snapshot.TradeDate, Analyst: vo.KindSentimentAnalyst, Synthetic: true, Expected: &vo.AnalystExpectation{Status: "skipped"}}
	report, err := runtime.EvaluateAnalysts(context.Background(), []AnalystCase{c}, 1, true)
	if err != nil || report.Skipped != 1 || report.Passed != 0 || report.Failed != 0 || report.QualityPassed != 1 || report.TotalTokens != 0 {
		t.Fatalf("skip counted as success/failure or charged: %+v %v", report, err)
	}
}
func TestResearchOmitsOptionalSentimentAndDoesNotScheduleSupplement(t *testing.T) {
	root := researchRoot(t)
	plan := researchPlan(vo.KindSentimentAnalyst, vo.KindMarketAnalyst)
	plan.Questions[0].Question = "可选社交任务应从报告中移除"
	client := &researchClient{plan: plan, gap: true}
	runtime := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	if err := runtime.Research(context.Background(), root, "研究价格", vo.DefaultResearchLimits(), nil); err != nil {
		t.Fatal(err)
	}
	report := root.Research()
	if report.Status != "complete" || !report.QuestionSkipped("q1") || len(report.Attempts) != 2 || len(report.Review.Gaps) != 0 || strings.Contains(report.Report, "可选社交任务应从报告中移除") || client.calls.Load() != 5 {
		t.Fatalf("optional sentiment became research gap: %+v calls=%d", report, client.calls.Load())
	}
}
func TestSentimentOnlyResearchSkipsReviewAndReport(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindSentimentAnalyst)}
	runtime := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	if err := runtime.Research(context.Background(), root, "研究社交", vo.DefaultResearchLimits(), nil); err != nil {
		t.Fatal(err)
	}
	if root.Research().Status != "skipped" || root.Research().Report != "" || client.calls.Load() != 1 || client.reviews.Load() != 0 {
		t.Fatalf("empty sentiment generated research report: %+v", root.Research())
	}
}

func TestSentimentOnlyEngineRunReturnsNoReportAndNoModelCalls(t *testing.T) {
	turn := testTurn(t, entities.NewSentimentAnalyst())
	req, _ := analysis.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysis.DepthQuick, []string{"sentiment"}, "")
	runtime := NewRuntimeService(nil, nil, nil, nil, RuntimeConfig{}, nil)
	engine := NewEngineService(runtime, newAsOfSpy(stock.Quote{}), nil, nil, nil, nil, nil, nil, EngineConfig{})
	result, err := engine.Run(context.Background(), "optional-only", req, nil)
	if err != nil || result == nil || len(result.Reports) != 0 || result.Usage.Calls != 0 {
		t.Fatalf("optional-only analysis became data failure/report: %+v %v", result, err)
	}
	var analystSkipped bool
	for _, phase := range result.Phases {
		if phase.Phase == vo.PhaseAnalyst.String() {
			analystSkipped = phase.Skipped && len(phase.SkippedAgents) == 1
		}
	}
	if !analystSkipped {
		t.Fatalf("skip missing from phase result: %+v", result.Phases)
	}
}

func TestMissingSocialIsNotAddedToGlobalMissingWarnings(t *testing.T) {
	root := researchRoot(t)
	snapshot := root.Snapshot()
	req, _ := analysis.NewRequest(snapshot.Code, snapshot.TradeDate, analysis.DepthQuick, nil, "")
	brief := snapshot.Market
	brief.Missing = []string{"社交舆情"}
	engine := NewEngineService(nil, nil, nil, nil, nil, nil, nil, nil, EngineConfig{})
	if err := engine.prepareMissingData(context.Background(), req, &brief); err != nil {
		t.Fatal(err)
	}
	for _, name := range brief.Missing {
		if name == "社交舆情" {
			t.Fatal("optional social absence became warning to all analysts")
		}
	}
}
