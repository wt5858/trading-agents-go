package value_objects

import (
	"fmt"
	"strings"

	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// MarketContext is sourced sector/index evidence keyed by the covered stock.
// Index identities are separate from stock quote symbols.
type MarketContext struct {
	Code        shared.StockCode `json:"code"`
	Scope       string           `json:"scope"`
	Reference   string           `json:"reference"`
	AsOf        shared.TradeDate `json:"as_of"`
	AvailableOn shared.TradeDate `json:"available_on"`
	Source      string           `json:"source"`
	Data        string           `json:"data"`
}

func (v MarketContext) Validate() error {
	canonical, err := shared.NewStockCode(v.Code.Symbol, v.Code.Market)
	if err != nil || !v.Code.Market.Valid() || canonical.Symbol != v.Code.Symbol {
		return fmt.Errorf("invalid context instrument")
	}
	if v.Code.IsZero() || (v.Scope != "sector" && v.Scope != "index") || strings.TrimSpace(v.Reference) == "" || v.AsOf.IsZero() || v.AvailableOn.IsZero() || v.AvailableOn.Before(v.AsOf) || strings.TrimSpace(v.Source) == "" || strings.TrimSpace(v.Data) == "" || len([]rune(v.Data)) > 12000 {
		return fmt.Errorf("invalid sourced market context")
	}
	return nil
}
func (v MarketContext) VisibleTo(code shared.StockCode, cutoff shared.TradeDate) bool {
	return v.Validate() == nil && v.Code.SameInstrument(code) && !cutoff.IsZero() && !cutoff.Before(v.AsOf) && !cutoff.Before(v.AvailableOn)
}
