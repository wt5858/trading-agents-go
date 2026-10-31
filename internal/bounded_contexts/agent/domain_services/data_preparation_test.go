package domain_services

import (
	"context"
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stocksvc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

var _ AnalysisDataBackfiller = (*stocksvc.StockService)(nil)

type preparationSpy struct {
	calls   int
	request stock.AnalysisDataRequest
	result  stock.PreparedAnalysisData
}

func (s *preparationSpy) Klines(context.Context, stocksvc.KlineQuery) ([]stock.Kline, error) {
	return nil, nil
}
func (s *preparationSpy) PrepareAnalysisData(_ context.Context, r stock.AnalysisDataRequest) (stock.PreparedAnalysisData, error) {
	s.calls++
	s.request = r
	return s.result, nil
}

func TestEngineRecoversColdDatabaseBeforeFreezingAnalystTools(t *testing.T) {
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	day := shared.MustTradeDate("2026-10-31")
	req, _ := analysis.NewRequest(code, day, analysis.DepthStandard, nil, "")
	filler := &preparationSpy{result: stock.PreparedAnalysisData{Quote: &stock.Quote{Code: code, TradeDate: day.AddDays(-1), Source: "third-party"}, News: []stock.News{{Code: code, Title: "公司公告", URL: "https://example.test/a", PublishedAt: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC), Source: "issuer"}}, Preparations: []stock.DataPreparation{{Kind: stock.AnalysisQuote, Status: "fetched", Saved: true, Count: 1}, {Kind: stock.AnalysisNews, Status: "fetched", Saved: true, Count: 1}, {Kind: stock.AnalysisSocial, Status: "unsupported", Message: "当前数据源没有社交舆情接口"}}}}
	engine := NewEngineService(nil, newAsOfSpy(stock.Quote{}), filler, nil, nil, nil, nil, nil, EngineConfig{})
	brief, err := engine.collect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if filler.calls != 1 || len(filler.request.Needs) != 4 || filler.request.AsOf != day || brief.Quote.Code.IsZero() || len(brief.News) != 1 {
		t.Fatalf("empty DB ended without recovery: %+v %+v", brief, filler.request)
	}
	root := entities.NewAnalysisContext("recovered", req)
	root.LoadMarketBrief(brief)
	reg, _ := freezeMarket(entities.Turn{Contract: entities.NewNewsAnalyst().Contract(), Snapshot: root.Snapshot()})
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetQuote || tool.spec.Name == vo.ToolGetNews {
			if tool.evidence.Missing || tool.evidence.Preparation == nil || tool.evidence.Preparation.Status != "fetched" {
				t.Fatalf("recovered record not frozen: %+v", tool.evidence)
			}
		}
	}
	if len(root.TraceSnapshot(time.Now(), "").DataPreparations) != 3 {
		t.Fatal("preparation attempts not owned by run trace")
	}
	reg, _ = freezeMarket(entities.Turn{Contract: entities.NewSentimentAnalyst().Contract(), Snapshot: root.Snapshot()})
	for _, tool := range reg.tools {
		if tool.spec.Name == vo.ToolGetSocialSentiment && tool.evidence.MissingReason != "当前数据源没有社交舆情接口" {
			t.Fatal("unsupported social reported as unexplained missing")
		}
	}
}
func TestPreparationSkipsAlreadyAvailableKindsAndRechecksInvalidLocalRecords(t *testing.T) {
	root := researchRoot(t)
	snapshot := root.Snapshot()
	req, _ := analysis.NewRequest(snapshot.Code, snapshot.TradeDate, analysis.DepthStandard, nil, "")
	brief := snapshot.Market
	brief.Financials = []stock.Financial{{Code: req.Code, ReportDate: req.TradeDate.AddDays(-90), AnnounceDate: req.TradeDate.AddDays(-1)}}
	brief.Social = []stock.SocialPost{{Code: req.Code, Platform: "local", Content: "sample", PublishedAt: brief.News[0].PublishedAt}}
	filler := &preparationSpy{}
	engine := NewEngineService(nil, nil, filler, nil, nil, nil, nil, nil, EngineConfig{})
	if err := engine.prepareMissingData(context.Background(), req, &brief); err != nil || filler.calls != 0 {
		t.Fatalf("unnecessary third-party IO: %v calls=%d", err, filler.calls)
	}
	brief.News[0].PublishedAt = time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	brief.News = brief.News[:1]
	if err := engine.prepareMissingData(context.Background(), req, &brief); err != nil || filler.calls != 1 || len(filler.request.Needs) != 1 || filler.request.Needs[0] != stock.AnalysisNews {
		t.Fatalf("filtered-out local data suppressed recovery: %+v %v", filler.request, err)
	}
}
