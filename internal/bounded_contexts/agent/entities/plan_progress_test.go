package entities

import (
	"context"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 这个文件是「编排计划」与「进度条」之间那条无编译期保护的契约的执行点。
//
// Progress.Advance 对未知 key 的处理是原样返回（为了让引擎新增步骤不打崩旧任务），
// 于是引擎汇报一个拼错的 key 不会报任何错——进度条只是安静地停在那里，
// 而分析其实跑得好好的。这类哑故障在生产上极难定位，所以必须在这里全量比对。

func buildRequest(t *testing.T, depth analysis_vo.Depth, analysts []string) analysis_vo.Request {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate("2024-03-01"), depth, analysts, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// TestPlanStepKeysMatchProgress 是本包最重要的一条测试。
//
// 断言两件事：
//   - 集合相等：进度条上的每一个步骤都有人汇报，引擎汇报的每一个键都在进度条上；
//   - 顺序相容：实际执行序列是进度序列的子序列。前端按数组顺序画流程图，
//     顺序对不上会让用户看到「风控评估」排在「分析师」前面。
//
// 两条都必须查。只查集合，顺序错了看不出来；只查顺序，
// 少一个键的情况会被「子序列」这个宽松条件放过去。
func TestPlanStepKeysMatchProgress(t *testing.T) {
	crew := NewCrew()

	cases := []struct {
		name     string
		depth    analysis_vo.Depth
		analysts []string
	}{
		{"深度1-仅分析师", analysis_vo.DepthQuick, nil},
		{"深度2-含多空辩论", 2, nil},
		{"深度3-标准全流程", analysis_vo.DepthStandard, nil},
		{"深度5-全部六位分析师", analysis_vo.DepthExhaustive, nil},
		{"自选分析师子集", analysis_vo.DepthStandard, []string{"market", "news"}},
		{"自选分析师含未知ID", analysis_vo.DepthStandard, []string{"market", "astrology", "sentiment"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := buildRequest(t, tc.depth, tc.analysts)

			plan, err := NewPlan(req, crew)
			if err != nil {
				t.Fatalf("编排计划构建失败: %v", err)
			}

			want := make([]string, 0, 16)
			for _, s := range analysis_vo.NewProgress(req).Steps {
				want = append(want, s.Key)
			}

			// 引擎会汇报的全部键：首尾两步由 EngineService 负责，
			// 中间由编排器按成员契约汇报，跑不了的分析师则被显式标记为失败。
			got := []string{value_objects.StepPrepare.String()}
			for _, k := range plan.StepKeys() {
				got = append(got, k.String())
			}
			for _, k := range plan.Unsupported {
				got = append(got, k.String())
			}
			got = append(got, value_objects.StepReport.String())

			// 1) 集合必须完全相等。
			//    进度里有、引擎不报 -> 进度条永远停在那一格；
			//    引擎报了、进度里没有 -> Advance 静默忽略，那一步的耗时无从展示。
			if len(got) != len(want) {
				t.Fatalf("步骤数不一致:\n引擎 %v\n进度 %v", got, want)
			}
			wantSet := make(map[string]bool, len(want))
			for _, k := range want {
				wantSet[k] = true
			}
			for _, k := range got {
				if !wantSet[k] {
					t.Errorf("引擎汇报了进度里不存在的步骤 %q\n引擎 %v\n进度 %v", k, got, want)
				}
				delete(wantSet, k)
			}
			for k := range wantSet {
				t.Errorf("进度里的步骤 %q 没有任何成员汇报，进度条会停在这里\n引擎 %v", k, got)
			}

			// 2) 实际执行的顺序必须是进度顺序的子序列。
			//    前端按数组顺序画流程图，顺序对不上会让「风控评估」排到「分析师」前面。
			ordered := append([]string{value_objects.StepPrepare.String()}, stepStrings(plan.StepKeys())...)
			ordered = append(ordered, value_objects.StepReport.String())
			if !isSubsequence(ordered, want) {
				t.Errorf("执行顺序不是进度顺序的子序列:\n执行 %v\n进度 %v", ordered, want)
			}
		})
	}
}

func stepStrings(keys []value_objects.StepKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	return out
}

// isSubsequence 判断 sub 是否按序出现在 all 中。
func isSubsequence(sub, all []string) bool {
	i := 0
	for _, v := range all {
		if i < len(sub) && sub[i] == v {
			i++
		}
	}
	return i == len(sub)
}

// TestUnsupportedAnalystIsReportedFailed 点了一个不存在的分析师时，
// 那一格进度必须被显式标记为失败——而不是一直停着，让整条进度条看起来卡住。
func TestUnsupportedAnalystIsReportedFailed(t *testing.T) {
	req := buildRequest(t, analysis_vo.DepthQuick, []string{"market", "astrology"})
	plan, err := NewPlan(req, NewCrew())
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	if len(plan.Unsupported) != 1 || plan.Unsupported[0].String() != "analyst:astrology" {
		t.Fatalf("未记录不可用分析师: %v", plan.Unsupported)
	}

	sink := &recordingSink{}
	if _, err := NewOrchestrator(plan, sink).Run(context.Background(), stubRuntime{},
		newTestContext(t, analysis_vo.DepthQuick)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	_, failed := sink.snapshot()
	if !contains(failed, "analyst:astrology") {
		t.Errorf("不可用分析师的步骤未被标记失败: %v", failed)
	}
}

// TestStepKeyOfCoversEveryKind 每一位成员都必须有进度键。
// 缺一个的表现是那一步永远不亮，而不是报错。
func TestStepKeyOfCoversEveryKind(t *testing.T) {
	seen := make(map[value_objects.StepKey]value_objects.AgentKind, len(value_objects.AllKinds()))
	for _, kind := range value_objects.AllKinds() {
		key := value_objects.StepKeyOf(kind)
		if key.IsZero() {
			t.Errorf("成员 %s 没有进度键", kind)
			continue
		}
		if dup, ok := seen[key]; ok {
			// 两位成员共用一个键会让先完成的那位覆盖另一位的进度。
			t.Errorf("进度键 %q 被 %s 与 %s 共用", key, dup, kind)
		}
		seen[key] = kind
	}
}

// TestNewPlan_StageShape 校验每个深度下的阶段结构：模式、并发上限、成功下限。
func TestNewPlan_StageShape(t *testing.T) {
	crew := NewCrew()

	t.Run("深度1只有分析师与交易员", func(t *testing.T) {
		plan, err := NewPlan(buildRequest(t, analysis_vo.DepthQuick, nil), crew)
		if err != nil {
			t.Fatalf("构建失败: %v", err)
		}
		if len(plan.Stages) != 2 {
			t.Fatalf("阶段数 = %d, 期望 2", len(plan.Stages))
		}
		if plan.Stages[0].Phase != value_objects.PhaseAnalyst ||
			plan.Stages[1].Phase != value_objects.PhaseTrading {
			t.Errorf("阶段顺序错误: %v -> %v", plan.Stages[0].Phase, plan.Stages[1].Phase)
		}
	})

	t.Run("深度5含辩论与风控两段", func(t *testing.T) {
		plan, err := NewPlan(buildRequest(t, analysis_vo.DepthExhaustive, nil), crew)
		if err != nil {
			t.Fatalf("构建失败: %v", err)
		}
		wantPhases := []value_objects.Phase{
			value_objects.PhaseAnalyst,
			value_objects.PhaseDebate,
			value_objects.PhaseTrading,
			value_objects.PhaseRisk, // 三位辩手并行
			value_objects.PhaseRisk, // 风控经理终裁
		}
		if len(plan.Stages) != len(wantPhases) {
			t.Fatalf("阶段数 = %d, 期望 %d", len(plan.Stages), len(wantPhases))
		}
		for i, want := range wantPhases {
			if plan.Stages[i].Phase != want {
				t.Errorf("第 %d 阶段 = %v, 期望 %v", i, plan.Stages[i].Phase, want)
			}
		}

		analystStage := plan.Stages[0]
		if !analystStage.Mode.Parallel() {
			t.Error("分析师阶段必须并行")
		}
		if analystStage.Limit != AnalystFanOutLimit {
			t.Errorf("分析师并发上限 = %d, 期望 %d", analystStage.Limit, AnalystFanOutLimit)
		}
		if analystStage.MinSuccess != 1 {
			t.Errorf("分析师阶段 MinSuccess = %d, 期望 1", analystStage.MinSuccess)
		}
		if analystStage.HasStrictMember() {
			t.Error("分析师阶段应当全员容错")
		}

		debateStage := plan.Stages[1]
		if debateStage.Mode.Parallel() {
			t.Error("多空辩论必须串行：空头要读到多头的论证")
		}
		if !debateStage.HasStrictMember() {
			t.Error("研究经理应当是严格成员")
		}

		riskDebate := plan.Stages[3]
		if !riskDebate.Mode.Parallel() || riskDebate.Limit != RiskFanOutLimit {
			t.Errorf("风控辩论阶段配置错误: mode=%v limit=%d", riskDebate.Mode, riskDebate.Limit)
		}
		if riskDebate.HasStrictMember() {
			t.Error("三位风控辩手应当全员容错")
		}
		if riskDebate.MinSuccess != 0 {
			t.Error("风控辩手全挂时仍应允许经理独立终裁")
		}
	})

	t.Run("没有可用分析师时报错", func(t *testing.T) {
		req := analysis_vo.RehydrateRequest(
			mustCode(t), shared_vo.MustTradeDate("2024-03-01"),
			analysis_vo.DepthStandard, []string{"astrology"}, "")
		if _, err := NewPlan(req, crew); err == nil {
			t.Fatal("全部分析师 ID 非法时应当报错")
		}
	})
}

func mustCode(t *testing.T) shared_vo.StockCode {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	return code
}

// TestCrew_IsComplete 花名册必须凑齐十四位，且每位的契约自洽。
func TestCrew_IsComplete(t *testing.T) {
	crew := NewCrew()
	kinds := value_objects.AllKinds()
	if len(kinds) != 14 {
		t.Fatalf("成员种类 = %d, 期望 14", len(kinds))
	}
	for _, kind := range kinds {
		m := crew.Member(kind)
		if m == nil {
			t.Errorf("缺少成员: %s", kind)
			continue
		}
		c := m.Contract()
		if c.Kind != kind {
			t.Errorf("%s 的契约身份错位: %s", kind, c.Kind)
		}
		if c.Layer != kind.Layer() || c.Phase != kind.Phase() {
			t.Errorf("%s 的层/阶段与身份不一致: %v/%v", kind, c.Layer, c.Phase)
		}
		if c.Step != value_objects.StepKeyOf(kind) {
			t.Errorf("%s 的进度键错位: %s", kind, c.Step)
		}
		if !c.Policy.Valid() {
			t.Errorf("%s 的失败策略非法: %s", kind, c.Policy)
		}
		// 不带工具的成员不该配工具轮数，带工具的必须配。
		if c.Access.IsEmpty() && c.MaxToolRounds != 0 {
			t.Errorf("%s 无工具授权却配置了工具轮数", kind)
		}
	}
}

// TestCrew_ToolAuthorizationIsDistinct 分析师之间的工具授权必须有差异。
//
// 如果每位分析师都能看到全部数据，六份报告会迅速收敛成同一份，
// 后面的多空辩论也就失去了前提。
func TestCrew_ToolAuthorizationIsDistinct(t *testing.T) {
	crew := NewCrew()
	market := crew.Member(value_objects.KindMarketAnalyst).Contract().Access
	news := crew.Member(value_objects.KindNewsAnalyst).Contract().Access

	if !market.Allows(value_objects.ToolGetTechnicalIndics) {
		t.Error("市场分析师应当能读技术指标")
	}
	if market.Allows(value_objects.ToolGetNews) {
		t.Error("市场分析师不该拿到资讯工具")
	}
	if !news.Allows(value_objects.ToolGetNews) {
		t.Error("新闻分析师应当能读资讯")
	}
	if news.Allows(value_objects.ToolGetFinancials) {
		t.Error("新闻分析师不该拿到财务工具，否则会把基本面报告写第二遍")
	}
	// 两位裁决者不碰任何工具：它们的输入只有同僚的报告。
	for _, kind := range []value_objects.AgentKind{
		value_objects.KindResearchManager, value_objects.KindRiskManager,
	} {
		if !crew.Member(kind).Contract().Access.IsEmpty() {
			t.Errorf("%s 不应拥有任何工具授权", kind)
		}
	}
}
