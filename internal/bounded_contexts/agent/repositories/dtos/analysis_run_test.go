package dtos

import (
	"github.com/shopspring/decimal"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"

	"testing"
	"time"
)

func TestModelAccountingDTORoundTrip(t *testing.T) {
	original := vo.TurnRecord{Kind: vo.KindMarketAnalyst, AgentArtifact: `{"version":"market-agent-v1","valid":true}`, ModelCalls: []vo.ModelCallRecord{{Round: 1, Model: "historical-model", Provider: "test", StartedAt: time.Now().Truncate(time.Millisecond), Duration: 12 * time.Millisecond, Usage: vo.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Calls: 1, CostUSD: decimal.RequireFromString("0.012345"), PricingVersion: "historical-price", CostStatus: "estimated"}, FinishReason: "stop"}}}
	row := turnColumnsOf(original)
	got := row.toDomain()
	if got.AgentArtifact != original.AgentArtifact || len(got.ModelCalls) != 1 {
		t.Fatalf("lost artifact: %+v", got)
	}
	call := got.ModelCalls[0]
	if !call.Usage.CostUSD.Equal(original.ModelCalls[0].Usage.CostUSD) || call.Usage.PricingVersion != "historical-price" || call.Usage.CostStatus != "estimated" || call.Duration != 12*time.Millisecond {
		t.Fatalf("lost historical accounting: %+v", call)
	}
}
func TestLegacyTurnWithoutModelCalls(t *testing.T) {
	got := (turnColumns{Kind: "market", Content: "old report"}).toDomain()
	if got.Content != "old report" || len(got.ModelCalls) != 0 || got.AgentArtifact != "" {
		t.Fatalf("legacy trace broken: %+v", got)
	}
}
