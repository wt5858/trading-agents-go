package dtos

import (
	"testing"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"go.mongodb.org/mongo-driver/bson"
)

func TestLegacyQuoteMissingMarketIsRecognizedForWeekendAnalysis(t *testing.T) {
	for _, symbol := range []string{"002131", "002131.SZ"} {
		raw, err := bson.Marshal(bson.M{"symbol": symbol, "trade_date": "2026-10-30", "raw_code": "002131.SZ", "source": "archived"})
		if err != nil {
			t.Fatal(err)
		}
		var row QuoteDto
		if err := bson.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		quote := row.ToDomain()
		code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
		request := vo.AnalysisDataRequest{Code: code, AsOf: shared.MustTradeDate("2026-10-31")}
		if !request.QuoteVisible(&quote) || quote.Code.Market != shared.MarketCN || quote.Code.Symbol != "002131" {
			t.Fatalf("valid Friday quote mismatched on Saturday: %+v", quote)
		}
	}
	row := QuoteDto{CodeColumns: CodeColumns{Symbol: "002131", Market: "US"}, TradeDate: "2026-10-30"}
	if row.ToDomain().Code.Market != shared.MarketUS {
		t.Fatal("explicit valid market silently reassigned")
	}
}

func TestInlineInstrumentSurvivesBSONForAllMarketDataDTOs(t *testing.T) {
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	day := shared.MustTradeDate("2026-10-30")
	raw, err := bson.Marshal(FromDomainQuote(vo.Quote{Code: code, TradeDate: day}))
	if err != nil {
		t.Fatal(err)
	}
	if bson.Raw(raw).Lookup("symbol").StringValue() != "002131" || bson.Raw(raw).Lookup("market").StringValue() != "CN" {
		t.Fatal("inline identity omitted during encoding")
	}
	var quote QuoteDto
	if err := bson.Unmarshal(raw, &quote); err != nil {
		t.Fatal(err)
	}
	if !quote.ToDomain().Code.SameInstrument(code) {
		t.Fatal("quote identity omitted during decoding")
	}
	var news NewsDto
	if err := bson.Unmarshal(raw, &news); err != nil {
		t.Fatal(err)
	}
	if !news.ToDomain().Code.SameInstrument(code) {
		t.Fatal("news identity omitted")
	}
	var social SocialPostDto
	if err := bson.Unmarshal(raw, &social); err != nil {
		t.Fatal(err)
	}
	if !social.ToDomain().Code.SameInstrument(code) {
		t.Fatal("social identity omitted")
	}
	var kline KlineDto
	if err := bson.Unmarshal(raw, &kline); err != nil {
		t.Fatal(err)
	}
	if !kline.ToDomain().Code.SameInstrument(code) {
		t.Fatal("kline identity omitted")
	}
	var financial FinancialDto
	if err := bson.Unmarshal(raw, &financial); err != nil {
		t.Fatal(err)
	}
	if !financial.ToDomain().Code.SameInstrument(code) {
		t.Fatal("financial identity omitted")
	}
	var marketContext MarketContextDTO
	if err := bson.Unmarshal(raw, &marketContext); err != nil {
		t.Fatal(err)
	}
	if !marketContext.ToDomain().Code.SameInstrument(code) {
		t.Fatal("market context identity omitted")
	}
}
