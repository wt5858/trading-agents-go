// Package entities 承载报告上下文的全部业务不变式。
//
// 本包不感知 HTTP、数据库与事务——那些分别属于 application/ 与 repositories/。
// 本上下文只有一个聚合根：Report。章节（Section）是值对象而不是子实体，
// 它没有独立的生命周期，也不该有自己的仓储：一个脱离报告单独存在的章节没有意义。
package entities

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Report 是报告上下文的聚合根：一次分析的可交付成果。
//
// # 为什么 TaskID 是 string
//
// Task 是另一个上下文的聚合根。跨聚合只按标识引用，不持有实体指针：
// 持有指针意味着每打开一份报告都要把整个分析任务（含全部原始产出）加载进内存，
// 也意味着报告会跟着任务的后续状态变化——而报告是一份已交付的快照，它不该变。
//
// # 为什么 Symbol 是 StockCode 而不是 Symbol + Market 两个字段
//
// StockCode 自带 Market。拆成两个裸字段就没有任何地方能保证它们是配套的，
// 一次赋值漏改就会产出「市场是美股、代码是 600519」这种谁也发现不了的脏数据。
//
// # 为什么 Confidence / RiskScore 这些数字存在这里
//
// 它们是分析决策的既成事实，报告生成那一刻抄录下来，读路径永不重算。
// 报告是快照：同一份报告在任何时刻、任何代码版本下都必须给出同样的数字。
type Report struct {
	domain_event.EventRecorder

	ID     string
	UserID uint64
	// TaskID 是触发本报告的分析任务标识，同时也是本聚合的业务唯一键：
	// 一个任务只该有一份报告，这条不变式由 reports.task_id 的唯一索引兜底。
	TaskID string

	Symbol    shared_vo.StockCode
	TradeDate shared_vo.TradeDate

	Title    string
	Summary  string
	Sections []value_objects.Section

	// 下面这组是从分析决策原样抄录的终局数字。它们在生成时固化，
	// 任何读路径都不得再按其它字段推导——见类型注释。
	Action      analysis_vo.Action
	Confidence  decimal.Decimal // 0-1
	RiskScore   decimal.Decimal // 0-10，越高越危险
	TargetPrice decimal.Decimal
	StopLoss    decimal.Decimal
	Position    decimal.Decimal // 建议仓位百分比 0-100

	CreatedAt time.Time
}

// FromAnalysisResult 把一次分析产出投影成报告。这是报告进入系统的唯一入口。
//
// # 为什么投影写在聚合里而不是写在领域服务里
//
// 「一份报告由哪些章节构成、按什么顺序排、结论段怎么措辞」是报告这个概念的
// 业务规则本身，不是编排。放进服务意味着任何一条新的生成路径（补偿任务、
// 历史数据回填、后台重建）都要把同一套规则再抄一遍，而抄漏一次的结果是
// 两份来源不同、排版不一致的报告同时存在于系统里。
//
// 入参收的是 analysis 上下文的 Result 值对象而不是 Task 实体：
// 值对象是一份不可变的数据快照，跨上下文传它是安全的；传实体则会把
// 两个上下文的生命周期焊死。
func FromAnalysisResult(id string, userID uint64, taskID string, result analysis_vo.Result) (*Report, error) {
	if id == "" {
		return nil, custom_errors.Invalid("报告 ID 不能为空")
	}
	if userID == 0 {
		return nil, custom_errors.Invalid("报告必须归属于一个用户")
	}
	if taskID == "" {
		// 没有任务来源的报告无法去重，也无法追溯结论是怎么来的。
		return nil, custom_errors.Invalid("报告必须关联一个分析任务")
	}
	if result.Code.IsZero() {
		return nil, custom_errors.Invalid("分析结果不完整：缺少股票代码，无法生成报告")
	}

	decision := result.Decision
	tradeDate := result.TradeDate

	// 章节内容先收成一张 key -> 正文 的表，再交给排序表投影。
	// 这样「有哪些内容」和「按什么顺序排」是两件分开的事：
	// 前者随分析深度变化，后者是固定契约。
	contents := make(map[value_objects.SectionKey]string, len(result.Reports)+1)
	for agentID, content := range result.Reports {
		contents[value_objects.SectionKey(agentID)] = strings.TrimSpace(content)
	}
	// 结论段是唯一一段不来自任何智能体的内容，由聚合自己合成。
	contents[value_objects.SectionConclusion] = renderConclusion(decision)

	r := &Report{
		ID:        id,
		UserID:    userID,
		TaskID:    taskID,
		Symbol:    result.Code,
		TradeDate: tradeDate,
		Title:     renderTitle(result.Code, tradeDate),
		Summary:   strings.TrimSpace(decision.Summary),
		Sections:  value_objects.OrderSections(contents),

		// 逐字抄录，不做任何再加工（连夹取都不做）：分析上下文已经在
		// Decision.Normalized() 里收敛过一次，这里再收敛一次只会制造
		// 「报告里的数字和分析结果里的数字对不上」这种无从排查的差异。
		Action:      decision.Action,
		Confidence:  decision.Confidence,
		RiskScore:   decision.RiskScore,
		TargetPrice: decision.TargetPrice,
		StopLoss:    decision.StopLoss,
		Position:    decision.Position,

		CreatedAt: time.Now(),
	}

	r.AddDomainEvent(domain_events.NewOnReportGenerated(
		r.ID, r.TaskID, r.UserID, r.Symbol.FullSymbol(),
		r.Action.String(), r.Confidence))
	return r, nil
}

// OwnedBy 判断报告归属，供 domain_services/ 做越权校验。
func (r *Report) OwnedBy(userID uint64) bool { return r.UserID == userID }

// SectionOf 取某一章节。
//
// 不存在返回 NotFound 而不是空章节：请求一个不存在的章节 key 是调用方拼错了路径，
// 静默返回空白会让前端以为「这一节确实没内容」，把一个拼写错误伪装成分析结果。
func (r *Report) SectionOf(key value_objects.SectionKey) (value_objects.Section, error) {
	if key.IsZero() {
		return value_objects.Section{}, custom_errors.Invalid("章节标识不能为空")
	}
	if s, ok := value_objects.FindSection(r.Sections, key); ok {
		return s, nil
	}
	return value_objects.Section{}, custom_errors.NotFound(
		"分析报告(id=%s) 不存在章节 %s", r.ID, key.String())
}

// renderTitle 生成报告标题。未指定交易日时退化为只带代码的标题，
// 而不是拼出一个「600519.SH  分析报告」这样带空洞的字符串。
func renderTitle(code shared_vo.StockCode, date shared_vo.TradeDate) string {
	if date.IsZero() {
		return fmt.Sprintf("%s 分析报告", code.FullSymbol())
	}
	return fmt.Sprintf("%s %s 分析报告", code.FullSymbol(), date.String())
}

// renderConclusion 把结构化决策渲染成结论段正文。
//
// 这段文字在生成时一次性拼好并随报告落库，读路径绝不重拼：
// 一旦改成读的时候按 Confidence 现算措辞，同一份报告在代码升级后就会
// 换一种说法，而用户手里可能正拿着上一版的截图。
//
// 百分比这类乘除同理——算一次，固化进正文。
func renderConclusion(d analysis_vo.Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**操作建议：%s**\n\n", d.Action.DisplayName())
	fmt.Fprintf(&b, "- 置信度：%s%%\n", d.Confidence.Mul(decimal.NewFromInt(100)).StringFixed(0))
	fmt.Fprintf(&b, "- 风险评分：%s / 10\n", d.RiskScore.StringFixed(1))
	if d.TargetPrice.IsPositive() {
		fmt.Fprintf(&b, "- 目标价：%s\n", d.TargetPrice.StringFixed(2))
	}
	if d.StopLoss.IsPositive() {
		fmt.Fprintf(&b, "- 止损价：%s\n", d.StopLoss.StringFixed(2))
	}
	if d.Position.IsPositive() {
		fmt.Fprintf(&b, "- 建议仓位：%s%%\n", d.Position.StringFixed(0))
	}
	if s := strings.TrimSpace(d.Summary); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}
	if reason := strings.TrimSpace(d.Reasoning); reason != "" {
		fmt.Fprintf(&b, "\n**决策依据**\n\n%s\n", reason)
	}
	return b.String()
}
