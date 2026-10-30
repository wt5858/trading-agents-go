package value_objects

import (
	"testing"

	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func TestContextRequiresSourceAndAvailability(t *testing.T) {
	code, _ := shared.NewStockCode("600519", shared.MarketCN)
	v := MarketContext{Code: code, Scope: "index", Reference: "benchmark:CSI300", AsOf: shared.MustTradeDate("2024-03-01"), AvailableOn: shared.MustTradeDate("2024-03-04"), Source: "fixture", Data: "index close 100"}
	if v.VisibleTo(code, shared.MustTradeDate("2024-03-01")) {
		t.Fatal("unavailable context leaked")
	}
	if !v.VisibleTo(code, shared.MustTradeDate("2024-03-04")) {
		t.Fatal("available context missing")
	}
	v.Source = ""
	if v.Validate() == nil {
		t.Fatal("source required")
	}
}
