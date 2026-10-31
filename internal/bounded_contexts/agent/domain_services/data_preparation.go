package domain_services

import (
	"context"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Local records are filtered before deciding whether a kind needs one external
// preparation attempt. The backfiller batches kinds and owns provider IO/storage.
func (s *EngineService) prepareMissingData(ctx context.Context, req analysis.Request, b *entities.MarketBrief) error {
	r := stock.AnalysisDataRequest{Code: req.Code, AsOf: req.TradeDate.OrToday(), Window: lookbackRange(req.TradeDate, s.cfg.NewsLookbackDays), FinancialLimit: s.cfg.FinancialLimit, NewsLimit: s.cfg.NewsLimit, SocialLimit: s.cfg.SocialLimit}
	if r.FinancialLimit > 100 {
		r.FinancialLimit = 100
	}
	if r.NewsLimit > 100 {
		r.NewsLimit = 100
	}
	if r.SocialLimit > 100 {
		r.SocialLimit = 100
	}
	if !r.QuoteVisible(&b.Quote) {
		b.Quote = stock.Quote{}
		r.Needs = append(r.Needs, stock.AnalysisQuote)
	}
	var financials []stock.Financial
	for _, f := range b.Financials {
		if r.FinancialVisible(f) {
			financials = append(financials, f)
		}
	}
	b.Financials = financials
	var news []stock.News
	for _, n := range b.News {
		if r.PublishedVisible(n.Code, n.PublishedAt) && n.HasNaturalKey() && n.Title != "" {
			news = append(news, n)
		}
	}
	b.News = news
	var social []stock.SocialPost
	for _, p := range b.Social {
		if r.PublishedVisible(p.Code, p.PublishedAt) && p.Platform != "" && p.Content != "" {
			social = append(social, p)
		}
	}
	b.Social = social
	if len(b.Financials) == 0 {
		r.Needs = append(r.Needs, stock.AnalysisFinancials)
	}
	if len(b.News) == 0 {
		r.Needs = append(r.Needs, stock.AnalysisNews)
	}
	if len(b.Social) == 0 {
		r.Needs = append(r.Needs, stock.AnalysisSocial)
	}
	if len(r.Needs) == 0 {
		return nil
	}
	filler, ok := s.backfill.(AnalysisDataBackfiller)
	if !ok {
		for _, kind := range r.Needs {
			b.DataPreparations = append(b.DataPreparations, stock.DataPreparation{Kind: kind, Status: "disabled", Message: "未配置第三方自动补数通道"})
		}
	} else {
		data, err := filler.PrepareAnalysisData(ctx, r)
		b.DataPreparations = append(b.DataPreparations, data.Preparations...)
		if r.QuoteVisible(data.Quote) {
			b.Quote = *data.Quote
			b.QuoteMissingReason = ""
		}
		for _, f := range data.Financials {
			if r.FinancialVisible(f) {
				b.Financials = append(b.Financials, f)
			}
		}
		for _, n := range data.News {
			if r.PublishedVisible(n.Code, n.PublishedAt) && n.HasNaturalKey() && n.Title != "" {
				b.News = append(b.News, n)
			}
		}
		for _, p := range data.Social {
			if r.PublishedVisible(p.Code, p.PublishedAt) && p.Platform != "" && p.Content != "" {
				b.Social = append(b.Social, p)
			}
		}
		if err != nil && ctx.Err() != nil {
			return custom_errors.Unavailable("数据补齐已取消").Wrap(ctx.Err())
		}
		if err != nil && len(data.Preparations) == 0 {
			for _, kind := range r.Needs {
				b.DataPreparations = append(b.DataPreparations, stock.DataPreparation{Kind: kind, Status: "failed", Message: custom_errors.MessageOf(err)})
			}
		}
	}
	if !r.QuoteVisible(&b.Quote) {
		if p := preparationOf(*b, stock.AnalysisQuote); p != nil {
			if b.QuoteMissingReason != "" {
				b.QuoteMissingReason += "；"
			}
			b.QuoteMissingReason += p.Message
		}
	}
	// Replace transient local-read failures with the final availability result.
	availability := map[string]bool{"行情快照": r.QuoteVisible(&b.Quote), "财务数据": len(b.Financials) > 0, "资讯": len(b.News) > 0, "社交舆情": true}
	var missing []string
	for _, name := range b.Missing {
		if ready, known := availability[name]; !known || !ready {
			missing = append(missing, name)
		}
	}
	b.Missing = missing
	return nil
}

func preparationOf(b entities.MarketBrief, kind stock.AnalysisDataKind) *stock.DataPreparation {
	for i := len(b.DataPreparations) - 1; i >= 0; i-- {
		if b.DataPreparations[i].Kind == kind {
			p := b.DataPreparations[i]
			return &p
		}
	}
	return nil
}

func preparationSummary(items []stock.DataPreparation) string {
	names := map[stock.AnalysisDataKind]string{stock.AnalysisQuote: "行情", stock.AnalysisFinancials: "财务", stock.AnalysisNews: "资讯", stock.AnalysisSocial: "舆情"}
	var parts []string
	for _, p := range items {
		message := []rune(p.Message)
		if len(message) > 120 {
			message = message[:120]
		}
		parts = append(parts, names[p.Kind]+"："+string(message))
	}
	if len(parts) == 0 {
		return "未配置可用的补数通道"
	}
	return strings.Join(parts, "；")
}
