package domain_services

import (
	"context"
	"errors"
	"time"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

type analysisDataStore interface {
	SaveQuotes(context.Context, []vo.Quote) error
	SaveFinancials(context.Context, []vo.Financial) error
	SaveNews(context.Context, []vo.News) error
	SaveSocialPosts(context.Context, []vo.SocialPost) error
}

// PrepareAnalysisData is called once after the analysis's local reads. It performs
// at most two concurrent provider operations, with 10s per kind and 20s overall.
// Each kind is independent; provider/storage errors remain explicit preparations.
func (s *StockService) PrepareAnalysisData(ctx context.Context, r vo.AnalysisDataRequest) (vo.PreparedAnalysisData, error) {
	if s.marketRepo == nil {
		return vo.PreparedAnalysisData{}, custom_errors.Unavailable("行情仓储未配置")
	}
	provider := s.provider
	if selection, ok := provider.(interface{ ForAnalysis() DataProvider }); ok {
		provider = selection.ForAnalysis()
	}
	return prepareAnalysisData(ctx, r, s.marketRepo, provider, time.Now())
}
func prepareAnalysisData(ctx context.Context, r vo.AnalysisDataRequest, store analysisDataStore, provider DataProvider, now time.Time) (vo.PreparedAnalysisData, error) {
	if err := r.Validate(); err != nil {
		return vo.PreparedAnalysisData{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	type result struct {
		data    vo.PreparedAnalysisData
		attempt vo.DataPreparation
	}
	outcomes, groupErr := concurrency.Settle(ctx, r.Needs, 2, func(ctx context.Context, kind vo.AnalysisDataKind) (result, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		started := time.Now()
		out := result{attempt: vo.DataPreparation{Kind: kind, Status: "empty", Message: "已尝试第三方，但未返回符合分析时点的记录"}}
		if provider == nil || provider.Name() == "mock" || !provider.Supports(r.Code.Market) {
			out.attempt.Status = "unsupported"
			out.attempt.Message = "没有配置支持该市场的真实第三方数据源"
			return out, nil
		}
		out.attempt.Provider = provider.Name()
		var err error
		switch kind {
		case vo.AnalysisQuote:
			var quote *vo.Quote
			if p, ok := provider.(HistoricalQuoteProvider); ok {
				quote, err = p.FetchQuoteAsOf(ctx, r.Code, r.AsOf)
			} else if r.AsOf == shared.TradeDateOf(now.In(vo.MarketLocation(r.Code.Market))) {
				quote, err = provider.FetchQuote(ctx, r.Code)
			} else {
				err = custom_errors.Unavailable("当前数据源无历史行情快照接口；保留历史日线摘要").Wrap(ErrDataUnsupported)
			}
			if err == nil && quote != nil {
				if r.QuoteVisible(quote) {
					out.data.Quote = quote
					out.attempt.Count = 1
					err = store.SaveQuotes(ctx, []vo.Quote{*quote})
				} else {
					out.attempt.Rejected++
				}
			}
		case vo.AnalysisFinancials:
			var items []vo.Financial
			items, err = provider.FetchFinancials(ctx, r.Code, r.FinancialLimit)
			if err == nil {
				for _, f := range items {
					if r.FinancialVisible(f) {
						out.data.Financials = append(out.data.Financials, f)
					} else {
						out.attempt.Rejected++
					}
				}
				if len(out.data.Financials) > 0 {
					if len(out.data.Financials) > r.FinancialLimit {
						out.data.Financials = out.data.Financials[:r.FinancialLimit]
					}
					out.attempt.Count = len(out.data.Financials)
					err = store.SaveFinancials(ctx, out.data.Financials)
				}
			}
		case vo.AnalysisNews:
			var items []vo.News
			items, err = provider.FetchNews(ctx, r.Code, r.Window, r.NewsLimit)
			if err == nil {
				for _, n := range items {
					if r.PublishedVisible(n.Code, n.PublishedAt) && n.HasNaturalKey() && n.Title != "" {
						out.data.News = append(out.data.News, n)
					} else {
						out.attempt.Rejected++
					}
				}
				if len(out.data.News) > 0 {
					if len(out.data.News) > r.NewsLimit {
						out.data.News = out.data.News[:r.NewsLimit]
					}
					out.attempt.Count = len(out.data.News)
					err = store.SaveNews(ctx, out.data.News)
				}
			}
		case vo.AnalysisSocial:
			var items []vo.SocialPost
			if p, ok := provider.(SocialDataProvider); ok {
				items, err = p.FetchSocialPosts(ctx, r.Code, r.Window, r.SocialLimit)
			} else {
				err = custom_errors.Unavailable("当前配置的数据源不提供社交舆情接口").Wrap(ErrDataUnsupported)
			}
			if err == nil {
				for _, p := range items {
					if r.PublishedVisible(p.Code, p.PublishedAt) && p.Platform != "" && p.Content != "" {
						out.data.Social = append(out.data.Social, p)
					} else {
						out.attempt.Rejected++
					}
				}
				if len(out.data.Social) > 0 {
					if len(out.data.Social) > r.SocialLimit {
						out.data.Social = out.data.Social[:r.SocialLimit]
					}
					out.attempt.Count = len(out.data.Social)
					err = store.SaveSocialPosts(ctx, out.data.Social)
				}
			}
		}
		out.attempt.DurationMS = time.Since(started).Milliseconds()
		if err != nil {
			out.attempt.Status = "failed"
			out.attempt.Message = custom_errors.MessageOf(err)
			if errors.Is(err, context.DeadlineExceeded) {
				out.attempt.Message = "第三方补数请求超时"
			}
			if errors.Is(err, context.Canceled) {
				out.attempt.Message = "第三方补数请求已取消"
			}
			if out.attempt.Count > 0 {
				out.attempt.Message = "第三方已返回数据，但归档保存失败，未纳入本轮证据：" + custom_errors.MessageOf(err)
			}
			if errors.Is(err, ErrDataUnsupported) {
				out.attempt.Status = "unsupported"
				if kind == vo.AnalysisSocial {
					out.attempt.Message = "当前配置的数据源不提供社交舆情接口"
				}
			}
			out.data = vo.PreparedAnalysisData{}
			return out, nil
		}
		if out.attempt.Count > 0 {
			out.attempt.Status = "fetched"
			out.attempt.Saved = true
			out.attempt.Message = "本地缺失后已从第三方补齐并入库"
		}
		return out, nil
	})
	out := vo.PreparedAnalysisData{}
	for _, o := range outcomes {
		if o.Err != nil {
			out.Preparations = append(out.Preparations, vo.DataPreparation{Kind: r.Needs[o.Index], Status: "failed", Message: "第三方补数被取消或超时"})
			continue
		}
		d := o.Value.data
		if d.Quote != nil {
			out.Quote = d.Quote
		}
		out.Financials = append(out.Financials, d.Financials...)
		out.News = append(out.News, d.News...)
		out.Social = append(out.Social, d.Social...)
		out.Preparations = append(out.Preparations, o.Value.attempt)
	}
	return out, groupErr
}
