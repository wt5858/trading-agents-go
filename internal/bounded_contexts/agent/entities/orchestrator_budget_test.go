package entities

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
)

// costingAgent 是一位会花钱的成员。
//
// 与 fakeAgent 分开而不是给它加一个字段：预算测试关心的只有「花了多少」与
// 「被调用了没有」，而 fakeAgent 已经背着失败注入、延时、并发钩子三件事，
// 再挂一个会让每个读它的人都得先判断哪些字段与自己有关。
type costingAgent struct {
	contract value_objects.Contract
	cost     decimal.Decimal

	mu    sync.Mutex
	calls int
}

func newCostingAgent(t *testing.T, kind value_objects.AgentKind, cost string) *costingAgent {
	t.Helper()
	c, err := value_objects.NewContract(kind, value_objects.StepKeyOf(kind),
		value_objects.NoDataAccess(), value_objects.PolicyTolerant)
	if err != nil {
		t.Fatalf("构造契约失败: %v", err)
	}
	return &costingAgent{contract: c, cost: decimal.RequireFromString(cost)}
}

func (a *costingAgent) Contract() value_objects.Contract { return a.contract }

func (a *costingAgent) Act(_ context.Context, _ Runtime, ac *AnalysisContext) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	ac.CommitTurn(value_objects.TurnRecord{
		Kind:    a.contract.Kind,
		Content: "报告:" + a.contract.Kind.String(),
		Usage:   value_objects.Usage{Calls: 1, TotalTokens: 100, CostUSD: a.cost},
	})
	return nil
}

func (a *costingAgent) called() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// twoStagePlan 造一个「分析师 → 交易」的两阶段计划，每阶段一位成员。
// 两个阶段是必要的：成本天花板最划算的止损点就在阶段之间，
// 单阶段的计划测不出「后续阶段一次调用都没发生」这条性质。
func twoStagePlan(first, second Agent) Plan {
	return Plan{Stages: []Stage{
		{
			Phase:      value_objects.PhaseAnalyst,
			Mode:       value_objects.ModeParallel,
			Members:    []Agent{first},
			Limit:      1,
			MinSuccess: 1,
		},
		{
			Phase:   value_objects.PhaseTrading,
			Mode:    value_objects.ModeSequential,
			Members: []Agent{second},
		},
	}}
}

// TestOrchestrator_NoCeilingByDefault 是这道护栏最重要的回归保护。
//
// costCeilingUSD 是 decimal 零值，而 decimal 的零值就是 0。
// 如果判定写成「累计成本 >= 上限就停」而漏掉「上限为零表示不限」这一条，
// 结果不是报错，而是**第一位成员跑完之后所有人集体跳过**——
// 一次分析只剩一份报告，看起来像模型出了问题，完全不像配置问题。
func TestOrchestrator_NoCeilingByDefault(t *testing.T) {
	first := newCostingAgent(t, value_objects.KindMarketAnalyst, "5.00")
	second := newCostingAgent(t, value_objects.KindTrader, "5.00")

	ac := newTestContext(t, analysis_vo.DepthStandard)
	outcomes, err := NewOrchestrator(twoStagePlan(first, second), nil).
		Run(context.Background(), stubRuntime{}, ac)
	if err != nil {
		t.Fatalf("未设上限时不应中止: %v", err)
	}
	if len(outcomes) != 2 {
		t.Errorf("阶段数 = %d, 期望 2", len(outcomes))
	}
	if first.called() != 1 || second.called() != 1 {
		t.Errorf("未设上限时全员都该跑: first=%d second=%d", first.called(), second.called())
	}
}

// TestOrchestrator_CeilingStopsLaterStages 锁定护栏真的会拦人。
func TestOrchestrator_CeilingStopsLaterStages(t *testing.T) {
	// 第一位就把预算花超：上限 1.00，它花掉 1.50。
	first := newCostingAgent(t, value_objects.KindMarketAnalyst, "1.50")
	second := newCostingAgent(t, value_objects.KindTrader, "1.50")

	ac := newTestContext(t, analysis_vo.DepthStandard)
	outcomes, err := NewOrchestrator(twoStagePlan(first, second), nil).
		WithCostCeiling(decimal.RequireFromString("1.00")).
		Run(context.Background(), stubRuntime{}, ac)

	if err == nil {
		t.Fatal("超出成本上限后应当中止并报错")
	}
	if first.called() != 1 {
		t.Errorf("第一阶段应正常执行: calls=%d", first.called())
	}
	// 这是本测试的要害：护栏的价值就在于后续阶段**一次调用都没发生**。
	if second.called() != 0 {
		t.Errorf("超出上限后第二阶段仍被执行了 %d 次", second.called())
	}
	// 已完成阶段的产出必须保留：为止损而丢掉已经付过钱的结果毫无道理。
	if len(outcomes) != 1 {
		t.Errorf("已完成阶段数 = %d, 期望 1", len(outcomes))
	}
	if ac.Reports()[value_objects.KindMarketAnalyst.String()] == "" {
		t.Error("已完成成员的报告被丢弃了")
	}
}

// TestOrchestrator_CeilingNotReachedRunsAll 守住护栏的另一侧：没超就不该拦。
//
// 只测「超了会停」是不够的——一个恒返回 true 的判定同样能让那个测试通过，
// 而它会把每一次分析都砍在第一阶段。
func TestOrchestrator_CeilingNotReachedRunsAll(t *testing.T) {
	first := newCostingAgent(t, value_objects.KindMarketAnalyst, "0.10")
	second := newCostingAgent(t, value_objects.KindTrader, "0.10")

	ac := newTestContext(t, analysis_vo.DepthStandard)
	_, err := NewOrchestrator(twoStagePlan(first, second), nil).
		WithCostCeiling(decimal.RequireFromString("10.00")).
		Run(context.Background(), stubRuntime{}, ac)
	if err != nil {
		t.Fatalf("远未触及上限时不应中止: %v", err)
	}
	if first.called() != 1 || second.called() != 1 {
		t.Errorf("未触及上限时全员都该跑: first=%d second=%d", first.called(), second.called())
	}
}

// TestOrchestrator_CeilingStopsPeersWithinStage 锁定阶段**内**的拦截。
//
// 阶段间的检查挡不住同一个并行阶段里的后续批次：扇出上限为 1 时，
// 三位成员实际是依次执行的，第一位花超之后，后两位必须被拦下。
// 没有 runMember 里那道检查，这个阶段会把预算继续烧完才轮到阶段间的检查。
func TestOrchestrator_CeilingStopsPeersWithinStage(t *testing.T) {
	first := newCostingAgent(t, value_objects.KindMarketAnalyst, "9.00")
	second := newCostingAgent(t, value_objects.KindFundamentalsAnalyst, "9.00")
	third := newCostingAgent(t, value_objects.KindNewsAnalyst, "9.00")

	plan := Plan{Stages: []Stage{{
		Phase: value_objects.PhaseAnalyst,
		Mode:  value_objects.ModeParallel,
		// 扇出 1 把并行退化成串行，让「第一位跑完时后两位还没开始」成为确定事实。
		// 真实的并发扇出下，几位成员会同时通过检查，那是这道软护栏已知且接受的漏量。
		Limit:      1,
		Members:    []Agent{first, second, third},
		MinSuccess: 1,
	}}}

	ac := newTestContext(t, analysis_vo.DepthStandard)
	_, err := NewOrchestrator(plan, nil).
		WithCostCeiling(decimal.RequireFromString("1.00")).
		Run(context.Background(), stubRuntime{}, ac)
	// 容错阶段且 MinSuccess=1，第一位成功了，所以阶段整体不算失败。
	if err != nil {
		t.Fatalf("首位成功且满足 MinSuccess，阶段不应失败: %v", err)
	}
	if first.called() != 1 {
		t.Errorf("第一位应正常执行: calls=%d", first.called())
	}
	if second.called() != 0 || third.called() != 0 {
		t.Errorf("预算耗尽后同阶段的后续成员仍被执行: second=%d third=%d",
			second.called(), third.called())
	}
}
