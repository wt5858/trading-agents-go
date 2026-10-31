package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysisvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stocksvc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stockvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

type researchClient struct {
	plan                                                       vo.ResearchPlan
	gap, novelty, invalidPlan, invalidReview, invalidSynthesis bool
	blockStage                                                 string
	calls, inflight, peak, reviews                             atomic.Int32
}

func (c *researchClient) Provider() string { return "research-test" }
func (c *researchClient) EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool) {
	return decimal.RequireFromString("0.001"), true
}
func (c *researchClient) Complete(ctx context.Context, r vo.CompletionRequest) (*vo.CompletionResult, error) {
	c.calls.Add(1)
	active := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		old := c.peak.Load()
		if active <= old || c.peak.CompareAndSwap(old, active) {
			break
		}
	}
	stage := strings.Split(r.Messages[0].Content, "\n")[0]
	if stage == c.blockStage {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := &vo.CompletionResult{Usage: vo.Usage{Calls: 1, TotalTokens: 30, CostUSD: decimal.RequireFromString("0.0001"), CostStatus: "estimated"}, FinishReason: "stop"}
	var output any
	switch stage {
	case "research:plan":
		output = c.plan
		if c.invalidPlan {
			p := c.plan
			p.Questions = append(p.Questions, p.Questions[0])
			output = p
		}
	case "research:review":
		c.reviews.Add(1)
		review := vo.ResearchReview{Gaps: []vo.ResearchGap{}}
		if c.gap {
			review.Gaps = append(review.Gaps, vo.ResearchGap{QuestionID: c.plan.Questions[0].ID, Reason: "检查现金流反例", Recoverable: true, Query: "counterexample"})
		}
		if c.invalidReview {
			review.Gaps = append(review.Gaps, vo.ResearchGap{QuestionID: "invented", Reason: "wrong"})
		}
		output = review
	case "research:synthesis":
		var trace vo.ResearchTrace
		_ = json.Unmarshal([]byte(r.Messages[len(r.Messages)-1].Content), &trace)
		sel := vo.ResearchSelection{}
		for _, f := range trace.Findings {
			sel.FindingIDs = append(sel.FindingIDs, f.ID)
		}
		if c.invalidSynthesis {
			sel.FindingIDs = append(sel.FindingIDs, "fabricated")
		}
		output = sel
	default:
		// Synchronize enough to exercise actual bounded analyst fan-out.
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		for _, m := range r.Messages {
			if m.Role == vo.RoleTool {
				var page frozenEvidence
				if json.Unmarshal([]byte(m.Content), &page) == nil && page.ID != "" {
					report := vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Kind: "fact", Claim: page.Data, EvidenceIDs: []string{page.ID}, EvidenceQuotes: map[string]string{page.ID: page.Data}}}}
					if page.Missing {
						report = vo.AnalystReport{Status: "insufficient", Direction: "neutral", Missing: []string{"缺失证据"}}
					}
					raw, _ := json.Marshal(report)
					result.Content = string(raw)
					return result, nil
				}
			}
		}
		tool, args := vo.ToolGetQuote, json.RawMessage(`{}`)
		for _, q := range c.plan.Questions {
			if strings.Contains(r.Messages[1].Content, q.Analyst.DisplayName()) && q.Analyst == vo.KindNewsAnalyst {
				tool = vo.ToolGetNews
			}
		}
		if c.novelty {
			for _, m := range r.Messages {
				if strings.HasPrefix(m.Content, "research:work\n") {
					var task vo.ResearchTask
					raw := strings.SplitN(strings.TrimPrefix(m.Content, "research:work\n"), "\n围绕", 2)[0]
					_ = json.Unmarshal([]byte(raw), &task)
					if task.Round > 0 {
						tool = vo.ToolSearchEvidence
						args = json.RawMessage(fmt.Sprintf(`{"query":"counterexample-%c"}`, 'A'+task.Round))
					}
				}
			}
		}
		result.ToolCalls = []vo.ToolCall{{ID: "read", Name: tool, Arguments: args}}
		return result, nil
	}
	raw, err := json.Marshal(output)
	result.Content = string(raw)
	return result, err
}
func researchRoot(t *testing.T) *entities.AnalysisContext {
	turn := pilotTurn(t)
	req, err := analysisvo.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysisvo.DepthStandard, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	root := entities.NewAnalysisContext("research-test", req)
	turn.Snapshot.Market.News = []stockvo.News{{Code: turn.Snapshot.Code, PublishedAt: time.Date(2024, 2, 28, 1, 0, 0, 0, time.UTC), Title: "counterexample-A", Content: "经营现金流与利润不一致", Source: "archive", URL: "https://example.test/a"}, {Code: turn.Snapshot.Code, PublishedAt: time.Date(2024, 2, 27, 1, 0, 0, 0, time.UTC), Title: "counterexample-B", Content: "另一个反例", Source: "archive", URL: "https://example.test/b"}}
	turn.Snapshot.Market.News = append(turn.Snapshot.Market.News, stockvo.News{Code: turn.Snapshot.Code, PublishedAt: time.Date(2024, 2, 26, 1, 0, 0, 0, time.UTC), Title: "counterexample-C", Content: "第三项反例", Source: "archive", URL: "https://example.test/c"})
	root.LoadMarketBrief(turn.Snapshot.Market)
	return root
}
func researchPlan(roles ...vo.AgentKind) vo.ResearchPlan {
	p := vo.ResearchPlan{}
	for i, k := range roles {
		p.Questions = append(p.Questions, vo.ResearchQuestion{ID: fmt.Sprintf("q%d", i+1), Analyst: k, Question: "核查证据与反例", Acceptance: []string{"原文支持并检查反证"}})
	}
	return p
}
func TestResearchWorkflowTracksQuestionsCostAndConcurrency(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst, vo.KindNewsAnalyst)}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	var checkpoints atomic.Int32
	err := rt.Research(WithCostBudget(context.Background(), decimal.NewFromInt(1)), root, "价格与新闻是否相互支持？", vo.DefaultResearchLimits(), func(context.Context) error { checkpoints.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := root.Research()
	if got.Status != "complete" || len(got.Attempts) != 2 || len(got.Selected) != 2 || client.reviews.Load() != 1 || client.peak.Load() != 2 {
		t.Fatalf("invalid workflow: %+v peak=%d", got, client.peak.Load())
	}
	trace := root.TraceSnapshot(time.Now(), "")
	if trace.Usage.Calls != 7 || !trace.Usage.CostUSD.Equal(decimal.RequireFromString("0.0007")) || len(trace.PendingTurns) != 0 || checkpoints.Load() < 10 {
		t.Fatalf("lost accounting/checkpoints: %+v", trace)
	}
	for _, turn := range trace.Turns {
		if turn.Kind == vo.KindTrader || turn.Kind == vo.KindRiskManager || turn.Purpose == "" {
			t.Fatal("research escaped its pipeline or lost purpose")
		}
	}
	copy := root.Research()
	copy.Plan.Questions[0].Acceptance[0] = "mutated"
	if root.Research().Plan.Questions[0].Acceptance[0] == "mutated" {
		t.Fatal("mutable state escaped root")
	}
}
func TestResearchStopsWithoutNewEvidenceAndKeepsReviewGap(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst), gap: true}
	err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(context.Background(), root, "核查反例", vo.DefaultResearchLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := root.Research()
	if len(got.Attempts) != 2 || got.Attempts[1].NewDocuments != 0 || got.StopReason != "no_new_evidence" || got.Status != "partial" || len(got.Unresolved) == 0 || client.reviews.Load() != 1 {
		t.Fatalf("unbounded or hidden gap: %+v", got)
	}
}
func TestResearchAtMostTwoTargetedSupplements(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindNewsAnalyst, vo.KindMarketAnalyst), gap: true, novelty: true}
	err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(context.Background(), root, "新闻反证", vo.DefaultResearchLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := root.Research()
	n := 0
	for _, a := range got.Attempts {
		if a.QuestionID == "q2" {
			n++
		}
	}
	if len(got.Attempts) != 4 || n != 1 || got.StopReason != "supplement_limit" || got.Status != "partial" {
		t.Fatalf("wrong targeted schedule: %+v", got)
	}
}
func TestResearchMalformedCoordinatorReturnsSupportedPartial(t *testing.T) {
	for _, stage := range []string{"plan", "review", "synthesis"} {
		t.Run(stage, func(t *testing.T) {
			root := researchRoot(t)
			client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst), invalidPlan: stage == "plan", invalidReview: stage == "review", invalidSynthesis: stage == "synthesis"}
			err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(context.Background(), root, "核查", vo.DefaultResearchLimits(), nil)
			got := root.Research()
			if err == nil || got.Status == "running" || got.Status == "complete" || got.Report == "" {
				t.Fatalf("malformed %s escaped: %+v %v", stage, got, err)
			}
			if stage == "synthesis" && len(got.Selected) != 1 {
				t.Fatal("valid partial evidence lost")
			}
		})
	}
}
func TestResearchRequestAndTokenBudgetsIncludeCoordinators(t *testing.T) {
	for _, tokens := range []bool{false, true} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			root := researchRoot(t)
			client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst)}
			limits := vo.DefaultResearchLimits()
			if tokens {
				limits.MaxTokens = 1000
			} else {
				limits.MaxRequests = 3
			}
			err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(context.Background(), root, "核查", limits, nil)
			if !errors.Is(err, entities.ErrResearchBudget) || root.Research().StopReason != "budget_exhausted" || client.calls.Load() > 3 {
				t.Fatalf("budget bypass: calls=%d err=%v trace=%+v", client.calls.Load(), err, root.Research())
			}
			if tokens && client.calls.Load() != 0 {
				t.Fatal("token reservation occurred after request")
			}
		})
	}
}
func TestResearchCancellationPreservesFinishedEvidence(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst), blockStage: "research:review"}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(ctx, root, "核查", vo.DefaultResearchLimits(), nil)
	if !errors.Is(err, context.DeadlineExceeded) || root.Research().StopReason != "timeout" || len(root.Research().Selected) != 1 || len(root.TraceSnapshot(time.Now(), "").PendingTurns) != 0 {
		t.Fatalf("lost interrupted research: %+v %v", root.Research(), err)
	}
}

func TestResearchDollarBudgetRejectsBeforeProviderCall(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst)}
	err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil).Research(WithCostBudget(context.Background(), decimal.RequireFromString("0.00001")), root, "核查", vo.DefaultResearchLimits(), nil)
	if !errors.Is(err, entities.ErrCostBudget) || client.calls.Load() != 0 || root.Research().StopReason != "budget_exhausted" {
		t.Fatalf("cost budget escaped: %v %+v", err, root.Research())
	}
}

func TestResearchIndividualAnalystTimeoutStillRunsReview(t *testing.T) {
	root := researchRoot(t)
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst)}
	err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{AgentTimeout: time.Millisecond}, nil).Research(context.Background(), root, "核查", vo.DefaultResearchLimits(), nil)
	if err != nil || client.reviews.Load() != 1 || root.Research().Status != "insufficient" || root.Research().Attempts[0].Error == "" {
		t.Fatalf("single analyst timeout aborted research: %v %+v", err, root.Research())
	}
}

type researchBackfillSpy struct {
	calls atomic.Int32
	end   string
}

func (s *researchBackfillSpy) Klines(_ context.Context, q stocksvc.KlineQuery) ([]stockvo.Kline, error) {
	s.calls.Add(1)
	s.end = q.End
	return nil, errors.New("archive unavailable")
}

func TestResearchEngineUsesBoundedHistoricalBackfillAndHonorsBothCostCeilings(t *testing.T) {
	for _, parentSmall := range []bool{false, true} {
		t.Run(fmt.Sprint(parentSmall), func(t *testing.T) {
			turn := pilotTurn(t)
			req, _ := analysisvo.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysisvo.DepthStandard, nil, "")
			client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst)}
			runtime := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
			reader := newAsOfSpy(turn.Snapshot.Market.Quote)
			backfill := &researchBackfillSpy{}
			parent, engineLimit := decimal.NewFromInt(1), decimal.RequireFromString("0.00001")
			if parentSmall {
				parent, engineLimit = engineLimit, parent
			}
			engine := NewEngineService(runtime, reader, backfill, nil, nil, nil, nil, nil, EngineConfig{MaxCostUSD: engineLimit})
			trace, err := engine.RunResearch(WithCostBudget(context.Background(), parent), "engine-research", req, "核查", vo.DefaultResearchLimits())
			if !errors.Is(err, entities.ErrCostBudget) || client.calls.Load() != 0 || backfill.calls.Load() != 1 || backfill.end != "2024-03-01" || trace.Research == nil || trace.Research.StopReason != "budget_exhausted" {
				t.Fatalf("archive/budget boundary broken: %+v %v", trace, err)
			}
			for _, date := range reader.bounds {
				if date != "2024-03-01" {
					t.Fatalf("wrong historical boundary: %s", date)
				}
			}
		})
	}
}
func TestResearchEnginePreparationFailureStillHasResearchTrace(t *testing.T) {
	turn := pilotTurn(t)
	req, _ := analysisvo.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysisvo.DepthStandard, nil, "")
	client := &researchClient{plan: researchPlan(vo.KindMarketAnalyst)}
	runtime := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	engine := NewEngineService(runtime, nil, nil, nil, nil, nil, nil, nil, EngineConfig{})
	trace, err := engine.RunResearch(context.Background(), "preparation-failure", req, "核查", vo.DefaultResearchLimits())
	if err == nil || trace.Research == nil || trace.Research.Status != "insufficient" || trace.Research.Report == "" || !trace.Failed || client.calls.Load() != 0 {
		t.Fatalf("lost preparation failure trace: %+v %v", trace, err)
	}
}
func TestResearchSearchUsesOnlyAuthorizedPointInTimeEvidence(t *testing.T) {
	root := researchRoot(t)
	brief := root.Snapshot().Market
	future := brief.News[0]
	future.PublishedAt = time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)
	future.Content = "future-only"
	brief.News = append(brief.News, future)
	root.LoadMarketBrief(brief)
	turn := entities.Turn{Contract: entities.NewNewsAnalyst().Contract(), Snapshot: root.Snapshot(), Research: &vo.ResearchTask{}}
	turn.Contract.Access = vo.NewDataAccess(append(turn.Contract.Access.Names(), vo.ToolSearchEvidence)...)
	reg, _ := freezeMarket(turn)
	tool, ok := reg.Lookup(vo.ToolSearchEvidence)
	if !ok {
		t.Fatal("missing search")
	}
	for _, query := range []string{"future-only", "counterexample"} {
		raw, err := tool.Invoke(context.Background(), ToolInvocation{Arguments: json.RawMessage(fmt.Sprintf(`{"query":%q,"limit":1}`, query))})
		if err != nil {
			t.Fatal(err)
		}
		var page frozenEvidence
		_ = json.Unmarshal([]byte(raw), &page)
		if query == "future-only" && !page.Missing {
			t.Fatal("future leakage")
		}
		if query == "counterexample" && (len(page.Documents) != 1 || page.NextOffset == nil || page.Documents[0].Source != "archive") {
			t.Fatalf("lost source/pagination: %s", raw)
		}
	}
	turn.Contract = entities.NewMarketAnalyst().Contract()
	reg, _ = freezeMarket(turn)
	tool, _ = reg.Lookup(vo.ToolSearchEvidence)
	raw, err := tool.Invoke(context.Background(), ToolInvocation{Arguments: json.RawMessage(`{"query":"counterexample"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var page frozenEvidence
	_ = json.Unmarshal([]byte(raw), &page)
	if !page.Missing {
		t.Fatal("cross-role evidence exposed")
	}
	turn.Research = nil
	reg, _ = freezeMarket(turn)
	if _, ok := reg.Lookup(vo.ToolSearchEvidence); ok {
		t.Fatal("research search enabled on regular analysis")
	}
}
