package value_objects

import (
	"fmt"
	"time"
	_ "time/tzdata"

	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

type AnalysisDataKind string

const (
	AnalysisQuote      AnalysisDataKind = "quote"
	AnalysisFinancials AnalysisDataKind = "financials"
	AnalysisNews       AnalysisDataKind = "news"
	AnalysisSocial     AnalysisDataKind = "social"
)

type AnalysisDataRequest struct {
	Code                                   shared.StockCode
	AsOf                                   shared.TradeDate
	Window                                 shared.DateRange
	FinancialLimit, NewsLimit, SocialLimit int
	Needs                                  []AnalysisDataKind
}

func (r AnalysisDataRequest) Validate() error {
	if r.Code.IsZero() || !r.Code.Market.Valid() || r.AsOf.IsZero() || r.Window.Start.IsZero() || r.Window.End != r.AsOf || r.Window.End.Before(r.Window.Start) {
		return fmt.Errorf("analysis backfill requires instrument and bounded point-in-time window")
	}
	if r.FinancialLimit < 1 || r.FinancialLimit > 100 || r.NewsLimit < 1 || r.NewsLimit > 100 || r.SocialLimit < 1 || r.SocialLimit > 100 {
		return fmt.Errorf("invalid analysis backfill limits")
	}
	seen := map[AnalysisDataKind]bool{}
	for _, kind := range r.Needs {
		if seen[kind] {
			return fmt.Errorf("duplicate backfill kind")
		}
		seen[kind] = true
		switch kind {
		case AnalysisQuote, AnalysisFinancials, AnalysisNews, AnalysisSocial:
		default:
			return fmt.Errorf("invalid backfill kind")
		}
	}
	return nil
}

type DataPreparation struct {
	Kind       AnalysisDataKind `json:"kind"`
	Provider   string           `json:"provider"`
	Status     string           `json:"status"`
	Message    string           `json:"message"`
	Count      int              `json:"count"`
	Rejected   int              `json:"rejected"`
	Saved      bool             `json:"saved"`
	DurationMS int64            `json:"duration_ms"`
}
type PreparedAnalysisData struct {
	Quote        *Quote
	Financials   []Financial
	News         []News
	Social       []SocialPost
	Preparations []DataPreparation
}

func MarketLocation(m shared.Market) *time.Location {
	if m == shared.MarketUS {
		if l, err := time.LoadLocation("America/New_York"); err == nil {
			return l
		}
	}
	return time.FixedZone("CST", 8*60*60)
}
func MarketDayBounds(m shared.Market, d shared.TradeDate) (time.Time, time.Time) {
	t, _ := d.Time()
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, MarketLocation(m))
	return start, start.AddDate(0, 0, 1)
}
func (r AnalysisDataRequest) QuoteVisible(q *Quote) bool {
	return q != nil && q.Code.SameInstrument(r.Code) && !q.TradeDate.IsZero() && !r.AsOf.Before(q.TradeDate)
}
func (r AnalysisDataRequest) FinancialVisible(f Financial) bool {
	return f.Code.SameInstrument(r.Code) && !f.ReportDate.IsZero() && !r.AsOf.Before(f.ReportDate) && !f.AnnounceDate.IsZero() && f.DisclosedBy(r.AsOf)
}
func (r AnalysisDataRequest) PublishedVisible(code shared.StockCode, published time.Time) bool {
	start, _ := MarketDayBounds(r.Code.Market, r.Window.Start)
	_, end := MarketDayBounds(r.Code.Market, r.AsOf)
	return code.SameInstrument(r.Code) && !published.IsZero() && !published.Before(start) && published.Before(end)
}
