package domain_services

import (
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// PromptService 负责把「一位成员 + 一份上下文快照」渲染成模型消息。
//
// # 为什么是模板而不是字符串拼接
//
// 十四位成员的提示词有大量共享部分（标的信息、数据纪律、输出格式要求）。
// 字符串拼接的写法下，这些共享部分会被复制十四份，
// 而「不许编造数据」这条纪律一旦要改，就得改十四处且必然漏掉一两处。
// text/template 的 define/template 机制让共享块只有一份。
//
// # 为什么模板在代码里而不是在文件里
//
// 提示词是业务逻辑，不是配置：改一个字就可能让风控经理的结构化块解析不出来。
// 放进代码意味着它跟着版本走、跟着测试走，也不会出现「容器里少挂了个目录
// 导致所有分析返回空提示词」这种故障。
type PromptService struct {
	tpl *template.Template
}

// NewPromptService 解析模板。解析失败直接 panic：
// 模板是硬编码常量，解析不过只可能是有人改坏了代码，
// 让它在进程启动的第一秒炸掉，远好过在某次分析里返回半截提示词。
func NewPromptService() *PromptService {
	return &PromptService{tpl: template.Must(template.New("agent").Parse(promptTemplates))}
}

// Render 渲染出一位成员的 system + user 消息。
//
// system 承载稳定的身份与纪律，user 承载本次的数据与任务。
// 这样切分不只是习惯：system 部分在同一次分析的十四次调用里高度重复，
// 各家厂商的前缀缓存正是按这种结构生效的，切碎了会让缓存命中率归零。
func (s *PromptService) Render(turn entities.Turn) ([]value_objects.Message, error) {
	kind := turn.Contract.Kind
	if !kind.Valid() {
		return nil, custom_errors.Invalid("未知的智能体标识: %s", kind)
	}
	view := newPromptView(turn)

	system, err := s.render(kind.String()+".system", view)
	if err != nil {
		return nil, err
	}
	user, err := s.render(kind.String()+".user", view)
	if err != nil {
		return nil, err
	}
	return []value_objects.Message{
		value_objects.SystemMessage(system),
		value_objects.UserMessage(user),
	}, nil
}

func (s *PromptService) render(name string, view promptView) (string, error) {
	var sb strings.Builder
	t := s.tpl.Lookup(name)
	if t == nil {
		return "", custom_errors.Internal("缺少提示词模板: %s", name)
	}
	if err := t.Execute(&sb, view); err != nil {
		return "", custom_errors.Internal("渲染提示词失败: %s", name).Wrap(err)
	}
	return strings.TrimSpace(sb.String()), nil
}

// ---------------------------------------------------------------------------
// 模板视图
// ---------------------------------------------------------------------------

// promptView 是模板的唯一数据源。
//
// 所有数据段（行情、指标、财务、资讯、舆情）在这里就被格式化成文本，
// 而不是把值对象丢进模板里让模板去取字段：模板里写不出「样本不足时显示数据不足」
// 这类判断，写出来也没法测试。格式化留在 Go 里，模板只负责排版。
type promptView struct {
	Agent     string
	Symbol    string
	Name      string
	Industry  string
	Market    string
	TradeDate string
	Depth     int

	Quote      string
	Indicators string
	Financials string
	News       string
	Social     string
	Missing    string
	Tools      string

	// DecisionBlock 是风控经理必须照抄的结构化块模板。
	// 它和解析器共用同一个常量，不会出现「提示词改了解析器没改」的脱节。
	DecisionBlock string

	reports  map[string]string
	failures map[string]string
}

func newPromptView(turn entities.Turn) promptView {
	s := turn.Snapshot
	reports := make(map[string]string, len(s.Reports))
	for k, v := range s.Reports {
		reports[k.String()] = v
	}
	failures := make(map[string]string, len(s.Failures))
	for k, v := range s.Failures {
		failures[k.String()] = v
	}

	return promptView{
		Agent:         turn.Contract.DisplayName,
		Symbol:        s.Code.FullSymbol(),
		Name:          orPlaceholder(s.Market.Name, "未知名称"),
		Industry:      orPlaceholder(s.Market.Industry, "未知行业"),
		Market:        s.Code.Market.DisplayName(),
		TradeDate:     s.TradeDate.String(),
		Depth:         s.Depth.Int(),
		Quote:         formatQuote(s.Market.Quote),
		Indicators:    formatIndicators(s.Market.Indicators),
		Financials:    formatFinancials(s.Market.Financials),
		News:          formatNews(s.Market.News),
		Social:        formatSocial(s.Market.Social),
		Missing:       formatMissing(s.Market.Missing),
		Tools:         formatTools(turn.Contract.Access),
		DecisionBlock: value_objects.DecisionBlockTemplate,
		reports:       reports,
		failures:      failures,
	}
}

// Report 取某位成员的报告，缺席时返回一句明确的缺席说明。
//
// 返回「（缺席）」而不是空串是关键：空串会让模板渲染出一个空白小节，
// 模型看到空白会自行脑补内容，而看到「情绪分析师缺席」则会如实地
// 在结论里降低情绪面的权重。
func (v promptView) Report(kind string) string {
	if r := strings.TrimSpace(v.reports[kind]); r != "" {
		return r
	}
	if reason := strings.TrimSpace(v.failures[kind]); reason != "" {
		return fmt.Sprintf("（本次缺席：%s）", reason)
	}
	return "（本次缺席：未产出报告）"
}

// Has 判定某位成员是否有报告，供模板决定要不要渲染整个小节。
func (v promptView) Has(kind string) bool { return strings.TrimSpace(v.reports[kind]) != "" }

// AnalystReports 汇总全部分析师报告，按固定顺序排版。
// 顺序固定是为了让同一次分析的提示词可复现——顺序抖动会让厂商的前缀缓存失效。
func (v promptView) AnalystReports() string {
	var sb strings.Builder
	for _, k := range value_objects.AnalystKinds() {
		if !v.Has(k.String()) {
			continue
		}
		sb.WriteString("### ")
		sb.WriteString(k.DisplayName())
		sb.WriteString("\n")
		sb.WriteString(v.reports[k.String()])
		sb.WriteString("\n\n")
	}
	if sb.Len() == 0 {
		return "（全部分析师均未产出报告）"
	}
	return strings.TrimSpace(sb.String())
}

// AbsentAnalysts 列出缺席的分析师，让下游智能体知道自己看到的是一份残缺的证据链。
func (v promptView) AbsentAnalysts() string {
	var missing []string
	for _, k := range value_objects.AnalystKinds() {
		if v.Has(k.String()) {
			continue
		}
		if reason, failed := v.failures[k.String()]; failed {
			missing = append(missing, fmt.Sprintf("%s（%s）", k.DisplayName(), reason))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return strings.Join(missing, "、")
}

// RiskDebate 汇总三位风控辩手的发言。
func (v promptView) RiskDebate() string {
	order := []value_objects.AgentKind{
		value_objects.KindRiskAggressive,
		value_objects.KindRiskConservative,
		value_objects.KindRiskNeutral,
	}
	var sb strings.Builder
	for _, k := range order {
		if !v.Has(k.String()) {
			continue
		}
		sb.WriteString("### ")
		sb.WriteString(k.DisplayName())
		sb.WriteString("\n")
		sb.WriteString(v.reports[k.String()])
		sb.WriteString("\n\n")
	}
	if sb.Len() == 0 {
		return "（三位风控辩手均未产出意见，请仅依据交易方案与行情数据独立终裁）"
	}
	return strings.TrimSpace(sb.String())
}

// ---------------------------------------------------------------------------
// 数据段格式化
//
// 这几个函数把值对象渲染成模型好读的文本。共同的规矩是：
// 没有数据就明说「暂无数据」，绝不用 0 冒充——模型分不清
// 「成交额 0」和「没取到成交额」，但它会把 0 当成事实用下去。
// ---------------------------------------------------------------------------

func formatQuote(q stock_vo.Quote) string {
	if q.Code.IsZero() {
		return "暂无行情数据。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "交易日: %s\n", q.TradeDate.String())
	fmt.Fprintf(&sb, "收盘: %s  开盘: %s  最高: %s  最低: %s  昨收: %s\n",
		q.Close.StringFixed(3), q.Open.StringFixed(3), q.High.StringFixed(3),
		q.Low.StringFixed(3), q.PreClose.StringFixed(3))
	// 涨跌额与涨跌幅读的是数据源落库的值，不用 Close-PreClose 现推：
	// 除权日、停牌复牌等场景下两者并不相等，以数据源口径为准。
	fmt.Fprintf(&sb, "涨跌: %s (%s%%)\n", signed(q.Change, 3), signed(q.ChangePct, 2))
	fmt.Fprintf(&sb, "成交量: %s  成交额: %s  换手率: %s%%\n",
		formatAmount(q.Volume), formatAmount(q.Amount), q.Turnover.StringFixed(2))
	fmt.Fprintf(&sb, "市盈率(PE): %s  市净率(PB): %s", formatRatio(q.PE), formatRatio(q.PB))
	if q.IsLimitUp() {
		sb.WriteString("\n注意: 该标的当日接近或触及涨停。")
	}
	return sb.String()
}

// formatIndicators 渲染技术指标。
//
// 这里读的是从 IndicatorRepository 取回的快照，本函数不做任何计算。
// 这条约束是刻意的：指标是乘除派生量，读路径重算会让报告里的数字
// 和图表上的数字对不上，也让同一次任务的重跑得出不同结论。
func formatIndicators(in value_objects.Indicators) string {
	if in.IsZero() {
		return "暂无技术指标数据（K 线不足或尚未计算）。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "样本根数: %d\n", in.Samples)
	fmt.Fprintf(&sb, "均线: MA5=%s MA10=%s MA20=%s MA60=%s（%s）\n",
		formatPrice(in.MA5), formatPrice(in.MA10), formatPrice(in.MA20), formatPrice(in.MA60),
		in.TrendState())
	fmt.Fprintf(&sb, "MA20 乖离率: %s%%\n", signed(in.DeviationMA20Pct, 2))
	if in.HasMACD() {
		fmt.Fprintf(&sb, "MACD: DIF=%s DEA=%s 柱=%s（%s）\n",
			in.MACDDIF.StringFixed(4), in.MACDDEA.StringFixed(4),
			in.MACDHist.StringFixed(4), in.MACDSignal())
	} else {
		sb.WriteString("MACD: 样本不足\n")
	}
	fmt.Fprintf(&sb, "RSI: RSI6=%s RSI14=%s（%s）\n",
		formatRatio(in.RSI6), formatRatio(in.RSI14), in.RSIState())
	if in.HasMA20() {
		fmt.Fprintf(&sb, "布林带: 上轨=%s 中轨=%s 下轨=%s（%s）\n",
			formatPrice(in.BollUpper), formatPrice(in.BollMid), formatPrice(in.BollLower),
			in.BollPosition())
	} else {
		sb.WriteString("布林带: 样本不足\n")
	}
	if in.HasATR() {
		fmt.Fprintf(&sb, "ATR14: %s（用于设定止损幅度）\n", formatPrice(in.ATR14))
	}
	fmt.Fprintf(&sb, "量能: 5日均量=%s 20日均量=%s", formatAmount(in.VolMA5), formatAmount(in.VolMA20))
	return sb.String()
}

func formatFinancials(items []stock_vo.Financial) string {
	if len(items) == 0 {
		return "暂无财务数据。"
	}
	// 按报告期倒序，最近的排前面。
	sorted := append([]stock_vo.Financial(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[j].ReportDate.Before(sorted[i].ReportDate)
	})
	if len(sorted) > 8 {
		sorted = sorted[:8]
	}

	var sb strings.Builder
	sb.WriteString("| 报告期 | 口径 | 营收 | 净利润 | EPS | ROE% | 毛利率% | 净利率% | 资产负债率% |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, f := range sorted {
		// 净利率读的是落库的 NetMargin，不用 NetProfit/Revenue 现算：
		// 部分数据源的绝对营收字段本就为空，现算只会得到 0 这个假值。
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			f.ReportDate.String(), f.PeriodType.String(),
			formatAmount(f.Revenue), formatAmount(f.NetProfit), formatRatio(f.EPS),
			formatRatio(f.ROE), formatRatio(f.GrossMargin), formatRatio(f.NetMargin),
			formatRatio(f.DebtRatio))
	}
	return strings.TrimSpace(sb.String())
}

func formatNews(items []stock_vo.News) string {
	if len(items) == 0 {
		return "暂无相关资讯。"
	}
	if len(items) > 15 {
		items = items[:15]
	}
	var sb strings.Builder
	for i, n := range items {
		fmt.Fprintf(&sb, "%d. [%s] %s（来源: %s，情感分 %s）\n",
			i+1, n.PublishedAt.Format("2006-01-02 15:04"), strings.TrimSpace(n.Title),
			orPlaceholder(n.Source, "未知"), n.Sentiment.Value().StringFixed(2))
		if c := strings.TrimSpace(n.Content); c != "" {
			fmt.Fprintf(&sb, "   摘要: %s\n", truncate(c, 180))
		}
	}
	return strings.TrimSpace(sb.String())
}

func formatSocial(items []stock_vo.SocialPost) string {
	if len(items) == 0 {
		return "暂无社交舆情数据。"
	}
	if len(items) > 20 {
		items = items[:20]
	}
	// 情绪分布是计数统计，不是乘除派生量，可以在这里就地汇总。
	var pos, neg, neutral int
	var sb strings.Builder
	for i, p := range items {
		switch {
		case p.Sentiment.IsPositive():
			pos++
		case p.Sentiment.IsNegative():
			neg++
		default:
			neutral++
		}
		fmt.Fprintf(&sb, "%d. [%s|%s] %s（情感分 %s，互动 %d）\n",
			i+1, p.Platform, p.PublishedAt.Format("01-02 15:04"),
			truncate(strings.TrimSpace(p.Content), 120),
			p.Sentiment.Value().StringFixed(2), p.Engagement)
	}
	return fmt.Sprintf("样本 %d 条（正面 %d / 中性 %d / 负面 %d）\n%s",
		len(items), pos, neutral, neg, strings.TrimSpace(sb.String()))
}

func formatMissing(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return strings.Join(missing, "、")
}

func formatTools(access value_objects.DataAccess) string {
	if access.IsEmpty() {
		return ""
	}
	names := access.Names()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n.String())
	}
	sort.Strings(out)
	return strings.Join(out, "、")
}

// formatAmount 把大额数字折成中文单位，让模型少犯数量级错误。
// 这是展示层的换算，换算结果不会回流进任何计算或落库。
func formatAmount(v decimal.Decimal) string {
	switch av := v.Abs(); {
	case av.IsZero():
		return "暂无"
	case av.GreaterThanOrEqual(yi):
		return v.DivRound(yi, 4).StringFixed(2) + "亿"
	case av.GreaterThanOrEqual(wan):
		return v.DivRound(wan, 4).StringFixed(2) + "万"
	default:
		return v.StringFixed(2)
	}
}

func formatPrice(v decimal.Decimal) string {
	if v.IsZero() {
		return "暂无"
	}
	return v.StringFixed(3)
}

func formatRatio(v decimal.Decimal) string {
	if v.IsZero() {
		return "暂无"
	}
	return v.StringFixed(2)
}

// signed 渲染带正负号的数值。原先靠 fmt 的 %+.2f 动词，
// 而 decimal 不是 fmt 认识的数值类型——%+.2f 作用在结构体上会打印出
// {值 指数} 这种内部形态，编译期不报错，只在提示词里悄悄变成乱码。
func signed(d decimal.Decimal, scale int32) string {
	s := d.StringFixed(scale)
	if d.IsNegative() {
		return s
	}
	return "+" + s
}

// 单位换算常量。提到包级避免在渲染循环里反复构造。
var (
	wan = decimal.NewFromInt(10_000)
	yi  = decimal.NewFromInt(100_000_000)
)

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func orPlaceholder(s, placeholder string) string {
	if strings.TrimSpace(s) == "" {
		return placeholder
	}
	return s
}
