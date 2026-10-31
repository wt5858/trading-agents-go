package domain_services

import (
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

func additionalAnalystTools(turn entities.Turn) []frozenTool {
	s := turn.Snapshot
	var financials []stock.Financial
	for _, f := range s.Market.Financials {
		if f.Code.SameInstrument(s.Code) && !f.ReportDate.IsZero() && !s.TradeDate.Before(f.ReportDate) && !f.AnnounceDate.IsZero() && f.DisclosedBy(s.TradeDate) {
			financials = append(financials, f)
		}
	}
	sort.Slice(financials, func(i, j int) bool { return financials[j].ReportDate.Before(financials[i].ReportDate) })
	financialLimit, newsLimit, socialLimit := 8, 15, 30
	if turn.Research != nil {
		financialLimit, newsLimit, socialLimit = 24, 100, 60
	}
	financialTruncated := len(financials) > financialLimit
	if financialTruncated {
		financials = financials[:financialLimit]
	}
	// End of the analysis date in Asia/Shanghai; publication timestamps retain their offsets.
	date, _ := s.TradeDate.Time()
	location := time.FixedZone("CST", 8*60*60)
	if s.Code.Market.String() == "US" {
		if loaded, err := time.LoadLocation("America/New_York"); err == nil {
			location = loaded
		}
	}
	end := time.Date(date.Year(), date.Month(), date.Day()+1, 0, 0, 0, 0, location)
	var news []stock.News
	for _, n := range s.Market.News {
		if n.Code.SameInstrument(s.Code) && !n.PublishedAt.IsZero() && n.PublishedAt.Before(end) {
			news = append(news, n)
		}
	}
	sort.Slice(news, func(i, j int) bool {
		if news[i].PublishedAt.Equal(news[j].PublishedAt) {
			return news[i].URL < news[j].URL
		}
		return news[i].PublishedAt.After(news[j].PublishedAt)
	})
	newsTruncated := len(news) > newsLimit
	if newsTruncated {
		news = news[:newsLimit]
	}
	var posts []stock.SocialPost
	for _, p := range s.Market.Social {
		if p.Code.SameInstrument(s.Code) && !p.PublishedAt.IsZero() && p.PublishedAt.Before(end) {
			posts = append(posts, p)
		}
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].PublishedAt.Equal(posts[j].PublishedAt) {
			return posts[i].Platform+posts[i].Author+posts[i].Content < posts[j].Platform+posts[j].Author+posts[j].Content
		}
		return posts[i].PublishedAt.After(posts[j].PublishedAt)
	})
	socialTruncated := len(posts) > socialLimit
	if socialTruncated {
		posts = posts[:socialLimit]
	}

	create := func(name vo.ToolName, docs []evidenceDocument) frozenTool {
		truncated := map[vo.ToolName]bool{vo.ToolGetFinancials: financialTruncated, vo.ToolGetNews: newsTruncated, vo.ToolGetSocialSentiment: socialTruncated}[name]
		kind := map[vo.ToolName]stock.AnalysisDataKind{vo.ToolGetFinancials: stock.AnalysisFinancials, vo.ToolGetNews: stock.AnalysisNews, vo.ToolGetSocialSentiment: stock.AnalysisSocial}[name]
		preparation := preparationOf(s.Market, kind)
		reason := ""
		if len(docs) == 0 && preparation != nil {
			reason = preparation.Message
		}
		return frozenTool{spec: vo.NewToolSpec(name, name.String(), vo.NewParamSchema(nil)), documents: docs, evidence: frozenEvidence{Preparation: preparation, MissingReason: reason, Missing: len(docs) == 0, Truncated: truncated, Source: "sourced_documents"}}
	}
	var fdocs, ndocs, pdocs []evidenceDocument
	for _, f := range financials {
		fdocs = append(fdocs, evidenceDocument{Source: f.Source, AvailableOn: f.AnnounceDate.String(), Content: formatFinancials([]stock.Financial{f})})
	}
	seenNews := map[string]bool{}
	for _, n := range news {
		key := n.URL
		if key == "" {
			key = evidenceDigest(n.Title + n.Content)
		}
		if seenNews[key] {
			continue
		}
		seenNews[key] = true
		fingerprint := n.Content
		if strings.TrimSpace(fingerprint) == "" {
			fingerprint = n.Title
		}
		ndocs = append(ndocs, evidenceDocument{ContentFingerprint: evidenceDigest(strings.Join(strings.Fields(fingerprint), " ")), Source: n.Source, URL: n.URL, PublishedAt: n.PublishedAt.Format(time.RFC3339), AvailableOn: n.PublishedAt.Format(time.RFC3339), Content: n.Title + "\n" + n.Content})
	}
	for _, post := range posts {
		pdocs = append(pdocs, evidenceDocument{Source: post.Platform, PublishedAt: post.PublishedAt.Format(time.RFC3339), AvailableOn: post.PublishedAt.Format(time.RFC3339), Content: formatSocial([]stock.SocialPost{post})})
	}
	out := []frozenTool{create(vo.ToolGetFinancials, fdocs), create(vo.ToolGetNews, ndocs), create(vo.ToolGetSocialSentiment, pdocs)}
	contexts := append([]stock.MarketContext(nil), s.Market.Contexts...)
	sort.Slice(contexts, func(i, j int) bool {
		a, c := contexts[i], contexts[j]
		if a.AsOf != c.AsOf {
			return c.AsOf.Before(a.AsOf)
		}
		if a.AvailableOn != c.AvailableOn {
			return c.AvailableOn.Before(a.AvailableOn)
		}
		return a.Reference+a.Source+a.Data < c.Reference+c.Source+c.Data
	})
	for _, scope := range []string{"sector", "index"} {
		var docs []evidenceDocument
		seen := map[string]bool{}
		for _, c := range contexts {
			key := c.Reference + "\x00" + c.Source
			if c.Scope == scope && c.VisibleTo(s.Code, s.TradeDate) && !seen[key] {
				seen[key] = true
				docs = append(docs, evidenceDocument{Source: c.Source, AvailableOn: c.AvailableOn.String(), Content: fmt.Sprintf("参考对象:%s 数据日期:%s\n%s", c.Reference, c.AsOf.String(), c.Data)})
			}
		}
		name := vo.ToolGetSectorContext
		if scope == "index" {
			name = vo.ToolGetIndexContext
		}
		out = append(out, create(name, docs))
	}
	return out
}
