package llm

import (
	"testing"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

func TestPricingDoesNotPresentUnknownAsFree(t *testing.T) {
	u := vo.Usage{Calls: 1, PromptTokens: 100, TotalTokens: 100}
	if got := PriceUsage("unknown-model", u); got.CostStatus != "unknown_price" {
		t.Fatal(got)
	}
	if got := PriceUsage("gpt-4o", vo.Usage{Calls: 1}); got.CostStatus != "unknown_usage" {
		t.Fatal(got)
	}
	if got := PriceUsage("gpt-4o", u); got.CostStatus != "estimated" || got.PricingVersion == "" {
		t.Fatal(got)
	}
}
