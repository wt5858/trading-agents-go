package domain_services

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

type analysisProviderFake struct {
	DataProvider
	quote                                               *vo.Quote
	financials                                          []vo.Financial
	news                                                []vo.News
	newsErr                                             error
	delay                                               time.Duration
	quoteCalls, newsCalls, financialCalls, active, peak atomic.Int32
}

func (p *analysisProviderFake) Name() string                { return "test-third-party" }
func (p *analysisProviderFake) Supports(shared.Market) bool { return true }
func (p *analysisProviderFake) enter(ctx context.Context) error {
	n := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	delay := p.delay
	if delay == 0 {
		delay = 5 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}
func (p *analysisProviderFake) FetchQuote(ctx context.Context, _ shared.StockCode) (*vo.Quote, error) {
	p.quoteCalls.Add(1)
	if err := p.enter(ctx); err != nil {
		return nil, err
	}
	return p.quote, nil
}
func (p *analysisProviderFake) FetchFinancials(ctx context.Context, _ shared.StockCode, _ int) ([]vo.Financial, error) {
	p.financialCalls.Add(1)
	if err := p.enter(ctx); err != nil {
		return nil, err
	}
	return p.financials, nil
}
func (p *analysisProviderFake) FetchNews(ctx context.Context, _ shared.StockCode, _ shared.DateRange, _ int) ([]vo.News, error) {
	p.newsCalls.Add(1)
	if err := p.enter(ctx); err != nil {
		return nil, err
	}
	return p.news, p.newsErr
}

type historicalAnalysisFake struct {
	*analysisProviderFake
	asOf shared.TradeDate
}

func (p *historicalAnalysisFake) FetchQuoteAsOf(ctx context.Context, _ shared.StockCode, day shared.TradeDate) (*vo.Quote, error) {
	p.asOf = day
	p.quoteCalls.Add(1)
	if err := p.enter(ctx); err != nil {
		return nil, err
	}
	return p.quote, nil
}

type socialAnalysisFake struct {
	*analysisProviderFake
	posts []vo.SocialPost
}

func (p *socialAnalysisFake) FetchSocialPosts(ctx context.Context, _ shared.StockCode, _ shared.DateRange, _ int) ([]vo.SocialPost, error) {
	if err := p.enter(ctx); err != nil {
		return nil, err
	}
	return p.posts, nil
}

type analysisStoreFake struct {
	mu         sync.Mutex
	quote      []vo.Quote
	financials []vo.Financial
	news       []vo.News
	social     []vo.SocialPost
	fail       bool
}

func (s *analysisStoreFake) SaveQuotes(_ context.Context, v []vo.Quote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("private storage error")
	}
	s.quote = append(s.quote, v...)
	return nil
}
func (s *analysisStoreFake) SaveFinancials(_ context.Context, v []vo.Financial) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.financials = append(s.financials, v...)
	return nil
}
func (s *analysisStoreFake) SaveNews(_ context.Context, v []vo.News) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.news = append(s.news, v...)
	return nil
}
func (s *analysisStoreFake) SaveSocialPosts(_ context.Context, v []vo.SocialPost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.social = append(s.social, v...)
	return nil
}
func backfillFixture() (vo.AnalysisDataRequest, time.Time, *analysisProviderFake) {
	code, _ := shared.NewStockCode("002131.SZ", shared.MarketCN)
	day := shared.MustTradeDate("2026-10-31")
	window := shared.DateRange{Start: day.AddDays(-30), End: day}
	r := vo.AnalysisDataRequest{Code: code, AsOf: day, Window: window, FinancialLimit: 8, NewsLimit: 10, SocialLimit: 10, Needs: []vo.AnalysisDataKind{vo.AnalysisQuote, vo.AnalysisFinancials, vo.AnalysisNews, vo.AnalysisSocial}}
	p := &analysisProviderFake{quote: &vo.Quote{Code: code, TradeDate: day.AddDays(-1), Source: "external"}, financials: []vo.Financial{{Code: code, ReportDate: day.AddDays(-30), AnnounceDate: day.AddDays(-1), Source: "external"}}, news: []vo.News{{Code: code, Title: "公告", URL: "https://example.test/disclosure", Source: "issuer", PublishedAt: time.Date(2026, 10, 31, 15, 59, 0, 0, time.UTC)}}}
	return r, time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC), p
}
func TestAnalysisBackfillFetchesPersistsAndFiltersPointInTime(t *testing.T) {
	r, now, base := backfillFixture()
	future := base.news[0]
	future.PublishedAt = time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)
	future.URL += "/future"
	base.news = append(base.news, future)
	futureFinancial := base.financials[0]
	futureFinancial.AnnounceDate = r.AsOf.AddDays(1)
	base.financials = append(base.financials, futureFinancial)
	provider := &socialAnalysisFake{analysisProviderFake: base, posts: []vo.SocialPost{{Code: r.Code, Platform: "test-social", Content: "真实样本", PublishedAt: base.news[0].PublishedAt}}}
	store := &analysisStoreFake{}
	out, err := prepareAnalysisData(context.Background(), r, store, provider, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Quote == nil || out.Quote.TradeDate != shared.MustTradeDate("2026-10-30") || len(out.News) != 1 || len(out.Financials) != 1 || len(out.Social) != 1 || len(store.quote) != 1 || len(store.news) != 1 || len(store.financials) != 1 || len(store.social) != 1 {
		t.Fatalf("missing or leaked data: %+v", out)
	}
	if base.peak.Load() != 2 || base.quoteCalls.Load() != 1 || base.newsCalls.Load() != 1 || base.financialCalls.Load() != 1 {
		t.Fatalf("unbounded/duplicate requests: peak=%d", base.peak.Load())
	}
	for _, a := range out.Preparations {
		if a.Status != "fetched" || !a.Saved || a.Count != 1 {
			t.Fatalf("bad preparation: %+v", a)
		}
	}
}
func TestAnalysisBackfillDistinguishesFailureAndUnsupportedSocial(t *testing.T) {
	r, now, provider := backfillFixture()
	provider.newsErr = custom_errors.Unavailable("资讯源暂不可用")
	out, err := prepareAnalysisData(context.Background(), r, &analysisStoreFake{}, provider, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Quote == nil || len(out.Financials) != 1 || len(out.News) != 0 {
		t.Fatalf("one failed kind cancelled peers: %+v", out)
	}
	statuses := map[vo.AnalysisDataKind]string{}
	for _, a := range out.Preparations {
		statuses[a.Kind] = a.Status
	}
	if statuses[vo.AnalysisNews] != "failed" || statuses[vo.AnalysisSocial] != "unsupported" {
		t.Fatalf("misreported source capability: %+v", statuses)
	}
}
func TestHistoricalQuoteNeverFallsBackToCurrentQuoteEndpoint(t *testing.T) {
	r, now, p := backfillFixture()
	r.AsOf = r.AsOf.AddDays(-365)
	r.Window.End = r.AsOf
	r.Window.Start = r.AsOf.AddDays(-30)
	r.Needs = []vo.AnalysisDataKind{vo.AnalysisQuote}
	p.quote.TradeDate = r.AsOf.AddDays(-1)
	out, err := prepareAnalysisData(context.Background(), r, &analysisStoreFake{}, p, now)
	if err != nil || p.quoteCalls.Load() != 0 || out.Preparations[0].Status != "unsupported" {
		t.Fatalf("current endpoint leaked into history: %+v %v", out, err)
	}
	historical := &historicalAnalysisFake{analysisProviderFake: p}
	out, err = prepareAnalysisData(context.Background(), r, &analysisStoreFake{}, historical, now)
	if err != nil || out.Quote == nil || historical.asOf != r.AsOf || p.quoteCalls.Load() != 1 {
		t.Fatalf("as-of endpoint not used: %+v %v", out, err)
	}
}
func TestAnalysisBackfillDoesNotUseUnsavedDataAndCancelsPromptly(t *testing.T) {
	r, now, p := backfillFixture()
	r.Needs = []vo.AnalysisDataKind{vo.AnalysisQuote}
	out, err := prepareAnalysisData(context.Background(), r, &analysisStoreFake{fail: true}, p, now)
	if err != nil || out.Quote != nil || out.Preparations[0].Saved || out.Preparations[0].Status != "failed" {
		t.Fatalf("unsaved values entered evidence: %+v %v", out, err)
	}
	p.delay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = prepareAnalysisData(ctx, r, &analysisStoreFake{}, p, now)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("cancellation lost: %v", err)
	}
}
