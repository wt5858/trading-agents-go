package entities

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeAgent 是一位可编程的成员：可以指定它失败、指定它慢，以及记录它是否被调用过。
// 编排器只认 Agent 接口，因此这里完全不需要模型、提示词或数据库。
type fakeAgent struct {
	contract value_objects.Contract
	fail     error
	delay    time.Duration
	// hook 在 Act 真正执行时被调用，用来观察并发行为。
	hook func()

	mu    sync.Mutex
	calls int
}

func newFakeAgent(t *testing.T, kind value_objects.AgentKind, policy value_objects.FailurePolicy) *fakeAgent {
	t.Helper()
	c, err := value_objects.NewContract(kind, value_objects.StepKeyOf(kind),
		value_objects.NoDataAccess(), policy)
	if err != nil {
		t.Fatalf("构造契约失败: %v", err)
	}
	return &fakeAgent{contract: c}
}

func (f *fakeAgent) Contract() value_objects.Contract { return f.contract }

func (f *fakeAgent) Act(ctx context.Context, _ Runtime, ac *AnalysisContext) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.hook != nil {
		f.hook()
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.fail != nil {
		ac.RecordFailure(f.contract.Kind, f.fail.Error(), value_objects.Usage{})
		return f.fail
	}
	ac.PutReport(f.contract.Kind, "报告:"+f.contract.Kind.String(), value_objects.Usage{Calls: 1, TotalTokens: 10})
	return nil
}

func (f *fakeAgent) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// recordingSink 记录进度回调，用于验证「完成/失败」的汇报与键值。
type recordingSink struct {
	mu     sync.Mutex
	done   []string
	failed []string
}

func (s *recordingSink) Step(key, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = append(s.done, key)
}

func (s *recordingSink) StepFailed(key, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, key)
}

func (s *recordingSink) snapshot() (done, failed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.done...), append([]string(nil), s.failed...)
}

// stubRuntime 永远不会被调用：fakeAgent.Act 不使用 Runtime。
type stubRuntime struct{}

func (stubRuntime) Execute(context.Context, Turn) (TurnResult, error) {
	return TurnResult{Content: "stub"}, nil
}

func newTestContext(t *testing.T, depth analysis_vo.Depth) *AnalysisContext {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate("2024-03-01"), depth, nil, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return NewAnalysisContext(req)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 并行 × 容错
// ---------------------------------------------------------------------------

// TestOrchestrator_ParallelTolerant_OneFailureDoesNotStopPeers 是容错阶段的核心语义：
// 一位分析师挂掉，其余五位必须照常跑完。
func TestOrchestrator_ParallelTolerant_OneFailureDoesNotStopPeers(t *testing.T) {
	a := newFakeAgent(t, value_objects.KindMarketAnalyst, value_objects.PolicyTolerant)
	b := newFakeAgent(t, value_objects.KindFundamentalsAnalyst, value_objects.PolicyTolerant)
	b.fail = errors.New("数据源超时")
	c := newFakeAgent(t, value_objects.KindNewsAnalyst, value_objects.PolicyTolerant)

	plan := Plan{Stages: []Stage{{
		Phase:      value_objects.PhaseAnalyst,
		Mode:       value_objects.ModeParallel,
		Members:    []Agent{a, b, c},
		Limit:      3,
		MinSuccess: 1,
	}}}
	sink := &recordingSink{}
	ac := newTestContext(t, analysis_vo.DepthStandard)

	outcomes, err := NewOrchestrator(plan, sink).Run(context.Background(), stubRuntime{}, ac)
	if err != nil {
		t.Fatalf("容错阶段不应整体失败: %v", err)
	}
	if a.called() != 1 || c.called() != 1 {
		t.Errorf("同僚未被执行: a=%d c=%d", a.called(), c.called())
	}
	if len(outcomes) != 1 || len(outcomes[0].Failed) != 1 ||
		outcomes[0].Failed[0] != value_objects.KindFundamentalsAnalyst {
		t.Errorf("失败名单不正确: %+v", outcomes)
	}

	done, failed := sink.snapshot()
	if !contains(done, "analyst:market") || !contains(done, "analyst:news") {
		t.Errorf("成功步骤未汇报: %v", done)
	}
	if !contains(failed, "analyst:fundamentals") {
		t.Errorf("失败步骤未汇报: %v", failed)
	}
	// 失败的成员不应留下报告，但必须留下失败记录，供下游智能体知悉缺席。
	if ac.Reports()[value_objects.KindFundamentalsAnalyst.String()] != "" {
		t.Error("失败的成员不应产出报告")
	}
	if len(ac.FailedKinds()) != 1 {
		t.Errorf("失败记录数 = %d, 期望 1", len(ac.FailedKinds()))
	}
}

// TestOrchestrator_ParallelTolerant_AllFailViolatesMinSuccess 全员阵亡时，
// 每个人的失败都是可容忍的，但阶段整体没有产出，必须失败——
// 否则下一阶段会对着空上下文编造论据。
func TestOrchestrator_ParallelTolerant_AllFailViolatesMinSuccess(t *testing.T) {
	var members []Agent
	for _, kind := range []value_objects.AgentKind{
		value_objects.KindMarketAnalyst,
		value_objects.KindFundamentalsAnalyst,
	} {
		a := newFakeAgent(t, kind, value_objects.PolicyTolerant)
		a.fail = errors.New("模型不可用")
		members = append(members, a)
	}

	plan := Plan{Stages: []Stage{{
		Phase:      value_objects.PhaseAnalyst,
		Mode:       value_objects.ModeParallel,
		Members:    members,
		Limit:      2,
		MinSuccess: 1,
	}}}
	_, err := NewOrchestrator(plan, nil).Run(context.Background(), stubRuntime{},
		newTestContext(t, analysis_vo.DepthStandard))
	if err == nil {
		t.Fatal("全员失败且 MinSuccess=1 时应当返回错误")
	}
}

// TestOrchestrator_ParallelActuallyRunsConcurrently 并行阶段必须真的并行。
//
// 三位成员各自在钩子里等所有人到齐；如果实现退化成串行，第一个就会永远等下去，
// 测试会在超时后失败。
func TestOrchestrator_ParallelActuallyRunsConcurrently(t *testing.T) {
	const n = 3
	var (
		wg      sync.WaitGroup
		arrived = make(chan struct{}, n)
	)
	wg.Add(n)

	kinds := []value_objects.AgentKind{
		value_objects.KindRiskAggressive,
		value_objects.KindRiskConservative,
		value_objects.KindRiskNeutral,
	}
	members := make([]Agent, 0, n)
	for _, kind := range kinds {
		a := newFakeAgent(t, kind, value_objects.PolicyTolerant)
		a.hook = func() {
			arrived <- struct{}{}
			wg.Done()
			// 等所有人到齐。串行执行下这里会一直阻塞。
			wg.Wait()
		}
		members = append(members, a)
	}

	plan := Plan{Stages: []Stage{{
		Phase:   value_objects.PhaseRisk,
		Mode:    value_objects.ModeParallel,
		Members: members,
		Limit:   RiskFanOutLimit,
	}}}

	done := make(chan error, 1)
	go func() {
		_, err := NewOrchestrator(plan, nil).Run(context.Background(), stubRuntime{},
			newTestContext(t, analysis_vo.DepthExhaustive))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("并行阶段失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("并行阶段超时：只有 %d/%d 位成员启动，实现可能退化成串行", len(arrived), n)
	}
}

// TestOrchestrator_ParallelStrict_FailsFast 并行阶段里只要存在严格成员，
// 它的失败必须让整个阶段立即失败（走 concurrency.Map 的快速失败路径）。
func TestOrchestrator_ParallelStrict_FailsFast(t *testing.T) {
	strict := newFakeAgent(t, value_objects.KindRiskManager, value_objects.PolicyStrict)
	strict.fail = errors.New("上下文超长")
	peer := newFakeAgent(t, value_objects.KindRiskNeutral, value_objects.PolicyTolerant)
	peer.delay = 2 * time.Second

	plan := Plan{Stages: []Stage{{
		Phase:   value_objects.PhaseRisk,
		Mode:    value_objects.ModeParallel,
		Members: []Agent{strict, peer},
		Limit:   2,
	}}}

	start := time.Now()
	_, err := NewOrchestrator(plan, nil).Run(context.Background(), stubRuntime{},
		newTestContext(t, analysis_vo.DepthExhaustive))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("严格成员失败时阶段必须失败")
	}
	// 快速失败的意义就是不再等同僚跑完，否则只是在烧钱。
	if elapsed >= 2*time.Second {
		t.Errorf("耗时 %v，说明没有取消同僚，退化成了全员结算", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 串行 × 容错/严格
// ---------------------------------------------------------------------------

// TestOrchestrator_SequentialTolerant_ContinuesAfterFailure 多头挂了，
// 空头与研究经理照样要跑：单方论证仍然有裁决价值。
func TestOrchestrator_SequentialTolerant_ContinuesAfterFailure(t *testing.T) {
	bull := newFakeAgent(t, value_objects.KindBullResearcher, value_objects.PolicyTolerant)
	bull.fail = errors.New("模型限流")
	bear := newFakeAgent(t, value_objects.KindBearResearcher, value_objects.PolicyTolerant)
	manager := newFakeAgent(t, value_objects.KindResearchManager, value_objects.PolicyStrict)

	plan := Plan{Stages: []Stage{{
		Phase:   value_objects.PhaseDebate,
		Mode:    value_objects.ModeSequential,
		Members: []Agent{bull, bear, manager},
	}}}
	outcomes, err := NewOrchestrator(plan, nil).Run(context.Background(), stubRuntime{},
		newTestContext(t, analysis_vo.DepthStandard))
	if err != nil {
		t.Fatalf("容错成员失败不应中断阶段: %v", err)
	}
	if bear.called() != 1 || manager.called() != 1 {
		t.Errorf("后续成员未执行: bear=%d manager=%d", bear.called(), manager.called())
	}
	if len(outcomes[0].Failed) != 1 {
		t.Errorf("失败名单 = %v", outcomes[0].Failed)
	}
}

// TestOrchestrator_SequentialStrict_AbortsImmediately 严格成员失败后，
// 后续成员一个都不该跑：它们的输入已经不存在了。
func TestOrchestrator_SequentialStrict_AbortsImmediately(t *testing.T) {
	manager := newFakeAgent(t, value_objects.KindResearchManager, value_objects.PolicyStrict)
	manager.fail = errors.New("模型不可用")
	trader := newFakeAgent(t, value_objects.KindTrader, value_objects.PolicyStrict)

	plan := Plan{Stages: []Stage{
		{
			Phase:   value_objects.PhaseDebate,
			Mode:    value_objects.ModeSequential,
			Members: []Agent{manager},
		},
		{
			Phase:   value_objects.PhaseTrading,
			Mode:    value_objects.ModeSequential,
			Members: []Agent{trader},
		},
	}}
	sink := &recordingSink{}
	outcomes, err := NewOrchestrator(plan, sink).Run(context.Background(), stubRuntime{},
		newTestContext(t, analysis_vo.DepthStandard))

	if err == nil {
		t.Fatal("严格成员失败时必须返回错误")
	}
	if trader.called() != 0 {
		t.Error("严格成员失败后，后续阶段不应执行")
	}
	if len(outcomes) != 1 {
		t.Errorf("只应产出失败阶段的结果，got %d 个阶段", len(outcomes))
	}
	_, failed := sink.snapshot()
	if !contains(failed, "debate:manager") {
		t.Errorf("失败步骤未汇报: %v", failed)
	}
}

// TestOrchestrator_StopsOnCanceledContext 父 ctx 取消后不得再启动新成员。
func TestOrchestrator_StopsOnCanceledContext(t *testing.T) {
	agent := newFakeAgent(t, value_objects.KindTrader, value_objects.PolicyStrict)
	plan := Plan{Stages: []Stage{{
		Phase:   value_objects.PhaseTrading,
		Mode:    value_objects.ModeSequential,
		Members: []Agent{agent},
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewOrchestrator(plan, nil).Run(ctx, stubRuntime{},
		newTestContext(t, analysis_vo.DepthStandard)); err == nil {
		t.Fatal("已取消的 ctx 应当返回错误")
	}
	if agent.called() != 0 {
		t.Error("ctx 已取消时不应执行任何成员")
	}
}

// ---------------------------------------------------------------------------
// 成员与共享上下文
// ---------------------------------------------------------------------------

// scriptedRuntime 按成员身份返回预设文本。
type scriptedRuntime struct {
	replies map[value_objects.AgentKind]string
	err     error
}

func (r scriptedRuntime) Execute(_ context.Context, turn Turn) (TurnResult, error) {
	if r.err != nil {
		return TurnResult{Usage: value_objects.Usage{Calls: 1, TotalTokens: 5}}, r.err
	}
	return TurnResult{
		Content: r.replies[turn.Contract.Kind],
		Usage:   value_objects.Usage{Calls: 1, TotalTokens: 100, CostUSD: decimal.RequireFromString("0.01")},
	}, nil
}

// TestRiskManager_AbsorbsDecision 风控经理的产出必须在写进上下文的同时
// 被解析成结构化决策——这是 Result.Decision 的唯一来源。
func TestRiskManager_AbsorbsDecision(t *testing.T) {
	ac := newTestContext(t, analysis_vo.DepthExhaustive)
	// 前置条件：风控经理要求交易方案存在。
	ac.PutReport(value_objects.KindTrader, "交易方案：回踩买入", value_objects.Usage{})

	rt := scriptedRuntime{replies: map[value_objects.AgentKind]string{
		value_objects.KindRiskManager: "## 终裁\n采纳中性派意见。\n\n===决策===\n动作: 买入\n置信度: 0.66\n建议仓位: 20\n风险评分: 5\n===结束===",
	}}

	if err := NewRiskManager().Act(context.Background(), rt, ac); err != nil {
		t.Fatalf("风控经理执行失败: %v", err)
	}
	d := ac.Decision()
	if d.Action != analysis_vo.ActionBuy {
		t.Errorf("Action = %v, 期望 buy", d.Action)
	}
	if d.Confidence.LessThan(decimal.RequireFromString("0.65")) ||
		d.Confidence.GreaterThan(decimal.RequireFromString("0.67")) {
		t.Errorf("Confidence = %v, 期望 0.66", d.Confidence)
	}
	if usage := ac.Usage(); usage.Calls != 1 || usage.TotalTokens != 100 {
		t.Errorf("消耗未累计: %+v", usage)
	}
}

// TestCrewMember_PreconditionFailsFast 前置条件不满足时必须直接失败，
// 不去浪费一次模型调用。
func TestCrewMember_PreconditionFailsFast(t *testing.T) {
	ac := newTestContext(t, analysis_vo.DepthExhaustive)
	rt := scriptedRuntime{replies: map[value_objects.AgentKind]string{
		value_objects.KindRiskManager: "不该被调用",
	}}

	err := NewRiskManager().Act(context.Background(), rt, ac)
	if err == nil {
		t.Fatal("没有交易方案时风控经理应当失败")
	}
	if usage := ac.Usage(); usage.Calls != 0 {
		t.Errorf("前置条件失败不应产生模型消耗: %+v", usage)
	}
	if len(ac.FailedKinds()) != 1 {
		t.Error("前置条件失败应当被记入上下文")
	}
}

// TestCrewMember_EmptyContentIsFailure 模型返回空串（内容过滤、撞上 max_tokens）
// 必须算失败，否则下游会以为上游给过结论。
func TestCrewMember_EmptyContentIsFailure(t *testing.T) {
	ac := newTestContext(t, analysis_vo.DepthStandard)
	rt := scriptedRuntime{replies: map[value_objects.AgentKind]string{}}

	if err := NewMarketAnalyst().Act(context.Background(), rt, ac); err == nil {
		t.Fatal("空内容应当算作失败")
	}
	if ac.HasAnyReport() {
		t.Error("空内容不应被记成报告")
	}
}

// TestAnalysisContext_ConcurrentWrites 并行阶段会有多个协程同时写上下文。
// 这条测试在 -race 下运行时才有完整意义，但即使不开 race，
// map 的并发写也会直接 panic。
func TestAnalysisContext_ConcurrentWrites(t *testing.T) {
	ac := newTestContext(t, analysis_vo.DepthExhaustive)
	kinds := value_objects.AllKinds()

	var wg sync.WaitGroup
	for _, kind := range kinds {
		wg.Add(1)
		go func(k value_objects.AgentKind) {
			defer wg.Done()
			ac.PutReport(k, "报告", value_objects.Usage{Calls: 1, TotalTokens: 1})
			_ = ac.Snapshot()
		}(kind)
	}
	wg.Wait()

	if got := ac.Usage().Calls; got != len(kinds) {
		t.Errorf("并发累加丢失更新: Calls = %d, 期望 %d", got, len(kinds))
	}
	if len(ac.Reports()) != len(kinds) {
		t.Errorf("报告数 = %d, 期望 %d", len(ac.Reports()), len(kinds))
	}
}
