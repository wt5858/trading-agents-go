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
	if len(financials) > 8 {
		financials = financials[:8]
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
	if len(news) > 15 {
		news = news[:15]
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
	if len(posts) > 30 {
		posts = posts[:30]
	}
	create := func(n vo.ToolName, desc, data string, missing bool) frozenTool {
		return frozenTool{spec: vo.NewToolSpec(n, desc+"。冻结数据，无参数。", vo.NewParamSchema(nil)), evidence: frozenEvidence{Data: data, Missing: missing, Source: "prepared_point_in_time_data"}}
	}
	out := []frozenTool{
		create(vo.ToolGetFinancials, "已披露财务", formatFinancials(financials), len(financials) == 0),
		create(vo.ToolGetNews, "已发布资讯", formatNews(news), len(news) == 0),
		create(vo.ToolGetSocialSentiment, "已发布社交舆情", formatSocial(posts), len(posts) == 0),
	}
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
		var data strings.Builder
		seen := map[string]bool{}
		for _, c := range contexts {
			if c.Scope == scope && c.VisibleTo(s.Code, s.TradeDate) && !seen[c.Reference] {
				fmt.Fprintf(&data, "参考对象:%s 来源:%s 数据日期:%s 可获得日期:%s\n%s\n", c.Reference, c.Source, c.AsOf.String(), c.AvailableOn.String(), c.Data)
				seen[c.Reference] = true
			}
		}
		name := vo.ToolGetSectorContext
		if scope == "index" {
			name = vo.ToolGetIndexContext
		}
		out = append(out, create(name, scope+"专用背景", data.String(), data.Len() == 0))
	}
	return out
}
