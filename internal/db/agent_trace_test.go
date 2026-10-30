package db

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories/dtos"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysisvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"go.mongodb.org/mongo-driver/bson"
)

func TestAgentModelCallCostBSONRoundTrip(t *testing.T) {
	code, err := shared.NewStockCode("600519", shared.MarketCN)
	if err != nil {
		t.Fatal(err)
	}
	req, err := analysisvo.NewRequest(code, shared.MustTradeDate("2024-03-01"), analysisvo.DepthStandard, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ac := entities.NewAnalysisContext("cost-round-trip", req)
	cost := decimal.RequireFromString("0.012345")
	ac.CommitTurn(vo.TurnRecord{Kind: vo.KindMarketAnalyst, AgentArtifact: `{"version":"market-agent-v1"}`, Usage: vo.Usage{CostUSD: cost}, ModelCalls: []vo.ModelCallRecord{{ReservedCostUSD: decimal.RequireFromString("0.1"), Model: "test", Usage: vo.Usage{CostUSD: cost, PricingVersion: "historical", CostStatus: "estimated"}}}})
	row := dtos.FromDomainAnalysisRun(ac, time.Now(), "")
	raw, err := bson.MarshalWithRegistry(newDecimalRegistry(), row)
	if err != nil {
		t.Fatal(err)
	}
	var restored dtos.AnalysisRunDto
	if err := bson.UnmarshalWithRegistry(newDecimalRegistry(), raw, &restored); err != nil {
		t.Fatal(err)
	}
	trace := restored.ToDomain()
	if len(trace.Turns) != 1 || len(trace.Turns[0].ModelCalls) != 1 {
		t.Fatal("missing turn/call")
	}
	got := trace.Turns[0].ModelCalls[0]
	if !got.Usage.CostUSD.Equal(cost) || !got.ReservedCostUSD.Equal(decimal.RequireFromString("0.1")) || got.Usage.PricingVersion != "historical" || got.Usage.CostStatus != "estimated" || !trace.Usage.CostUSD.Equal(cost) {
		t.Fatalf("historical accounting changed: %+v", trace)
	}
}
