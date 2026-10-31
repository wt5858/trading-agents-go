package domain_services

import (
	"fmt"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

func frozenPriceTool(s entities.ContextSnapshot) frozenTool {
	price := vo.SelectPriceSnapshot(s.Code, s.TradeDate, s.Market.Quote, s.Market.Klines)
	e := frozenEvidence{DataKind: price.Kind, Missing: price.Kind == "missing", Preparation: preparationOf(s.Market, stock.AnalysisQuote)}
	var docs []evidenceDocument
	switch price.Kind {
	case "quote_snapshot":
		e.Data = formatQuote(price.Quote)
		e.Source = price.Quote.Source
		docs = []evidenceDocument{{Source: e.Source, AvailableOn: price.Quote.TradeDate.String(), Content: e.Data}}
	case "daily_kline":
		k := price.Daily
		e.Source = k.Source
		if e.Source == "" {
			e.Source = "prepared_daily_klines"
		}
		e.MissingFields = []string{"昨收", "涨跌额", "涨跌幅", "换手率", "PE", "PB", "实时行情"}
		var b strings.Builder
		fmt.Fprintf(&b, "行情快照未提供，以下为已准备的历史日线摘要，不能视为实时行情。\n交易日: %s\n复权: %t\n开盘: %s  最高: %s  最低: %s  收盘: %s\n成交量: %s  成交额: %s\n缺失字段: %s；未计算或补零。", k.TradeDate.String(), k.Adjusted, k.Open.String(), k.High.String(), k.Low.String(), k.Close.String(), k.Volume.String(), k.Amount.String(), strings.Join(e.MissingFields, "、"))
		if k.Adjusted {
			b.WriteString("\n这些是复权价格，不能作为未复权现价或交易执行价。")
		}
		e.Data = b.String()
		docs = []evidenceDocument{{Source: e.Source, AvailableOn: k.TradeDate.String(), Content: e.Data}}
	default:
		e.MissingReason = s.Market.QuoteMissingReason
		if e.MissingReason == "" {
			e.MissingReason = "本次快照没有符合标的及分析日期的行情记录，也没有可用日线；需先准备行情数据再发起新分析。"
		}
	}
	return frozenTool{spec: vo.NewToolSpec(vo.ToolGetQuote, "读取已准备的行情快照；快照缺失时返回历史日线摘要并标记缺失字段，不能当作实时完整报价。", vo.NewParamSchema(nil)), evidence: e, documents: docs}
}
