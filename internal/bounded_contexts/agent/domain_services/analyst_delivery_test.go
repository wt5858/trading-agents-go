package domain_services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

func deliveryReport(t *testing.T, turn entities.Turn) string {
	reg, _ := freezeMarket(turn)
	page := reg.tools[0].evidence
	quote := "交易日: " + turn.Snapshot.TradeDate.String()
	raw, err := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Kind: "fact", Claim: quote, EvidenceIDs: []string{page.ID}, EvidenceQuotes: map[string]string{page.ID: quote}}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func TestTruncatedAnalystJSONRepairsOnceWithCumulativeUsageAndCheckpoint(t *testing.T) {
	turn := pilotTurn(t)
	good := deliveryReport(t, turn)
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "quote", Name: vo.ToolGetQuote}}, Usage: vo.Usage{Calls: 1, TotalTokens: 10, CostUSD: decimal.RequireFromString("0.01")}}, {Content: `{"status":"complete","findings":[`, FinishReason: "length", Usage: vo.Usage{Calls: 1, TotalTokens: 20, CostUSD: decimal.RequireFromString("0.02")}}, {Content: good, Usage: vo.Usage{Calls: 1, TotalTokens: 30, CostUSD: decimal.RequireFromString("0.03")}}}}
	var snapshots []entities.TurnResult
	ctx := context.WithValue(context.Background(), modelObserverKey{}, modelObserver(func(partial entities.TurnResult) error { snapshots = append(snapshots, partial); return nil }))
	result, err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{MarketAgentEnabled: true}, nil).Execute(ctx, turn)
	if err != nil || result.AnalystReport == nil || !result.AnalystReport.SupportVerified || len(result.ModelCalls) != 3 || result.Usage.TotalTokens != 60 || !result.Usage.CostUSD.Equal(decimal.RequireFromString("0.06")) {
		t.Fatalf("repair/accounting failed: %+v %v", result, err)
	}
	last := snapshots[len(snapshots)-1]
	if last.Usage.TotalTokens != 60 || len(last.ModelCalls) != 3 || last.ModelCalls[2].Round != 2 {
		t.Fatalf("checkpoint reset original usage: %+v", last)
	}
	if len(client.round(2).Tools) != 0 {
		t.Fatal("repair permitted more tools")
	}
	var artifact struct {
		Valid    bool                       `json:"valid"`
		Attempts []analystValidationAttempt `json:"validation_attempts"`
	}
	_ = json.Unmarshal([]byte(result.AgentArtifact), &artifact)
	if !artifact.Valid || len(artifact.Attempts) != 2 || artifact.Attempts[0].Error == "" || artifact.Attempts[1].Error != "" {
		t.Fatal("raw failure or repair outcome lost")
	}
}
func TestAnalystRepairDoesNotAcceptInventedEvidenceOrLoop(t *testing.T) {
	turn := pilotTurn(t)
	bad := `{"status":"complete","direction":"neutral","findings":[{"kind":"fact","claim":"fabricated","evidence_ids":["invented"],"evidence_quotes":{"invented":"fabricated"}}]}`
	client := &scriptedClient{replies: []vo.CompletionResult{{Content: bad}, {Content: bad}}}
	result, err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{MarketAgentEnabled: true}, nil).Execute(context.Background(), turn)
	if err == nil || client.rounds() != 2 || result.AnalystReport != nil || result.Content != "" || !strings.Contains(err.Error(), "引用了未读取或不可用的证据") {
		t.Fatalf("unsafe repair or unbounded loop: %+v %v", result, err)
	}
}
func TestMissingFinancialsDeclinesWithoutUsingAuxiliaryQuoteAsEvidence(t *testing.T) {
	turn := pilotTurn(t)
	turn.Contract = entities.NewFundamentalsAnalyst().Contract()
	client := &scriptedClient{}
	result, err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{AnalystAgentsEnabled: true}, nil).Execute(context.Background(), turn)
	if err != nil || client.rounds() != 0 || result.AnalystReport == nil || result.AnalystReport.Status != "insufficient" || result.AnalystReport.DecisionEligible() || len(result.AnalystReport.Findings) != 0 {
		t.Fatalf("auxiliary price treated as fundamentals: %+v %v", result, err)
	}
}

func TestNewsRepairRemovesQuoteOnlyFindingAndKeepsNewsEvidence(t *testing.T) {
	turn := pilotTurn(t)
	turn.Contract = entities.NewNewsAnalyst().Contract()
	turn.Snapshot.Market.News = []stock.News{{Code: turn.Snapshot.Code, PublishedAt: time.Date(2024, 3, 1, 1, 0, 0, 0, time.UTC), Source: "exchange", Title: "公司披露年度报告"}}
	reg, _ := freezeMarket(turn)
	var news, quote frozenEvidence
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetNews {
			news = tool.evidence
		}
		if tool.spec.Name == vo.ToolGetQuote {
			quote = tool.evidence
		}
	}
	newsFinding := vo.AnalystFinding{Kind: "fact", Claim: "公司披露年度报告", EvidenceIDs: []string{news.ID}, EvidenceQuotes: map[string]string{news.ID: "公司披露年度报告"}}
	quoteFinding := vo.AnalystFinding{Kind: "fact", Claim: "交易日: " + turn.Snapshot.TradeDate.String(), EvidenceIDs: []string{quote.ID}, EvidenceQuotes: map[string]string{quote.ID: "交易日: " + turn.Snapshot.TradeDate.String()}}
	bad, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{newsFinding, quoteFinding}})
	good, _ := json.Marshal(vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{newsFinding}})
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "news", Name: vo.ToolGetNews}, {ID: "quote", Name: vo.ToolGetQuote}}}, {Content: string(bad)}, {Content: string(good)}}}
	result, err := NewRuntimeService(fixedRouter{client}, nil, nil, nil, RuntimeConfig{AnalystAgentsEnabled: true}, nil).Execute(context.Background(), turn)
	if err != nil || result.AnalystReport == nil || len(result.AnalystReport.Findings) != 1 || result.AnalystReport.Findings[0].Claim != newsFinding.Claim || client.rounds() != 3 {
		t.Fatalf("news correction lost supported finding: %+v %v", result, err)
	}
	var artifact struct {
		Attempts []analystValidationAttempt `json:"validation_attempts"`
	}
	if err := json.Unmarshal([]byte(result.AgentArtifact), &artifact); err != nil || len(artifact.Attempts) != 2 || !strings.Contains(artifact.Attempts[0].Error, "role-specific evidence") {
		t.Fatalf("missing news-specific diagnostic: %+v %v", artifact, err)
	}
}

func TestQuotePlaceholderRatesAreNotDeliveredAsObservedZero(t *testing.T) {
	quote := pilotTurn(t).Snapshot.Market.Quote
	quote.PreClose = decimal.NewFromInt(101)
	quote.Volume = decimal.NewFromInt(100)
	data := formatQuote(quote)
	if !strings.Contains(data, "存储字段与收盘/昨收不一致") || !strings.Contains(data, "换手率: 暂无") {
		t.Fatalf("placeholder zero rates presented as observed: %s", data)
	}
}
