package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

type newsRecoveryProvider struct {
	*stubProvider
	items     []vo.News
	newsCalls int
}

func (p *newsRecoveryProvider) FetchNews(context.Context, shared.StockCode, shared.DateRange, int) ([]vo.News, error) {
	p.newsCalls++
	return p.items, nil
}
func TestEmptyNewsSourceFallsThroughToNextConfiguredProvider(t *testing.T) {
	first := &newsRecoveryProvider{stubProvider: &stubProvider{name: "empty", markets: []shared.Market{shared.MarketCN}}}
	second := &newsRecoveryProvider{stubProvider: &stubProvider{name: "available", markets: []shared.Market{shared.MarketCN}}, items: []vo.News{{Title: "公司公告"}}}
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	items, err := NewComposite(first, second).FetchNews(context.Background(), code, shared.DateRange{}, 10)
	if err != nil || len(items) != 1 || first.newsCalls != 1 || second.newsCalls != 1 {
		t.Fatalf("empty source stopped fallback: %+v %v", items, err)
	}
}
func TestAnalysisProviderExcludesImplicitMockAndReportsSocialCapability(t *testing.T) {
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	composite := NewComposite(&stubProvider{name: "mock", markets: []shared.Market{shared.MarketCN}})
	if composite.ForAnalysis().Supports(shared.MarketCN) {
		t.Fatal("mock silently supplies real research evidence")
	}
	composite = NewComposite(&stubProvider{name: "real", markets: []shared.Market{shared.MarketCN}})
	_, err := composite.FetchSocialPosts(context.Background(), code, shared.DateRange{}, 10)
	if !errors.Is(err, svc.ErrDataUnsupported) {
		t.Fatalf("capability absence disguised as outage: %v", err)
	}
}
func TestTushareHistoricalQuoteRequestUsesAnalysisDateAndRejectsFutureRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Params["end_date"] != "20261031" || request.Params["start_date"] != "20261017" || request.Params["ts_code"] != "002131.SZ" {
			t.Errorf("wrong point-in-time request: %+v", request.Params)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"fields": []string{"ts_code", "trade_date", "open", "high", "low", "close", "pre_close", "change", "pct_chg", "vol", "amount"}, "items": [][]any{{"002131.SZ", "20261102", 1, 1, 1, 999, 1, 0, 0, 1, 1}, {"002131.SZ", "20261030", 1, 2, 1, 2, 1, 1, 100, 1, 1}}}})
	}))
	defer server.Close()
	provider := NewTushareProvider(fakeProviderToken, server.Client())
	provider.endpoint = server.URL
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	quote, err := provider.FetchQuoteAsOf(context.Background(), code, shared.MustTradeDate("2026-10-31"))
	if err != nil || quote.TradeDate != shared.MustTradeDate("2026-10-30") || quote.Close.String() != "2" {
		t.Fatalf("future/missing quote: %+v %v", quote, err)
	}
}
