package entities

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

type optionalRuntime struct{ calls atomic.Int32 }

func (r *optionalRuntime) Execute(context.Context, Turn) (TurnResult, error) {
	r.calls.Add(1)
	return TurnResult{Content: "有效报告", Usage: vo.Usage{Calls: 1}, AnalystReport: &vo.AnalystReport{Status: "complete", SupportVerified: true, Findings: []vo.AnalystFinding{{Kind: "fact", Claim: "已有事实"}}}}, nil
}
func TestSentimentWithoutValidSocialSkipsWithoutReportOrFailure(t *testing.T) {
	root := newTestContext(t, analysis.DepthQuick)
	_, end := stock.MarketDayBounds(root.Code().Market, root.TradeDate())
	other, _ := shared.NewStockCode("000001", shared.MarketCN)
	for _, posts := range [][]stock.SocialPost{nil, {{Code: other, Platform: "forum", Content: "wrong stock", PublishedAt: end.Add(-time.Hour)}}, {{Code: root.Code(), Platform: "forum", Content: "future", PublishedAt: end}}, {{Code: root.Code(), Platform: "forum", Content: " ", PublishedAt: end.Add(-time.Hour)}}} {
		root := newTestContext(t, analysis.DepthQuick)
		root.LoadMarketBrief(MarketBrief{Social: posts, News: []stock.News{{Code: root.Code(), Title: "新闻不能代替社交", PublishedAt: end.Add(-time.Hour)}}})
		runtime := &optionalRuntime{}
		if err := NewSentimentAnalyst().Act(context.Background(), runtime, root); err != nil {
			t.Fatal(err)
		}
		turns := root.Turns()
		if runtime.calls.Load() != 0 || len(turns) != 1 || !turns[0].Skipped || turns[0].Failed || turns[0].Content != "" || turns[0].AnalystReport != nil || len(root.Reports()) != 0 || len(root.FailedKinds()) != 0 || root.Usage().Calls != 0 {
			t.Fatalf("optional absence became report/failure: %+v", turns)
		}
	}
}
func TestSentimentWithValidSocialRunsNormally(t *testing.T) {
	root := newTestContext(t, analysis.DepthQuick)
	start, _ := stock.MarketDayBounds(root.Code().Market, root.TradeDate())
	root.LoadMarketBrief(MarketBrief{Social: []stock.SocialPost{{Code: root.Code(), Platform: "forum", Content: "sample", PublishedAt: start.Add(time.Hour)}}})
	runtime := &optionalRuntime{}
	if err := NewSentimentAnalyst().Act(context.Background(), runtime, root); err != nil {
		t.Fatal(err)
	}
	if runtime.calls.Load() != 1 || root.Turns()[0].Skipped || root.Reports()["sentiment"] == "" {
		t.Fatal("available sentiment was skipped")
	}
}
func TestOptionalSkipDoesNotBlockOtherAnalystsAndIsNotCountedAsSuccess(t *testing.T) {
	for _, failMarket := range []bool{false, true} {
		root := newTestContext(t, analysis.DepthQuick)
		market := newFakeAgent(t, vo.KindMarketAnalyst, vo.PolicyTolerant)
		if failMarket {
			market.fail = errors.New("market failed")
		}
		stage := Stage{Phase: vo.PhaseAnalyst, Mode: vo.ModeParallel, Members: []Agent{market, NewSentimentAnalyst()}, Limit: 2, MinSuccess: 1}
		sink := &recordingSink{}
		outcomes, err := NewOrchestrator(Plan{Stages: []Stage{stage}}, sink).Run(context.Background(), &optionalRuntime{}, root)
		if (err != nil) != failMarket || len(outcomes) != 1 || len(outcomes[0].SkippedAgents) != 1 || outcomes[0].SkippedAgents[0] != vo.KindSentimentAnalyst {
			t.Fatalf("skip affected stage success/failure: %+v %v", outcomes, err)
		}
		_, failed := sink.snapshot()
		for _, key := range failed {
			if key == vo.StepKeyOf(vo.KindSentimentAnalyst).String() {
				t.Fatal("skip recorded as failed progress")
			}
		}
	}
}
func TestOnlyOptionalSentimentIsCleanlySkipped(t *testing.T) {
	root := newTestContext(t, analysis.DepthQuick)
	runtime := &optionalRuntime{}
	plan := Plan{Stages: []Stage{{Phase: vo.PhaseAnalyst, Members: []Agent{NewSentimentAnalyst()}, MinSuccess: 1}, {Phase: vo.PhaseTrading, Members: []Agent{NewTrader()}}}}
	outcomes, err := NewOrchestrator(plan, &recordingSink{}).Run(context.Background(), runtime, root)
	if err != nil || len(outcomes) != 2 || !outcomes[0].Skipped || !outcomes[1].Skipped || runtime.calls.Load() != 0 {
		t.Fatalf("optional-only run failed or triggered trading: %+v %v", outcomes, err)
	}
}
