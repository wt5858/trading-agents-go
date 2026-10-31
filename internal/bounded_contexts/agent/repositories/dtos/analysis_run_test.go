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

func TestReusedFrozenToolResultSurvivesTraceRoundTrip(t *testing.T) {
	original := vo.TurnRecord{Kind: vo.KindMarketAnalyst, ToolCalls: []vo.ToolCallRecord{{Name: vo.ToolGetQuote, OK: true, Reused: true, ResultChars: 12}}}
	got := turnColumnsOf(original).toDomain()
	if len(got.ToolCalls) != 1 || !got.ToolCalls[0].Reused || !got.ToolCalls[0].OK || got.ToolCalls[0].ResultChars != 12 {
		t.Fatalf("lost reused tool trace: %+v", got.ToolCalls)
	}
	legacy := turnColumns{Kind: "market", ToolCalls: []toolCallColumns{{Name: "get_quote", OK: true}}}.toDomain()
	if legacy.ToolCalls[0].Reused {
		t.Fatal("legacy execution incorrectly marked as reuse")
	}
}

func TestSkippedSentimentTraceRemainsDistinctFromFailureAndReport(t *testing.T) {
	original := vo.TurnRecord{Kind: vo.KindSentimentAnalyst, Skipped: true, SkipReason: "没有社交样本"}
	got := turnColumnsOf(original).toDomain()
	if !got.Skipped || got.SkipReason != original.SkipReason || got.Failed || got.Content != "" || got.AnalystReport != nil {
		t.Fatalf("skip became report or failure: %+v", got)
	}
	legacy := (turnColumns{Kind: "sentiment", Content: "older report"}).toDomain()
	if legacy.Skipped {
		t.Fatal("legacy report incorrectly skipped")
	}
}
