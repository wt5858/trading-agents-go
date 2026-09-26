package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// turnColumns 是一条发言轨迹的落库形态，内嵌在运行文档的 turns 数组里。
//
// 内嵌而不是单开一个集合：一条轨迹脱离它所属的那次运行没有任何意义，
// 而 Mongo 的单文档写入本身就是原子的——把子项内嵌进根文档，
// 「整个聚合一次写完，不会出现根写进去了子项没写进去」这条约束
// 由存储引擎直接保证，不需要事务，也不需要在仓储里拼差异更新。
type turnColumns struct {
	Seq   int    `bson:"seq"`
	Kind  string `bson:"kind"`
	Phase string `bson:"phase"`

	StartedAt time.Time `bson:"started_at"`
	// DurationMS 存毫秒整数而不是 time.Duration：后者是纳秒整数，
	// 落库之后在 mongo shell 或看板里就是一串没人读得懂的 19 位数字。
	DurationMS int64 `bson:"duration_ms"`

	Model        string `bson:"model"`
	PromptChars  int    `bson:"prompt_chars"`
	PromptDigest string `bson:"prompt_digest"`
	Content      string `bson:"content"`

	PromptTokens     int             `bson:"prompt_tokens"`
	CompletionTokens int             `bson:"completion_tokens"`
	TotalTokens      int             `bson:"total_tokens"`
	Calls            int             `bson:"calls"`
	CostUSD          decimal.Decimal `bson:"cost_usd"`

	ToolRounds int `bson:"tool_rounds"`
	// ToolCalls 是这次发言里每次工具调用的明细，同样内嵌，理由同 turns 本身。
	//
	// omitempty：缓存命中与零工具的发言占了轨迹的大头，
	// 给它们各存一个空数组会让每份运行文档白白多出十四个字段。
	ToolCalls []toolCallColumns `bson:"tool_calls,omitempty"`
	Truncated bool              `bson:"truncated"`
	// CacheHit 为真时上面那几列消耗全是 0，因为这次发言取自缓存。
	// 不存这一列的话，轨迹里会出现一条「零 token 却有完整报告」的记录，
	// 看起来像计费漏记。
	CacheHit bool `bson:"cache_hit"`

	Failed     bool   `bson:"failed"`
	FailReason string `bson:"fail_reason,omitempty"`
}

// toolCallColumns 是一次工具调用的落库形态。
//
// 不存参数原文：模型幻觉出的超长参数会把运行文档撑大，而排查要的是
// 「调了什么、成没成、多久」。真要复现入参，对话记录里有完整的 ToolCall。
type toolCallColumns struct {
	Round int    `bson:"round"`
	Name  string `bson:"name"`
	OK    bool   `bson:"ok"`
	// FailReason 只在失败时存在，因此 omitempty；OK 那一列才是统计口径，
	// 聚合失败率时数的是 ok:false，不是「这个字段非空」。
	FailReason  string `bson:"fail_reason,omitempty"`
	DurationMS  int64  `bson:"duration_ms"`
	ResultChars int    `bson:"result_chars"`
	Truncated   bool   `bson:"truncated"`
}

func turnColumnsOf(r value_objects.TurnRecord) turnColumns {
	return turnColumns{
		Seq:   r.Seq,
		Kind:  r.Kind.String(),
		Phase: r.Phase.String(),
		// BSON 的 datetime 精度到毫秒，先截断，免得写进去的时间与读回来的不相等。
		StartedAt:  r.StartedAt.Truncate(time.Millisecond),
		DurationMS: r.DurationMS(),

		Model:        r.Model,
		PromptChars:  r.PromptChars,
		PromptDigest: r.PromptDigest,
		Content:      r.Content,

		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
		Calls:            r.Usage.Calls,
		// 单次发言的成本是「token 数 × 单价」推出来的派生量，在模型客户端算过一次，
		// 这里原样存下。读路径（成本看板、按成员归因）读的就是这一列，
		// 绝不会拿 token 数再乘一遍单价——单价表会随厂商调价而变，
		// 重算出来的历史成本和当初真实花掉的钱不是一回事。
		CostUSD: r.Usage.CostUSD,

		ToolRounds: r.ToolRounds,
		ToolCalls:  toolCallColumnsOf(r.ToolCalls),
		Truncated:  r.Truncated,
		CacheHit:   r.CacheHit,

		Failed:     r.Failed,
		FailReason: r.FailReason,
	}
}

// toolCallColumnsOf 对空输入返回 nil 而不是空切片，配合 omitempty
// 让「没调过工具」这件事在文档里不占字段。
func toolCallColumnsOf(in []value_objects.ToolCallRecord) []toolCallColumns {
	if len(in) == 0 {
		return nil
	}
	out := make([]toolCallColumns, 0, len(in))
	for _, r := range in {
		out = append(out, toolCallColumns{
			Round:       r.Round,
			Name:        r.Name.String(),
			OK:          r.OK,
			FailReason:  r.FailReason,
			DurationMS:  r.DurationMS(),
			ResultChars: r.ResultChars,
			Truncated:   r.Truncated,
		})
	}
	return out
}

func (c turnColumns) toolCallsToDomain() []value_objects.ToolCallRecord {
	if len(c.ToolCalls) == 0 {
		return nil
	}
	out := make([]value_objects.ToolCallRecord, 0, len(c.ToolCalls))
	for _, t := range c.ToolCalls {
		out = append(out, value_objects.ToolCallRecord{
			Round: t.Round,
			// 直接转换而不走 NewToolName：读路径要能原样回放历史轨迹，
			// 包括那些工具后来被改名或下线的运行。在这里做校验，
			// 只会让老数据在看板上凭空消失。
			Name:        value_objects.ToolName(t.Name),
			OK:          t.OK,
			FailReason:  t.FailReason,
			Duration:    time.Duration(t.DurationMS) * time.Millisecond,
			ResultChars: t.ResultChars,
			Truncated:   t.Truncated,
		})
	}
	return out
}

func (c turnColumns) toDomain() value_objects.TurnRecord {
	return value_objects.TurnRecord{
		Seq:          c.Seq,
		Kind:         value_objects.AgentKind(c.Kind),
		Phase:        value_objects.Phase(c.Phase),
		StartedAt:    c.StartedAt,
		Duration:     time.Duration(c.DurationMS) * time.Millisecond,
		Model:        c.Model,
		PromptChars:  c.PromptChars,
		PromptDigest: c.PromptDigest,
		Content:      c.Content,
		Usage: value_objects.Usage{
			PromptTokens:     c.PromptTokens,
			CompletionTokens: c.CompletionTokens,
			TotalTokens:      c.TotalTokens,
			Calls:            c.Calls,
			CostUSD:          c.CostUSD,
		},
		ToolRounds: c.ToolRounds,
		ToolCalls:  c.toolCallsToDomain(),
		Truncated:  c.Truncated,
		CacheHit:   c.CacheHit,
		Failed:     c.Failed,
		FailReason: c.FailReason,
	}
}

// decisionColumns 是终局决策的落库形态。
//
// 只存决策本身，不存它派生出来的任何展示字段：目标价与止损价是会被用户拿去下单的数字，
// analysis 上下文的 analysis_reports 表已经存了一份，两边各存各的原值即可，
// 谁都不许从另一边的数字反推自己那一份。
type decisionColumns struct {
	// DecidedBy 是真正给出这个结论的成员，由聚合在收敛决策时一并定出。
	// 它随决策一起落库而不是在读路径上重新推导——推导版本会在
	// 「风控经理发了言但结构化块坏掉」时与结论分叉（见 SettledDecision 的说明）。
	DecidedBy   string          `bson:"decided_by,omitempty"`
	Action      string          `bson:"action"`
	Confidence  decimal.Decimal `bson:"confidence"`
	RiskScore   decimal.Decimal `bson:"risk_score"`
	TargetPrice decimal.Decimal `bson:"target_price"`
	StopLoss    decimal.Decimal `bson:"stop_loss"`
	Position    decimal.Decimal `bson:"position"`
	Reasoning   string          `bson:"reasoning,omitempty"`
	Summary     string          `bson:"summary,omitempty"`
}

func decisionColumnsOf(s value_objects.SettledDecision) decisionColumns {
	d := s.Decision
	return decisionColumns{
		DecidedBy:   s.DecidedBy.String(),
		Action:      d.Action.String(),
		Confidence:  d.Confidence,
		RiskScore:   d.RiskScore,
		TargetPrice: d.TargetPrice,
		StopLoss:    d.StopLoss,
		Position:    d.Position,
		Reasoning:   d.Reasoning,
		Summary:     d.Summary,
	}
}

func (c decisionColumns) toDomain() value_objects.SettledDecision {
	return value_objects.SettledDecision{
		DecidedBy: value_objects.AgentKind(c.DecidedBy),
		Decision:  c.decisionToDomain(),
	}
}

func (c decisionColumns) decisionToDomain() analysis_vo.Decision {
	return analysis_vo.Decision{
		Action:      analysis_vo.Action(c.Action),
		Confidence:  c.Confidence,
		RiskScore:   c.RiskScore,
		TargetPrice: c.TargetPrice,
		StopLoss:    c.StopLoss,
		Position:    c.Position,
		Reasoning:   c.Reasoning,
		Summary:     c.Summary,
	}
}

// AnalysisRunDto 对应 agent_runs 集合：一次分析一份文档，主键就是发起它的任务 ID。
//
// 用任务 ID 当 _id 而不是自动生成：分析任务的派发消息是至少一次投递，
// 同一个任务被重投递后会再跑一遍，此时整份轨迹应当被最新一次覆盖，
// 而不是在集合里堆出两份无从分辨的记录。自然键做主键让这件事由存储层直接保证，
// 仓储侧不需要任何去重逻辑。
type AnalysisRunDto struct {
	RunID       string `bson:"_id"`
	codeColumns `bson:",inline"`
	// TradeDate 存 YYYY-MM-DD 定长串，与 agent_indicators 同构，理由见 indicator.go。
	TradeDate string `bson:"trade_date"`
	Depth     int    `bson:"depth"`
	// Model 是请求里指定的模型（可能是空串或别名）。
	// 每一位成员实际落到哪个模型上记在 turns[].model 里——
	// 两者不是一回事，路由可能因为某个模型不可用而回落到默认模型。
	RequestedModel string `bson:"requested_model,omitempty"`

	StartedAt  time.Time `bson:"started_at"`
	FinishedAt time.Time `bson:"finished_at"`
	DurationMS int64     `bson:"duration_ms"`

	Turns    []turnColumns   `bson:"turns"`
	Decision decisionColumns `bson:"decision"`

	PromptTokens     int             `bson:"prompt_tokens"`
	CompletionTokens int             `bson:"completion_tokens"`
	TotalTokens      int             `bson:"total_tokens"`
	Calls            int             `bson:"calls"`
	CostUSD          decimal.Decimal `bson:"cost_usd"`

	// Failed 记录这次运行整体是否失败，FailReason 是中止原因。
	// 有它才分得清「跑完了但结论是待定」与「跑到一半挂了」，
	// 这两种情况在轨迹里看起来很像：都是若干条成功记录后面跟着一条失败记录。
	Failed     bool   `bson:"failed"`
	FailReason string `bson:"fail_reason,omitempty"`
}

// FromDomainAnalysisRun 把一次运行的共享状态翻译成落库形态。
//
// finishedAt 与 failReason 由仓储的调用方给：聚合自己不知道引擎在它之外
// 还做了装配结果这一步，也不知道编排器最终是带着错误返回的还是正常收尾的。
func FromDomainAnalysisRun(ac *entities.AnalysisContext, finishedAt time.Time, failReason string) *AnalysisRunDto {
	turns := ac.Turns()
	rows := make([]turnColumns, 0, len(turns))
	for _, t := range turns {
		rows = append(rows, turnColumnsOf(t))
	}

	usage := ac.Usage()
	startedAt := ac.StartedAt().Truncate(time.Millisecond)
	finishedAt = finishedAt.Truncate(time.Millisecond)

	return &AnalysisRunDto{
		RunID:          ac.RunID(),
		codeColumns:    codeColumnsOf(ac.Code()),
		TradeDate:      ac.TradeDate().String(),
		Depth:          ac.Depth().Int(),
		RequestedModel: ac.Model(),

		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		DurationMS: finishedAt.Sub(startedAt).Milliseconds(),

		Turns: rows,
		// 存 FinalDecision 而不是 Decision：用户看到的结论是前者
		// （浅层分析没有风控经理，结论从交易员的方案里解析），
		// 轨迹里记另一个值会让「系统当时到底建议了什么」有两个互相矛盾的答案。
		Decision: decisionColumnsOf(ac.FinalDecision()),

		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		Calls:            usage.Calls,
		CostUSD:          usage.CostUSD,

		Failed:     failReason != "",
		FailReason: failReason,
	}
}

// AnalysisRunSummaryDto 是 agent_runs 的投影形态：只取评分用得上的几列，
// 不带 turns。它对应仓储里那条显式写了 projection 的查询，
// 字段少一个都会让那条查询白白多读几十 KB 正文。
type AnalysisRunSummaryDto struct {
	RunID       string `bson:"_id"`
	codeColumns `bson:",inline"`
	TradeDate   string          `bson:"trade_date"`
	Decision    decisionColumns `bson:"decision"`
	Failed      bool            `bson:"failed"`
}

func (dto AnalysisRunSummaryDto) ToDomain() value_objects.RunSummary {
	return value_objects.RunSummary{
		RunID:     dto.RunID,
		Code:      dto.codeColumns.toDomain(),
		TradeDate: shared_vo.MustTradeDate(dto.TradeDate),
		// 回测只关心「系统建议了什么」，不关心是谁给的，取决策本身即可。
		Decision: dto.Decision.decisionToDomain(),
		Failed:   dto.Failed,
	}
}

func ToDomainAnalysisRunSummaries(rows []AnalysisRunSummaryDto) []value_objects.RunSummary {
	out := make([]value_objects.RunSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomain())
	}
	return out
}

// ToDomain 把一份运行文档翻译成只读值对象。
//
// 读路径回的是 RunTrace 这个值对象而不是 AnalysisContext 聚合：
// 那个聚合带着锁、带着未发布的领域事件、带着一整份行情素材，
// 它存在的意义是支撑一次正在进行的运行。回放一次已经结束的分析不需要这些东西，
// 把它重新拼出来只会让调用方以为自己手里那份还能继续跑。
func (dto AnalysisRunDto) ToDomain() value_objects.RunTrace {
	turns := make([]value_objects.TurnRecord, 0, len(dto.Turns))
	for _, r := range dto.Turns {
		turns = append(turns, r.toDomain())
	}
	return value_objects.RunTrace{
		RunID:          dto.RunID,
		Code:           dto.codeColumns.toDomain(),
		TradeDate:      shared_vo.MustTradeDate(dto.TradeDate),
		Depth:          analysis_vo.Depth(dto.Depth),
		RequestedModel: dto.RequestedModel,
		StartedAt:      dto.StartedAt,
		FinishedAt:     dto.FinishedAt,
		Duration:       time.Duration(dto.DurationMS) * time.Millisecond,
		Turns:          turns,
		Decision:       dto.Decision.toDomain(),
		Usage: value_objects.Usage{
			PromptTokens:     dto.PromptTokens,
			CompletionTokens: dto.CompletionTokens,
			TotalTokens:      dto.TotalTokens,
			Calls:            dto.Calls,
			CostUSD:          dto.CostUSD,
		},
		Failed:     dto.Failed,
		FailReason: dto.FailReason,
	}
}
