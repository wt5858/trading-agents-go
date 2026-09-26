// Package entities 承载 agent 上下文的实体与聚合根：有身份、有生命周期、有不变式。
//
// 分层职责：
//   - 业务不变式全部在这里（阶段顺序、失败语义、层间可见性、决策收敛）；
//   - 不出现任何 IO：模型调用与取数由 Runtime 端口代劳，端口声明在本包、实现在 domain_services；
//   - 不出现任何持久化标签：落库形态属于 repositories/dtos。
package entities

import (
	"sync"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// MarketBrief 是数据层的内容：一次分析开始前一次性备齐的全部行情素材。
//
// 它只放值对象，不放 stock 上下文的实体：跨限界上下文只允许传 ID 与值对象，
// 把 stock.Stock 聚合搬进来会让 agent 的演进被 stock 的聚合结构绑死。
// 唯一的例外是几个描述性字段（Name/Industry），它们由引擎从 stock 上下文取出后
// 以裸值形式填入——它们是快照数据，不是对聚合的引用。
//
// Indicators 是从 IndicatorRepository **读回来**的，不是在这里算的。
// 这条规则的理由写在 value_objects/indicators.go 的注释里。
type MarketBrief struct {
	Name       string
	Industry   string
	Quote      stock_vo.Quote
	Klines     []stock_vo.Kline
	Indicators value_objects.Indicators
	Financials []stock_vo.Financial
	News       []stock_vo.News
	Social     []stock_vo.SocialPost
	// Missing 记录哪些素材没取到，提示词会如实告诉模型「这块没有数据」，
	// 而不是给它一个空数组让它自行脑补。
	Missing []string
	// TrackRecord 是本系统对该标的历史建议的兑现情况，已渲染成文本。
	//
	// # 它默认是空的
	//
	// 把「你上次看多，其后跌了 8%」喂回给模型是一个**会改变结论**的干预，
	// 因此它由 EngineConfig.MemoryEnabled 显式开启，默认关闭，
	// 并且要作为一个独立的对照臂去测——而不是悄悄改善所有人的结论。
	//
	// 小样本下它很可能是有害的：一只票上三五条历史记录不构成任何证据，
	// 而模型会把它当成强信号过度修正。上线前必须有一个数字说明它到底是好是坏。
	TrackRecord string
}

// HasIndicators 判定技术指标是否可用。
func (b MarketBrief) HasIndicators() bool { return !b.Indicators.IsZero() }

// ContextSnapshot 是分析上下文在某一时刻的只读快照。
//
// 它存在的理由是并发：六位分析师并行执行时，每个人都要读一遍上下文来渲染提示词。
// 直接把 *AnalysisContext 递给提示词服务意味着读取发生在锁外，
// 而另一位分析师可能正在写 Reports——那是一个必现但极难复现的 map 并发读写崩溃。
// 快照在锁内一次性复制完成，之后随便怎么读都安全。
type ContextSnapshot struct {
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Depth     analysis_vo.Depth
	// Model 是本次分析指定的模型，空串表示走路由的默认模型。
	// 它随快照下发而不是由 Runtime 自己决定：同一次分析的十四位成员
	// 必须用同一个模型，否则成本和结论都无从对照。
	Model  string
	Market MarketBrief
	// Reports 是截至快照时刻已完成的全部报告。
	// 层间可见性不在这里限制——限制它的是 Plan 的阶段顺序：
	// 交易员的快照里天然不会有风控的报告，因为风控还没跑。
	Reports map[value_objects.AgentKind]string
	// Failures 记录已失败的成员及原因，让下游智能体知道「情绪面缺席」这件事。
	Failures map[value_objects.AgentKind]string
	Decision analysis_vo.Decision
}

// ReportOf 取某位成员的报告，没有则返回空串。
func (s ContextSnapshot) ReportOf(k value_objects.AgentKind) string { return s.Reports[k] }

// HasReport 判定某位成员是否已产出报告。
func (s ContextSnapshot) HasReport(k value_objects.AgentKind) bool {
	return s.Reports[k] != ""
}

// AnalysisContext 是一次分析的五层共享状态，也是本上下文的核心聚合。
//
// # 为什么它必须自带锁
//
// 分析师阶段与风控辩论阶段都是并行执行的：三到六个 goroutine 会在几乎同一时刻
// 调用 PutReport / AddUsage。Go 的 map 并发写会直接 panic 并打掉整个进程，
// 而 Usage 的累加没有锁就是经典的丢失更新。
//
// 锁放在聚合内部而不是让调用方自己加，是因为「谁来加锁」一旦成为调用方的责任，
// 就一定会有某条新加的代码路径忘了加。RWMutex 而非 Mutex：
// 快照读远多于写（每位成员开工前读一次、写一次）。
//
// 嵌入的 EventRecorder 本身不是并发安全的（见 domain_kernel 的说明），
// 这里的安全性来自「所有 AddDomainEvent 调用都发生在本聚合的写锁内」这条约束。
type AnalysisContext struct {
	domain_event.EventRecorder

	// runID 是这次运行的身份，取自发起它的分析任务 ID。
	//
	// 不自己生成一个：轨迹落库之后唯一有意义的查法是「这个任务跑出了什么」，
	// 用任务 ID 当主键让这条查询不需要任何中间映射表，
	// 也让消息重投递导致的重跑天然覆盖同一份轨迹而不是堆出两份。
	// 空串表示这次运行没有对应任务（脚本直调），此时轨迹不落库。
	runID     string
	code      shared_vo.StockCode
	tradeDate shared_vo.TradeDate
	depth     analysis_vo.Depth
	model     string
	startedAt time.Time

	mu       sync.RWMutex
	market   MarketBrief
	reports  map[value_objects.AgentKind]string
	failures map[value_objects.AgentKind]string
	// turns 是按提交时刻排列的发言轨迹，只追加不修改。
	turns []value_objects.TurnRecord
	// seq 是发言序号的分配器，与 turns 同在写锁下，因此不会重号。
	seq      int
	usage    value_objects.Usage
	decision analysis_vo.Decision
}

// NewAnalysisContext 按分析请求创建共享状态。
// runID 传发起本次运行的任务 ID，没有对应任务时传空串。
func NewAnalysisContext(runID string, req analysis_vo.Request) *AnalysisContext {
	return &AnalysisContext{
		runID:     runID,
		code:      req.Code,
		tradeDate: req.TradeDate,
		depth:     req.Depth,
		model:     req.LLMModel,
		startedAt: time.Now(),
		reports:   make(map[value_objects.AgentKind]string, len(value_objects.AllKinds())),
		failures:  make(map[value_objects.AgentKind]string, 4),
		turns:     make([]value_objects.TurnRecord, 0, len(value_objects.AllKinds())),
	}
}

func (c *AnalysisContext) RunID() string                  { return c.runID }
func (c *AnalysisContext) Model() string                  { return c.model }
func (c *AnalysisContext) Code() shared_vo.StockCode      { return c.code }
func (c *AnalysisContext) TradeDate() shared_vo.TradeDate { return c.tradeDate }
func (c *AnalysisContext) Depth() analysis_vo.Depth       { return c.depth }
func (c *AnalysisContext) StartedAt() time.Time           { return c.startedAt }

// LoadMarketBrief 填充数据层。只允许在分析师阶段开始之前调用一次，
// 之后全程只读——数据层在跑的过程中变化会让两位分析师看到不同的行情，
// 他们的分歧就不再是观点分歧了。
func (c *AnalysisContext) LoadMarketBrief(b MarketBrief) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.market = b
}

// CommitTurn 收下一位成员的一次发言：记轨迹、记账、写报告或记失败、发事件。
//
// # 为什么成功与失败走同一个方法
//
// 它们原先是 PutReport 与 RecordFailure 两个方法，而两条路径要做的事有八成重合
// （追加轨迹、累加消耗、发一个成员级事件）。分成两个方法的直接后果是
// 每加一样要记的东西就得在两处各写一遍，漏掉的那一处只会在
// 「恰好这位成员失败了」的时候才暴露——agent.go 的注释里说的正是这类偏差。
// 合成一个入口之后，失败与成功的差别收敛成 rec.Failed 这一个分支。
//
// # 不变式
//
//   - 序号在写锁内分配，因此并行阶段的三到六位成员不会重号，
//     且序号顺序就是真实完成顺序；
//   - 空报告视同没写：模型偶尔返回空字符串（内容过滤、max_tokens 撞线），
//     把空串记成「有报告」会让下游智能体以为上游给过结论；
//   - 失败同样计消耗：撞上下文长度上限的那次调用是真花了钱的，
//     不记账会让成本统计长期偏低。
func (c *AnalysisContext) CommitTurn(rec value_objects.TurnRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 阶段可以从身份推出来，允许调用方不填：让每个调用点自己填一遍，
	// 迟早有人填成另一个阶段，而那种错误在轨迹里看起来完全正常。
	if rec.Phase == "" {
		rec.Phase = rec.Kind.Phase()
	}
	c.seq++
	rec.Seq = c.seq
	c.turns = append(c.turns, rec)

	c.usage = c.usage.Plus(rec.Usage)

	if rec.Failed {
		c.failures[rec.Kind] = rec.FailReason
		c.AddDomainEvent(domain_events.NewOnAgentFailed(
			c.code.FullSymbol(), c.tradeDate.String(), rec.Kind.String(), rec.FailReason))
		return
	}

	if rec.Content != "" {
		c.reports[rec.Kind] = rec.Content
		// 成功一次就把之前的失败记录清掉：失败后重试成功的成员
		// 不该在报告里继续挂着「已失败」的牌子。
		delete(c.failures, rec.Kind)
	}
	c.AddDomainEvent(domain_events.NewOnAgentCompleted(
		c.code.FullSymbol(), c.tradeDate.String(), rec.Kind.String(),
		rec.Usage.TotalTokens, rec.Usage.CostUSD))
}

// Turns 返回发言轨迹的副本，按提交顺序排列。
func (c *AnalysisContext) Turns() []value_objects.TurnRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]value_objects.TurnRecord(nil), c.turns...)
}

// SetDecision 收下终局决策，并在收下时就把它收敛到合法区间。
//
// 收敛发生在这里而不是读取时：决策会被落库、被推送、被写进报告，
// 读取时收敛意味着每个读者都要记得调一次 Normalized，漏一个就出一个越界数字。
func (c *AnalysisContext) SetDecision(d analysis_vo.Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decision = d.Normalized()
	c.AddDomainEvent(domain_events.NewOnDecisionMade(
		c.code.FullSymbol(), c.tradeDate.String(),
		c.decision.Action.String(), c.decision.Confidence, c.decision.RiskScore))
}

// Snapshot 在读锁内复制出一份完整的只读视图。
func (c *AnalysisContext) Snapshot() ContextSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	reports := make(map[value_objects.AgentKind]string, len(c.reports))
	for k, v := range c.reports {
		reports[k] = v
	}
	failures := make(map[value_objects.AgentKind]string, len(c.failures))
	for k, v := range c.failures {
		failures[k] = v
	}
	return ContextSnapshot{
		Code:      c.code,
		TradeDate: c.tradeDate,
		Depth:     c.depth,
		Model:     c.model,
		Market:    c.market,
		Reports:   reports,
		Failures:  failures,
		Decision:  c.decision,
	}
}

// Reports 返回全部报告的副本，键为成员标识。
// 它是 analysis_vo.Result.Reports 的直接来源，因此键必须是稳定的成员字面量。
func (c *AnalysisContext) Reports() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.reports))
	for k, v := range c.reports {
		out[k.String()] = v
	}
	return out
}

// FailedKinds 返回失败成员列表，顺序按 AllKinds 固定，便于稳定展示与测试。
func (c *AnalysisContext) FailedKinds() []value_objects.AgentKind {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]value_objects.AgentKind, 0, len(c.failures))
	for _, k := range value_objects.AllKinds() {
		if _, bad := c.failures[k]; bad {
			out = append(out, k)
		}
	}
	return out
}

func (c *AnalysisContext) Usage() value_objects.Usage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usage
}

func (c *AnalysisContext) Decision() analysis_vo.Decision {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.decision
}

// FinalDecision 返回这次分析对外的终局决策连同它的归属，
// 是「决策收敛」这条不变式的落点。
//
// 深度低于 3 时不跑风控阶段，也就没有风控经理来 SetDecision，此时决策从交易员的
// 方案里解析。不这么做的话，浅层分析会返回一个 action=undecided 的结果——
// 明明交易员已经白纸黑字写了「买入」，用户看到的却是「待定」。
//
// # 为什么连归属一起返回
//
// 因为「结论是谁的」只能在这里判。归属如果由别处另行推导（比如按
// 「风控经理有没有发言」），就会在一种很常见的情况下与结论分叉：
// 风控经理正常写了报告，但末尾的结构化块格式坏掉，ParseDecision 退化成
// undecided——结论已经回落到交易员，归属却还写着风控经理终裁。
// 决策链界面上「风控经理终裁」这行字，指的必须是真的给出了这个结论的那个人。
//
// 它必须在聚合里而不是在引擎里：终局决策有两个读者（装配给用户的 Result，
// 以及落库的运行轨迹），规则放在引擎里就意味着谁想读都得记得先调一次那个私有函数，
// 漏掉的那个读者会拿到一个和另一个读者不一致的结论。
func (c *AnalysisContext) FinalDecision() value_objects.SettledDecision {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.decision.Action.Valid() && c.decision.Action != analysis_vo.ActionUndecided {
		return value_objects.SettledDecision{
			Decision:  c.decision,
			DecidedBy: value_objects.KindRiskManager,
		}
	}

	plan := c.reports[value_objects.KindTrader]
	if plan == "" {
		// 谁都没给出方向。归属留空而不是硬安给某位成员：
		// 「没有结论」本身是一个诚实且可展示的状态。
		return value_objects.SettledDecision{Decision: c.decision}
	}
	fallback := value_objects.ParseDecision(plan)
	// 只在真的解析出方向时才替换，否则保留原值（含风控经理写下的理由）。
	if fallback.Action == analysis_vo.ActionUndecided {
		return value_objects.SettledDecision{Decision: c.decision}
	}
	if c.decision.Reasoning != "" {
		fallback.Reasoning = c.decision.Reasoning
	}
	return value_objects.SettledDecision{
		Decision:  fallback.Normalized(),
		DecidedBy: value_objects.KindTrader,
	}
}

// HasAnyReport 判定是否至少有一份报告。
// 一份都没有时，后续阶段拿到的是空上下文，让模型对着空气写报告毫无意义，
// 编排器据此判定整次分析失败。
func (c *AnalysisContext) HasAnyReport() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.reports) > 0
}

// DrainEvents 在写锁内取走累积的领域事件。
//
// 单独包一层而不是让调用方直接用 EventRecorder.GetAllPendingEvents：
// 后者会在并行阶段与 PutReport 并发访问同一个切片，
// 这正是 EventRecorder 文档里说的「不要在多协程里改聚合」。
// 本聚合天生并发，于是必须由它自己把这条约束补上。
func (c *AnalysisContext) DrainEvents() []domain_event.DomainEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.GetAllPendingEvents()
}
