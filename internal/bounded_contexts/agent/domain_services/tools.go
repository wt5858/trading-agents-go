package domain_services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 工具入参的默认值与上限。上限是必须的：模型很爱传 limit=10000，
// 而工具结果会在后续每一轮对话里重复出现，一次过量查询能吃掉整个上下文窗口。
const (
	defaultKlineLimit  = 60
	maxKlineLimit      = 250
	defaultNewsLimit   = 10
	maxNewsLimit       = 30
	defaultSocialLimit = 20
	maxSocialLimit     = 50
	defaultFinLimit    = 8
	maxFinLimit        = 20
	defaultLookbackDay = 30
	maxLookbackDay     = 365
)

// StockToolRegistry 是由 stock 上下文数据支撑的工具注册表。
//
// 它是 ToolRegistry 端口的唯一实现。工具本身很薄——取数、格式化成模型好读的文本——
// 真正的设计决策有两条：
//
//  1. 标的由运行时下发，模型只能调节辅助参数（周期、条数、回看天数）。
//     让模型自己指定 symbol，一次提示词注入就能让它去查另一只票，
//     而报告标题还写着原来那只。
//  2. get_technical_indicators 只读不算。它走 IndicatorRepository.Find，
//     绝不会在这里调用 ComputeIndicators——指标是乘除派生量，
//     在读路径上重算会让模型看到的数字和报告、图表上的数字对不上。
type StockToolRegistry struct {
	tools map[value_objects.ToolName]Tool
}

var _ ToolRegistry = (*StockToolRegistry)(nil)

// NewStockToolRegistry 组装六个内置工具。
//
// indicators 仓储可以为 nil（例如未启用 Mongo 的部署），此时指标工具会明确回答
// 「指标服务不可用」，而不是退化成现场计算——现场计算恰恰是这套设计要杜绝的事。
func NewStockToolRegistry(market MarketReader, indicators *repositories.IndicatorRepository) *StockToolRegistry {
	tools := []Tool{
		&quoteTool{market: market},
		&klineTool{market: market},
		&indicatorTool{indicators: indicators},
		&financialTool{market: market},
		&newsTool{market: market},
		&socialTool{market: market},
	}
	reg := &StockToolRegistry{tools: make(map[value_objects.ToolName]Tool, len(tools))}
	for _, t := range tools {
		reg.tools[t.Spec().Name] = t
	}
	return reg
}

func (r *StockToolRegistry) Lookup(name value_objects.ToolName) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Specs 返回授权范围内的工具声明。
func (r *StockToolRegistry) Specs(access value_objects.DataAccess) []value_objects.ToolSpec {
	all := make([]value_objects.ToolSpec, 0, len(r.tools))
	for _, name := range value_objects.AllToolNames() {
		if t, ok := r.tools[name]; ok {
			all = append(all, t.Spec())
		}
	}
	return value_objects.SpecsOf(all, access)
}

// ---------------------------------------------------------------------------
// 入参解析
//
// 统一走一个宽松结构体：模型给的参数经常多字段、错类型、或者干脆是空对象。
// 解析失败不报错而是退回默认值——为了一个多余的字段让工具调用失败，
// 只会让模型重试一遍同样的错误，白白烧掉一轮。
// ---------------------------------------------------------------------------

type toolArgs struct {
	Period string `json:"period"`
	Limit  int    `json:"limit"`
	Days   int    `json:"days"`
}

func parseArgs(raw json.RawMessage) toolArgs {
	var a toolArgs
	if len(raw) == 0 {
		return a
	}
	_ = json.Unmarshal(raw, &a)
	return a
}

func clampInt(v, def, max int) int {
	switch {
	case v <= 0:
		return def
	case v > max:
		return max
	default:
		return v
	}
}

// lookbackRange 构造「截至分析交易日往前 n 天」的区间。
//
// 右端一律锁在分析交易日，不用今天：回测 2024-03-01 的决策时，
// 让工具取回 3 月 5 日的新闻就是未来函数，得出的结论准得可疑却毫无意义。
func lookbackRange(tradeDate shared_vo.TradeDate, days int) shared_vo.DateRange {
	end := tradeDate.OrToday()
	return shared_vo.DateRange{Start: end.AddDays(-clampInt(days, defaultLookbackDay, maxLookbackDay)), End: end}
}

func parsePeriod(s string) stock_vo.Period {
	p, err := stock_vo.NewPeriod(s)
	if err != nil {
		// 模型写了个不认识的周期，退回日线而不是失败：
		// 日线是绝大多数场景下的正确答案。
		return stock_vo.PeriodDaily
	}
	return p.OrDaily()
}

// ---------------------------------------------------------------------------
// get_quote
// ---------------------------------------------------------------------------

type quoteTool struct{ market MarketReader }

func (t *quoteTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetQuote,
		"获取当前分析标的的最新行情快照，包含开高低收、涨跌幅、成交量额、换手率与 PE/PB。无需参数。",
		value_objects.NewParamSchema(nil),
	)
}

func (t *quoteTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.market == nil {
		return "", custom_errors.Unavailable("行情服务不可用")
	}
	q, err := t.market.LatestQuote(ctx, in.Code)
	if err != nil {
		return "", err
	}
	return formatQuote(*q), nil
}

// ---------------------------------------------------------------------------
// get_klines
// ---------------------------------------------------------------------------

type klineTool struct{ market MarketReader }

func (t *klineTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetKlines,
		"获取当前分析标的的历史 K 线序列（按交易日倒序）。用于观察价格走势与量能变化。",
		value_objects.NewParamSchema(map[string]value_objects.ParamField{
			"period": {
				Type:        "string",
				Description: "K 线周期，默认 daily",
				Enum:        []string{"daily", "weekly", "monthly"},
				Default:     "daily",
			},
			"limit": {
				Type:        "integer",
				Description: fmt.Sprintf("返回条数，默认 %d，最大 %d", defaultKlineLimit, maxKlineLimit),
				Default:     defaultKlineLimit,
			},
		}),
	)
}

func (t *klineTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.market == nil {
		return "", custom_errors.Unavailable("行情服务不可用")
	}
	args := parseArgs(in.Arguments)
	period := parsePeriod(args.Period)
	limit := clampInt(args.Limit, defaultKlineLimit, maxKlineLimit)

	// 区间右端锁在分析交易日，杜绝未来数据。
	rng := shared_vo.DateRange{End: in.TradeDate.OrToday()}
	klines, err := t.market.Klines(ctx, in.Code, period, rng, limit)
	if err != nil {
		return "", err
	}
	if len(klines) == 0 {
		return "该周期下没有可用的 K 线数据。", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s K 线（共 %d 根，按交易日倒序）\n", in.Code.FullSymbol(), period, len(klines))
	sb.WriteString("| 交易日 | 开 | 高 | 低 | 收 | 成交量 | 成交额 |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, k := range klines {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s | %s |\n",
			k.TradeDate.String(), k.Open.StringFixed(3), k.High.StringFixed(3),
			k.Low.StringFixed(3), k.Close.StringFixed(3),
			formatAmount(k.Volume), formatAmount(k.Amount))
	}
	if !klines[0].Adjusted {
		sb.WriteString("提示: 该序列未做复权处理，跨除权日比较价格时请注意。\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// ---------------------------------------------------------------------------
// get_technical_indicators
// ---------------------------------------------------------------------------

// indicatorTool 是纯读工具。
//
// 它不持有 MarketReader，也不引用 ComputeIndicators——从类型层面就没有重算的可能。
// 这是「派生量必须落库再读回」这条规则在代码结构上的体现：
// 想在读路径上重算，得先给这个结构体加一个依赖，那种改动在评审里藏不住。
type indicatorTool struct {
	indicators *repositories.IndicatorRepository
}

func (t *indicatorTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetTechnicalIndics,
		"获取当前分析标的在分析交易日的技术指标快照（MA/EMA/MACD/RSI/BOLL/ATR/量能均线）。"+
			"这些指标在数据准备阶段统一计算并落库，本工具读取的是那一份快照。",
		value_objects.NewParamSchema(map[string]value_objects.ParamField{
			"period": {
				Type:        "string",
				Description: "指标所基于的 K 线周期，默认 daily",
				Enum:        []string{"daily", "weekly", "monthly"},
				Default:     "daily",
			},
		}),
	)
}

func (t *indicatorTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.indicators == nil {
		return "", custom_errors.Unavailable("技术指标服务不可用")
	}
	period := parsePeriod(parseArgs(in.Arguments).Period)

	snapshot, err := t.indicators.Find(ctx, in.Code, period, in.TradeDate)
	if err != nil {
		if custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
			return "", err
		}
		// 分析交易日可能是非交易日（周末下单分析），退到不晚于它的最近一条。
		// 这仍然是纯读路径：退的是「读哪一条」，不是「要不要重新算一条」。
		snapshot, err = t.indicators.LatestNotAfter(ctx, in.Code, period, in.TradeDate)
		if err != nil {
			return "", err
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s 技术指标（交易日 %s，计算于 %s）\n",
		in.Code.FullSymbol(), period, snapshot.TradeDate.String(),
		snapshot.ComputedAt.Format("2006-01-02 15:04:05"))
	sb.WriteString(formatIndicators(snapshot.Indicators))
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// get_financials
// ---------------------------------------------------------------------------

type financialTool struct{ market MarketReader }

func (t *financialTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetFinancials,
		"获取当前分析标的的历史财务数据（营收、净利润、EPS、ROE、毛利率、净利率、资产负债率），按报告期倒序。",
		value_objects.NewParamSchema(map[string]value_objects.ParamField{
			"limit": {
				Type:        "integer",
				Description: fmt.Sprintf("返回期数，默认 %d，最大 %d", defaultFinLimit, maxFinLimit),
				Default:     defaultFinLimit,
			},
		}),
	)
}

func (t *financialTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.market == nil {
		return "", custom_errors.Unavailable("财务数据服务不可用")
	}
	limit := clampInt(parseArgs(in.Arguments).Limit, defaultFinLimit, maxFinLimit)
	items, err := t.market.Financials(ctx, in.Code, limit)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "没有可用的财务数据。", nil
	}
	return formatFinancials(items), nil
}

// ---------------------------------------------------------------------------
// get_news
// ---------------------------------------------------------------------------

type newsTool struct{ market MarketReader }

func (t *newsTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetNews,
		"获取当前分析标的在分析交易日之前一段时间内的相关资讯（标题、摘要、来源、情感分）。",
		value_objects.NewParamSchema(map[string]value_objects.ParamField{
			"days": {
				Type:        "integer",
				Description: fmt.Sprintf("回看天数，默认 %d，最大 %d", defaultLookbackDay, maxLookbackDay),
				Default:     defaultLookbackDay,
			},
			"limit": {
				Type:        "integer",
				Description: fmt.Sprintf("返回条数，默认 %d，最大 %d", defaultNewsLimit, maxNewsLimit),
				Default:     defaultNewsLimit,
			},
		}),
	)
}

func (t *newsTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.market == nil {
		return "", custom_errors.Unavailable("资讯服务不可用")
	}
	args := parseArgs(in.Arguments)
	items, err := t.market.News(ctx, in.Code, lookbackRange(in.TradeDate, args.Days),
		clampInt(args.Limit, defaultNewsLimit, maxNewsLimit))
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "该时间窗内没有相关资讯。", nil
	}
	return formatNews(items), nil
}

// ---------------------------------------------------------------------------
// get_social_sentiment
// ---------------------------------------------------------------------------

type socialTool struct{ market MarketReader }

func (t *socialTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(
		value_objects.ToolGetSocialSentiment,
		"获取当前分析标的在分析交易日之前一段时间内的社交媒体舆情样本与情绪分布。",
		value_objects.NewParamSchema(map[string]value_objects.ParamField{
			"days": {
				Type:        "integer",
				Description: fmt.Sprintf("回看天数，默认 %d，最大 %d", defaultLookbackDay, maxLookbackDay),
				Default:     defaultLookbackDay,
			},
			"limit": {
				Type:        "integer",
				Description: fmt.Sprintf("返回条数，默认 %d，最大 %d", defaultSocialLimit, maxSocialLimit),
				Default:     defaultSocialLimit,
			},
		}),
	)
}

func (t *socialTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if t.market == nil {
		return "", custom_errors.Unavailable("舆情服务不可用")
	}
	args := parseArgs(in.Arguments)
	items, err := t.market.SocialPosts(ctx, in.Code, lookbackRange(in.TradeDate, args.Days),
		clampInt(args.Limit, defaultSocialLimit, maxSocialLimit))
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "该时间窗内没有社交舆情样本。", nil
	}
	return formatSocial(items), nil
}
