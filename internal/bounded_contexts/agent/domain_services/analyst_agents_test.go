package domain_services

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func TestAllAnalystsUseAuthorizedPrimaryEvidence(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/agent/fixtures/synthetic-six-analysts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []AnalystCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Analyst.String(), func(t *testing.T) {
			turn, err := c.Turn()
			if err != nil {
				t.Fatal(err)
			}
			reg, _ := freezeMarket(turn)
			var evidence frozenTool
			found := false
			for _, tool := range reg.tools {
				if !turn.Contract.Access.Allows(tool.spec.Name) {
					t.Fatal("unauthorized evidence leaked")
				}
				if vo.IsPrimaryAnalystTool(c.Analyst, tool.spec.Name) && !tool.evidence.Missing {
					evidence = tool
					found = true
				}
			}
			if !found {
				t.Fatal("fixture missing primary evidence")
			}
			report, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Claim: "固定样本引用", EvidenceIDs: []string{evidence.evidence.ID}}}})
			client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "p", Name: evidence.spec.Name}}}, {Content: string(report)}}}
			rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{AnalystAgentsEnabled: true}, nil)
			result, err := rt.Execute(context.Background(), turn)
			if err != nil || result.AgentArtifact == "" || len(result.ModelCalls) != 2 {
				t.Fatalf("role execution failed: %+v %v", result, err)
			}
		})
	}
}
func TestQuoteCannotReplaceSectorEvidence(t *testing.T) {
	turn := pilotTurn(t)
	turn.Contract = entities.NewSectorAnalyst().Contract()
	reg, _ := freezeMarket(turn)
	var quote frozenTool
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetQuote {
			quote = tool
		}
	}
	output, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "bullish", Findings: []vo.AnalystFinding{{Claim: "个股上涨所以整个行业上涨", EvidenceIDs: []string{quote.evidence.ID}}}})
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "q", Name: vo.ToolGetQuote}}}, {Content: string(output)}}}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{AnalystAgentsEnabled: true}, nil)
	result, err := rt.Execute(context.Background(), turn)
	if err == nil || result.Content != "" {
		t.Fatal("sector claim accepted without sector evidence")
	}
}
func TestFrozenDataUsesAvailabilityAndNormalizedIdentity(t *testing.T) {
	turn := pilotTurn(t)
	alias := turn.Snapshot.Code
	alias.Raw = "600519.SH"
	turn.Snapshot.Market.Quote.Code = alias
	turn.Contract = entities.NewFundamentalsAnalyst().Contract()
	turn.Snapshot.Market.Financials = []stock.Financial{{Code: alias, ReportDate: shared.MustTradeDate("2023-12-31"), AnnounceDate: shared.MustTradeDate("2024-03-02")}}
	reg, _ := freezeMarket(turn)
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetQuote && tool.evidence.Missing {
			t.Fatal("input spelling treated as another instrument")
		}
		if tool.spec.Name == vo.ToolGetFinancials && !tool.evidence.Missing {
			t.Fatal("future disclosure leaked")
		}
	}
	turn.Contract = entities.NewNewsAnalyst().Contract()
	published := time.Date(2024, 3, 1, 22, 0, 0, 0, time.UTC)
	turn.Snapshot.Market.News = []stock.News{{Code: turn.Snapshot.Code, PublishedAt: published, Title: "late news"}}
	reg, _ = freezeMarket(turn)
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetNews && !tool.evidence.Missing {
			t.Fatal("next-day China news leaked")
		}
	}
}

type estimatedScript struct{ *scriptedClient }

func (estimatedScript) EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool) {
	return decimal.RequireFromString("0.1"), true
}
func TestRunBudgetRejectsUnfundedRequestAndRecordsReservation(t *testing.T) {
	turn := pilotTurn(t)
	client := estimatedScript{&scriptedClient{replies: []vo.CompletionResult{{Content: "正常报告", Usage: vo.Usage{CostUSD: decimal.RequireFromString("0.02"), CostStatus: "estimated", Calls: 1, TotalTokens: 10}}}}}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	ctx := WithCostBudget(context.Background(), decimal.RequireFromString("0.05"))
	_, err := rt.Execute(ctx, turn)
	if err == nil || client.rounds() != 0 {
		t.Fatal("unfunded request reached provider")
	}
	ctx = WithCostBudget(context.Background(), decimal.RequireFromString("0.1"))
	result, err := rt.Execute(ctx, turn)
	if err != nil || !result.ModelCalls[0].ReservedCostUSD.Equal(decimal.RequireFromString("0.1")) {
		t.Fatalf("reservation missing: %+v %v", result, err)
	}
	_, err = rt.Execute(ctx, turn)
	if err == nil || client.rounds() != 1 {
		t.Fatal("settlement did not constrain subsequent request")
	}
}
