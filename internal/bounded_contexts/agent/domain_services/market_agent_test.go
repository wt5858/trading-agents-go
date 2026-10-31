package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func pilotTurn(t *testing.T) entities.Turn {
	turn := testTurn(t, entities.NewMarketAnalyst())
	turn.Snapshot.Market.Quote.Code = turn.Snapshot.Code
	turn.Snapshot.Market.Quote.TradeDate = turn.Snapshot.TradeDate
	turn.Snapshot.Market.Quote.Close = decimal.NewFromInt(100)
	return turn
}
func TestMarketAgentFrozenEvidenceAndStoredCost(t *testing.T) {
	turn := pilotTurn(t)
	reg, _ := freezeMarket(turn)
	output := vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Kind: "inference", Claim: "收盘价100", EvidenceIDs: []string{reg.tools[0].evidence.ID}, EvidenceQuotes: map[string]string{reg.tools[0].evidence.ID: reg.tools[0].evidence.Data}}}}
	raw, _ := json.Marshal(output)
	client := &scriptedClient{replies: []vo.CompletionResult{
		{ToolCalls: []vo.ToolCall{{ID: "q1", Name: vo.ToolGetQuote}}, Usage: vo.Usage{Calls: 1, TotalTokens: 10, CostUSD: decimal.RequireFromString("0.01")}},
		{Content: string(raw), Usage: vo.Usage{Calls: 1, TotalTokens: 20, CostUSD: decimal.RequireFromString("0.02")}},
	}}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{MarketAgentEnabled: true}, nil)
	res, err := rt.Execute(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ModelCalls) != 2 || len(res.ToolCalls) != 1 || !res.Usage.CostUSD.Equal(decimal.RequireFromString("0.03")) {
		t.Fatalf("lost accounting: %+v", res)
	}
	if res.AgentArtifact == "" || res.Content == "" {
		t.Fatal("missing persisted artifact/report")
	}
	// No prefilled numerical market data in the initial prompt.
	for _, m := range client.round(0).Messages {
		if m.Role == vo.RoleTool {
			t.Fatal("unexpected prefilled tool result")
		}
	}
	turn.Snapshot.Market.Quote.Close = decimal.NewFromInt(200)
	original, err := reg.tools[0].Invoke(context.Background(), ToolInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	var e frozenEvidence
	_ = json.Unmarshal([]byte(original), &e)
	if e.ID != reg.tools[0].evidence.ID {
		t.Fatal("evidence changed after snapshot mutation")
	}
}
func TestMarketAgentRejectsUnseenAndMissingEvidence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unseen", true: "missing"}[missing], func(t *testing.T) {
			turn := pilotTurn(t)
			if missing {
				turn.Snapshot.Market.Quote.TradeDate = shared.TradeDate{}
			}
			reg, _ := freezeMarket(turn)
			raw, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "bullish", Findings: []vo.AnalystFinding{{Kind: "inference", Claim: "编造事实", EvidenceIDs: []string{reg.tools[0].evidence.ID}, EvidenceQuotes: map[string]string{reg.tools[0].evidence.ID: reg.tools[0].evidence.Data}}}})
			replies := []vo.CompletionResult{{Content: string(raw)}}
			if missing {
				replies = append([]vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "q", Name: vo.ToolGetQuote}}}}, replies...)
			}
			rt := NewRuntimeService(fixedRouter{&scriptedClient{replies: replies}}, nil, nil, nil, RuntimeConfig{MarketAgentEnabled: true}, nil)
			res, err := rt.Execute(context.Background(), turn)
			if missing {
				if err != nil || res.AnalystReport == nil || res.AnalystReport.Status != "insufficient" || len(res.ModelCalls) != 0 {
					t.Fatalf("missing primary evidence did not abstain: %+v %v", res, err)
				}
				return
			}
			if err == nil || res.Content != "" || res.AgentArtifact == "" {
				t.Fatalf("invalid report accepted: %+v %v", res, err)
			}
		})
	}
}
func TestMarketAgentBudgetStopsBeforeNextRequest(t *testing.T) {
	turn := pilotTurn(t)
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "q", Name: vo.ToolGetQuote}}, Usage: vo.Usage{Calls: 1, TotalTokens: 10}}}}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{MarketAgentEnabled: true, AgentMaxTotalTokens: 10}, nil)
	res, err := rt.Execute(context.Background(), turn)
	if err == nil || client.rounds() != 1 || len(res.ModelCalls) != 1 || res.Usage.TotalTokens != 10 {
		t.Fatalf("budget/accounting: %+v %v", res, err)
	}
}
func TestFailedModelAttemptIsRecorded(t *testing.T) {
	rt := NewRuntimeService(fixedRouter{&scriptedClient{err: errors.New("offline")}}, nil, nil, nil, RuntimeConfig{}, nil)
	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err == nil || len(res.ModelCalls) != 1 || !res.ModelCalls[0].Failed {
		t.Fatalf("failed attempt missing: %+v %v", res, err)
	}
}
func TestMarketSnapshotFiltersFutureQuote(t *testing.T) {
	turn := pilotTurn(t)
	turn.Snapshot.Market.Quote.TradeDate = turn.Snapshot.TradeDate.AddDays(1)
	reg, _ := freezeMarket(turn)
	if !reg.tools[0].evidence.Missing || reg.tools[0].evidence.Data != "数据缺失" {
		t.Fatal("future quote exposed")
	}
}

func TestMarketAnalystPairedExperiment(t *testing.T) {
	turn := pilotTurn(t)
	reg, id := freezeMarket(turn)
	raw, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Kind: "inference", Claim: "收盘100", EvidenceIDs: []string{reg.tools[0].evidence.ID}, EvidenceQuotes: map[string]string{reg.tools[0].evidence.ID: reg.tools[0].evidence.Data}}}})
	client := &scriptedClient{replies: []vo.CompletionResult{
		{Content: string(raw), Usage: vo.Usage{Calls: 1, TotalTokens: 5}},
		{ToolCalls: []vo.ToolCall{{ID: "q", Name: vo.ToolGetQuote}}, Usage: vo.Usage{Calls: 1, TotalTokens: 10}},
		{Content: string(raw), Usage: vo.Usage{Calls: 1, TotalTokens: 15}},
	}}
	rt := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{}, nil)
	out, err := rt.CompareMarketAnalyst(context.Background(), turn)
	if err != nil || out.BaselineError != "" || out.AgentError != "" {
		t.Fatalf("pair failed: %+v %v", out, err)
	}
	if out.SnapshotID != id || out.Baseline.Usage.TotalTokens != 5 || out.Agent.Usage.TotalTokens != 25 || out.Agent.AgentArtifact == "" {
		t.Fatalf("pair accounting: %+v", out)
	}
	if rt.cfg.MarketAgentEnabled {
		t.Fatal("experiment mutated shared runtime")
	}
}
