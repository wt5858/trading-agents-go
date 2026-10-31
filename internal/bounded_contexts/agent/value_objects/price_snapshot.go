package value_objects

import (
	stockvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// PriceSnapshot selects persisted prices without constructing a synthetic full quote.
// A daily bar retains its adjustment flag and never supplies ratios it did not store.
type PriceSnapshot struct {
	Kind  string
	Quote stockvo.Quote
	Daily stockvo.Kline
}

func SelectPriceSnapshot(code shared.StockCode, asOf shared.TradeDate, quote stockvo.Quote, klines []stockvo.Kline) PriceSnapshot {
	if code.IsZero() || asOf.IsZero() {
		return PriceSnapshot{Kind: "missing"}
	}
	if quote.Code.SameInstrument(code) && !quote.TradeDate.IsZero() && !asOf.Before(quote.TradeDate) {
		return PriceSnapshot{Kind: "quote_snapshot", Quote: quote}
	}
	var latest stockvo.Kline
	for _, k := range klines {
		if k.Code.SameInstrument(code) && k.Period == stockvo.PeriodDaily && !k.TradeDate.IsZero() && !asOf.Before(k.TradeDate) && (latest.TradeDate.IsZero() || latest.TradeDate.Before(k.TradeDate)) {
			latest = k
		}
	}
	if !latest.TradeDate.IsZero() {
		return PriceSnapshot{Kind: "daily_kline", Daily: latest}
	}
	return PriceSnapshot{Kind: "missing"}
}
