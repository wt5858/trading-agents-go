package marketdata

import (
	"context"
	"time"

	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Automatic recovery must not silently replace missing real evidence with mock data.
func (c *Composite) ForAnalysis() svc.DataProvider {
	var providers []svc.DataProvider
	for _, p := range c.providers {
		if p.Name() != "mock" {
			providers = append(providers, p)
		}
	}
	return NewComposite(providers...)
}

func (c *Composite) FetchQuoteAsOf(ctx context.Context, code shared.StockCode, asOf shared.TradeDate) (*vo.Quote, error) {
	if asOf.IsZero() {
		return nil, custom_errors.Invalid("历史行情必须指定日期")
	}
	today := shared.TradeDateOf(time.Now().In(vo.MarketLocation(code.Market)))
	return firstSuccess(ctx, c, code.Market, "FetchQuoteAsOf", func(p svc.DataProvider) (*vo.Quote, error) {
		var q *vo.Quote
		var err error
		if historical, ok := p.(svc.HistoricalQuoteProvider); ok {
			q, err = historical.FetchQuoteAsOf(ctx, code, asOf)
		} else if asOf == today {
			q, err = p.FetchQuote(ctx, code)
		} else {
			return nil, custom_errors.Unavailable("数据源没有历史行情快照接口").Wrap(svc.ErrDataUnsupported)
		}
		if err == nil && (q == nil || !q.Code.SameInstrument(code) || q.TradeDate.IsZero() || asOf.Before(q.TradeDate)) {
			err = custom_errors.NotFound("数据源没有符合分析日期的行情")
		}
		return q, err
	})
}
func (c *Composite) FetchSocialPosts(ctx context.Context, code shared.StockCode, r shared.DateRange, limit int) ([]vo.SocialPost, error) {
	return firstSuccess(ctx, c, code.Market, "FetchSocialPosts", func(p svc.DataProvider) ([]vo.SocialPost, error) {
		capability, ok := p.(svc.SocialDataProvider)
		if !ok {
			return nil, custom_errors.Unavailable("数据源没有社交舆情接口").Wrap(svc.ErrDataUnsupported)
		}
		items, err := capability.FetchSocialPosts(ctx, code, r, limit)
		if err == nil && len(items) == 0 {
			err = custom_errors.NotFound("数据源返回空社交舆情")
		}
		return items, err
	})
}
