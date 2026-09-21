package value_objects

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// Action 是最终交易建议值对象。
type Action string

const (
	ActionBuy       Action = "buy"
	ActionHold      Action = "hold"
	ActionSell      Action = "sell"
	ActionReduce    Action = "reduce"
	ActionIncrease  Action = "increase"
	ActionUndecided Action = "undecided"
)

// ParseAction 从 LLM 自由文本中提取交易动作，无法识别时返回 undecided。
//
// 这里不返回 error：模型输出天然不可控，把「没说清楚」当成错误会让
// 一次分析因为措辞问题整体失败；undecided 是一个诚实且可展示的结论。
func ParseAction(s string) Action {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "买入"), strings.Contains(l, "buy"):
		return ActionBuy
	case strings.Contains(l, "加仓"), strings.Contains(l, "increase"):
		return ActionIncrease
	case strings.Contains(l, "减仓"), strings.Contains(l, "reduce"):
		return ActionReduce
	case strings.Contains(l, "卖出"), strings.Contains(l, "清仓"), strings.Contains(l, "sell"):
		return ActionSell
	case strings.Contains(l, "持有"), strings.Contains(l, "观望"), strings.Contains(l, "hold"):
		return ActionHold
	}
	return ActionUndecided
}

func (a Action) Valid() bool {
	switch a {
	case ActionBuy, ActionHold, ActionSell, ActionReduce, ActionIncrease, ActionUndecided:
		return true
	}
	return false
}

func (a Action) String() string { return string(a) }

func (a Action) DisplayName() string {
	switch a {
	case ActionBuy:
		return "买入"
	case ActionIncrease:
		return "加仓"
	case ActionHold:
		return "持有"
	case ActionReduce:
		return "减仓"
	case ActionSell:
		return "卖出"
	}
	return "待定"
}

// Decision 是结构化的终局决策值对象。
//
// 各数值字段是 decimal 而不是 float64。它们由 LLM 以 JSON 数字返回，
// decimal 的 UnmarshalJSON 同时接受带引号与不带引号的数字，所以解析侧无需改动；
// 而 TargetPrice / StopLoss 是会被用户拿去下单的价格，
// 一个 12.340000000000001 的目标价出现在报告里，没有任何解释得通的理由。
type Decision struct {
	Action      Action          `json:"action"`
	Confidence  decimal.Decimal `json:"confidence"` // 0-1
	RiskScore   decimal.Decimal `json:"risk_score"` // 0-10，越高越危险
	TargetPrice decimal.Decimal `json:"target_price"`
	StopLoss    decimal.Decimal `json:"stop_loss"`
	Position    decimal.Decimal `json:"position"` // 建议仓位百分比 0-100
	Reasoning   string          `json:"reasoning"`
	Summary     string          `json:"summary"`
}

// Normalized 返回把各项指标夹回合法区间后的新决策。
//
// 夹取而非报错：这些数字来自 LLM 的结构化输出，越界是常态而非异常。
// 收敛在 VO 里意味着任何持有 Decision 的代码都不必再自己判边界。
func (d Decision) Normalized() Decision {
	out := d
	if !out.Action.Valid() {
		out.Action = ActionUndecided
	}
	out.Confidence = clamp(out.Confidence, decimal.Zero, decimal.NewFromInt(1))
	out.RiskScore = clamp(out.RiskScore, decimal.Zero, decimal.NewFromInt(10))
	out.Position = clamp(out.Position, decimal.Zero, decimal.NewFromInt(100))
	if out.TargetPrice.IsNegative() {
		out.TargetPrice = decimal.Zero
	}
	if out.StopLoss.IsNegative() {
		out.StopLoss = decimal.Zero
	}
	return out
}

// PhaseOutcome 记录每个阶段的执行情况，便于回溯与计费归因。
type PhaseOutcome struct {
	Phase     string          `json:"phase"`
	Agents    []string        `json:"agents"`
	DurationS decimal.Decimal `json:"duration_seconds"`
	Failed    []string        `json:"failed,omitempty"`
}

// TokenUsage 汇总一次分析的 LLM 消耗。
type TokenUsage struct {
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	TotalTokens      int             `json:"total_tokens"`
	Calls            int             `json:"calls"`
	CostUSD          decimal.Decimal `json:"cost_usd"`
}

// Plus 返回两次消耗相加后的新值对象。
// 不做原地累加：那会让同一个 usage 在多个阶段间共享时被意外改写；
// VO 不可变，因此返回新值。
func (u TokenUsage) Plus(other TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
		Calls:            u.Calls + other.Calls,
		CostUSD:          u.CostUSD.Add(other.CostUSD),
	}
}

// Result 是一次完整分析的产出值对象：各智能体报告 + 终局决策 + 成本。
//
// Code 用 shared_vo.StockCode 而非 Symbol/Market 两个裸 string：
// 这两个字段永远一起出现、且必须一起满足「代码符合该市场的格式」这条约束，
// 拆成两个 string 就没有任何地方能保证它们是配套的。
type Result struct {
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Decision  Decision
	Reports   map[string]string // agentID -> markdown 报告
	Usage     TokenUsage
	Phases    []PhaseOutcome
	CreatedAt time.Time
}

// NewResult 构造分析产出，顺带把决策收敛到合法区间。
func NewResult(code shared_vo.StockCode, tradeDate shared_vo.TradeDate, decision Decision,
	reports map[string]string, usage TokenUsage, phases []PhaseOutcome) Result {
	cloned := make(map[string]string, len(reports))
	for k, v := range reports {
		cloned[k] = v
	}
	return Result{
		Code:      code,
		TradeDate: tradeDate,
		Decision:  decision.Normalized(),
		Reports:   cloned,
		Usage:     usage,
		Phases:    append([]PhaseOutcome(nil), phases...),
		CreatedAt: time.Now(),
	}
}

func (r Result) IsZero() bool { return r.Code.IsZero() && r.CreatedAt.IsZero() }

// ReportOf 取某个智能体的报告，不存在时返回空串。
func (r Result) ReportOf(agentID string) string { return r.Reports[agentID] }

func clamp(v, lo, hi decimal.Decimal) decimal.Decimal {
	if v.LessThan(lo) {
		return lo
	}
	if v.GreaterThan(hi) {
		return hi
	}
	return v
}
