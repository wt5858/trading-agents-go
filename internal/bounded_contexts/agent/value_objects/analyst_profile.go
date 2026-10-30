package value_objects

import "fmt"

func AnalystObjective(k AgentKind) string {
	switch k {
	case KindMarketAnalyst:
		return "趋势、动量、量价和风险，禁止用缺失指标推断具体数值"
	case KindFundamentalsAnalyst:
		return "已披露财报的盈利、成长、负债和估值；缺失财报时明确证据不足"
	case KindNewsAnalyst:
		return "资讯的时效、可信度、利好利空及持续性，区分事实和传闻"
	case KindSentimentAnalyst:
		return "社交舆情样本、热度、情感倾向和样本偏差，不能用新闻代替社交数据"
	case KindSectorAnalyst:
		return "行业/板块相对强弱、轮动及风险；必须使用板块背景，个股价格不能替代行业数据"
	case KindIndexAnalyst:
		return "大盘指数环境及系统风险；必须使用指数背景，个股价格不能替代指数"
	}
	return ""
}
func IsPrimaryAnalystTool(k AgentKind, n ToolName) bool {
	switch k {
	case KindMarketAnalyst:
		return n == ToolGetQuote || n == ToolGetKlines || n == ToolGetTechnicalIndics
	case KindFundamentalsAnalyst:
		return n == ToolGetFinancials
	case KindNewsAnalyst:
		return n == ToolGetNews
	case KindSentimentAnalyst:
		return n == ToolGetSocialSentiment
	case KindSectorAnalyst:
		return n == ToolGetSectorContext
	case KindIndexAnalyst:
		return n == ToolGetIndexContext
	}
	return false
}
func (r AnalystReport) RequirePrimaryEvidence(primary map[string]bool) error {
	for _, f := range r.Findings {
		for _, id := range f.EvidenceIDs {
			if primary[id] {
				return nil
			}
		}
	}
	return fmt.Errorf("report must cite role-specific evidence")
}
