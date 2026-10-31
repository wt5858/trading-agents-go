package entities

import (
	"strings"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

// Social sentiment is optional. News cannot substitute for an actual social sample.
func (s ContextSnapshot) AnalystSkipReason(kind vo.AgentKind) string {
	if kind != vo.KindSentimentAnalyst || s.Code.IsZero() || s.TradeDate.IsZero() {
		return ""
	}
	_, end := stock.MarketDayBounds(s.Code.Market, s.TradeDate)
	for _, p := range s.Market.Social {
		if p.Code.SameInstrument(s.Code) && !p.PublishedAt.IsZero() && p.PublishedAt.Before(end) && strings.TrimSpace(p.Platform) != "" && strings.TrimSpace(p.Content) != "" {
			return ""
		}
	}
	return "未取得有效社交舆情样本，已跳过情绪分析"
}

func (c *AnalysisContext) SkipReason(kind vo.AgentKind) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := len(c.turns) - 1; i >= 0; i-- {
		if c.turns[i].Kind == kind {
			if c.turns[i].Skipped {
				return c.turns[i].SkipReason
			}
			return ""
		}
	}
	return ""
}
