package value_objects

import (
	"testing"

	"github.com/shopspring/decimal"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func TestPriceSnapshotPrefersValidQuoteAndFiltersDailyFallback(t *testing.T) {
	code, _ := shared.NewStockCode("600519", shared.MarketCN)
	other, _ := shared.NewStockCode("000001", shared.MarketCN)
	day := shared.MustTradeDate("2024-03-01")
	valid := stock.Kline{Code: code, Period: stock.PeriodDaily, TradeDate: day.AddDays(-1), Close: decimal.RequireFromString("123.456789"), Adjusted: true, Source: "archive"}
	future := valid
	future.TradeDate = day.AddDays(1)
	future.Close = decimal.NewFromInt(999)
	wrong := valid
	wrong.Code = other
	weekly := valid
	weekly.Period = stock.PeriodWeekly
	weekly.TradeDate = day
	older := valid
	older.TradeDate = day.AddDays(-5)
	bars := []stock.Kline{future, wrong, weekly, older, valid}
	p := SelectPriceSnapshot(code, day, stock.Quote{}, bars)
	if p.Kind != "daily_kline" || p.Daily.TradeDate != valid.TradeDate || !p.Daily.Close.Equal(valid.Close) || !p.Daily.Adjusted || p.Daily.Source != "archive" {
		t.Fatalf("invalid fallback: %+v", p)
	}
	quote := stock.Quote{Code: code, TradeDate: day, Close: decimal.NewFromInt(100), Source: "quote-source"}
	p = SelectPriceSnapshot(code, day, quote, bars)
	if p.Kind != "quote_snapshot" || p.Quote.Source != "quote-source" {
		t.Fatal("real quote replaced by daily approximation")
	}
	quote.TradeDate = day.AddDays(1)
	p = SelectPriceSnapshot(code, day, quote, bars)
	if p.Kind != "daily_kline" {
		t.Fatal("future quote accepted")
	}
	p = SelectPriceSnapshot(code, day, stock.Quote{}, []stock.Kline{future, wrong, weekly})
	if p.Kind != "missing" {
		t.Fatal("ineligible daily bars used")
	}
}
