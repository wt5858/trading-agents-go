package entities

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

func mustStockCode(t *testing.T) shared_vo.StockCode {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	return code
}

func sampleDecision() analysis_vo.Decision {
	return analysis_vo.Decision{
		Action:      analysis_vo.ActionBuy,
		Confidence:  decimal.RequireFromString("0.82"),
		RiskScore:   decimal.RequireFromString("3.5"),
		TargetPrice: decimal.RequireFromString("1980"),
		StopLoss:    decimal.RequireFromString("1620"),
		Position:    decimal.RequireFromString("30"),
		Reasoning:   "均线多头排列，业绩确定性高。",
		Summary:     "建议逢低买入并持有。",
	}
}

func buildResult(t *testing.T, reports map[string]string) analysis_vo.Result {
	t.Helper()
	return analysis_vo.NewResult(
		mustStockCode(t),
		shared_vo.MustTradeDate("2026-09-15"),
		sampleDecision(),
		reports,
		analysis_vo.TokenUsage{},
		nil,
	)
}

func sectionKeys(sections []value_objects.Section) []value_objects.SectionKey {
	out := make([]value_objects.SectionKey, 0, len(sections))
	for _, s := range sections {
		out = append(out, s.Key)
	}
	return out
}

// TestFromAnalysisResult_SectionOrdering 是本上下文最重要的一条投影规则：
// 无论各智能体报告以什么顺序到达，章节都必须按「结论 -> 证据 -> 风险」排布。
//
// 入参是 map，Go 的 map 遍历顺序是随机的——这个测试同时也是对
// 「排序不依赖遍历顺序」的回归保护。
func TestFromAnalysisResult_SectionOrdering(t *testing.T) {
	result := buildResult(t, map[string]string{
		// 刻意乱序书写：风控在最前，分析师在最后。
		"risk_manager":     "风险终裁正文",
		"trader":           "交易方案正文",
		"bear":             "空头论证正文",
		"bull":             "多头论证正文",
		"news":             "消息面正文",
		"market":           "技术面正文",
		"fundamentals":     "基本面正文",
		"research_manager": "研究结论正文",
	})

	report, err := FromAnalysisResult("rpt_1", 7, "task_1", result)
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}

	want := []value_objects.SectionKey{
		value_objects.SectionConclusion,
		value_objects.SectionMarket,
		value_objects.SectionFundamentals,
		value_objects.SectionNews,
		value_objects.SectionBull,
		value_objects.SectionBear,
		value_objects.SectionResearchManager,
		value_objects.SectionTrader,
		value_objects.SectionRiskManager,
	}
	got := sectionKeys(report.Sections)
	if len(got) != len(want) {
		t.Fatalf("章节数量不符，期望 %d，实际 %d（%v）", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("章节顺序不符，第 %d 节期望 %s，实际 %s（完整顺序 %v）", i, want[i], got[i], got)
		}
	}

	// 结论在最前、风险在最后，是这张排序表存在的全部理由，单独断言一次。
	if got[0] != value_objects.SectionConclusion {
		t.Fatalf("结论必须是第一节，实际是 %s", got[0])
	}
	if got[len(got)-1] != value_objects.SectionRiskManager {
		t.Fatalf("风险必须是最后一节，实际是 %s", got[len(got)-1])
	}

	// Order 必须严格递增：它随章节一起落库，读路径直接按它排序。
	for i := 1; i < len(report.Sections); i++ {
		if report.Sections[i].Order <= report.Sections[i-1].Order {
			t.Fatalf("Order 必须严格递增，第 %d 节 %d 未大于前一节 %d",
				i, report.Sections[i].Order, report.Sections[i-1].Order)
		}
	}

	// 标题取自权威表，而不是原样照抄 agentID。
	if report.Sections[1].Title != "技术面分析" {
		t.Fatalf("章节标题应取自排序表，实际 %q", report.Sections[1].Title)
	}
}

// TestFromAnalysisResult_OrderingIsStable 防回归：同一份输入必须每次产出同样的顺序。
// map 遍历随机，一旦投影逻辑不小心依赖了它，这个测试会间歇性失败。
func TestFromAnalysisResult_OrderingIsStable(t *testing.T) {
	reports := map[string]string{
		"market":       "技术面",
		"news":         "消息面",
		"fundamentals": "基本面",
		"trader":       "交易方案",
		"risk_manager": "风险终裁",
		// 两个排序表里没有的 key，验证追加段落本身也是确定性的。
		"zeta_analyst":  "未登记分析师 Z",
		"alpha_analyst": "未登记分析师 A",
	}

	var first []value_objects.SectionKey
	for i := 0; i < 20; i++ {
		report, err := FromAnalysisResult("rpt_1", 7, "task_1", buildResult(t, reports))
		if err != nil {
			t.Fatalf("生成报告失败: %v", err)
		}
		got := sectionKeys(report.Sections)
		if first == nil {
			first = got
			continue
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次生成的章节顺序发生漂移：%v != %v", i, got, first)
			}
		}
	}
}

// TestFromAnalysisResult_UnknownSectionsGoLast 未登记的章节追加在末尾并按字典序排，
// 而不是被丢弃：静默丢掉一整段分析内容是没人会发现的数据丢失。
func TestFromAnalysisResult_UnknownSectionsGoLast(t *testing.T) {
	report, err := FromAnalysisResult("rpt_1", 7, "task_1", buildResult(t, map[string]string{
		"market":        "技术面",
		"risk_manager":  "风险终裁",
		"zeta_analyst":  "未登记分析师 Z",
		"alpha_analyst": "未登记分析师 A",
	}))
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}

	got := sectionKeys(report.Sections)
	want := []value_objects.SectionKey{
		value_objects.SectionConclusion,
		value_objects.SectionMarket,
		value_objects.SectionRiskManager,
		"alpha_analyst",
		"zeta_analyst",
	}
	if len(got) != len(want) {
		t.Fatalf("章节数量不符，期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("章节顺序不符，期望 %v，实际 %v", want, got)
		}
	}
	// 表外章节的标题退化为 key 本身——难看是故意的，它是「有人忘了登记」的提示。
	if report.Sections[3].Title != "alpha_analyst" {
		t.Fatalf("未登记章节标题应退化为 key，实际 %q", report.Sections[3].Title)
	}
}

// TestFromAnalysisResult_DropsEmptySections 分析师是容错执行的，缺席者不该在报告里
// 留下一个点开是空白的折叠块。
func TestFromAnalysisResult_DropsEmptySections(t *testing.T) {
	report, err := FromAnalysisResult("rpt_1", 7, "task_1", buildResult(t, map[string]string{
		"market":       "技术面",
		"fundamentals": "",      // 分析师失败
		"news":         "   \n", // 只有空白
		"risk_manager": "风险终裁",
	}))
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}
	for _, s := range report.Sections {
		if s.Key == value_objects.SectionFundamentals || s.Key == value_objects.SectionNews {
			t.Fatalf("空内容章节不应出现在报告里: %s", s.Key)
		}
	}
	if len(report.Sections) != 3 {
		t.Fatalf("期望 3 节（结论/技术面/风险），实际 %d：%v", len(report.Sections), sectionKeys(report.Sections))
	}
}

// TestFromAnalysisResult_CopiesDecisionVerbatim 终局数字必须逐字抄录。
// 报告是快照，任何再加工都会让报告里的数字和分析结果对不上。
func TestFromAnalysisResult_CopiesDecisionVerbatim(t *testing.T) {
	result := buildResult(t, map[string]string{"market": "技术面"})
	report, err := FromAnalysisResult("rpt_1", 7, "task_1", result)
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}

	d := result.Decision
	if report.Action != d.Action ||
		report.Confidence != d.Confidence ||
		report.RiskScore != d.RiskScore ||
		report.TargetPrice != d.TargetPrice ||
		report.StopLoss != d.StopLoss ||
		report.Position != d.Position {
		t.Fatalf("终局数字未逐字抄录：报告 %+v，决策 %+v", report, d)
	}

	// 结论段在生成时就把百分比等派生量固化进正文，读路径不再重算。
	conclusion, err := report.SectionOf(value_objects.SectionConclusion)
	if err != nil {
		t.Fatalf("结论章节缺失: %v", err)
	}
	if !strings.Contains(conclusion.Content, "82%") {
		t.Fatalf("结论正文应固化置信度百分比，实际:\n%s", conclusion.Content)
	}
	if !strings.Contains(conclusion.Content, "买入") {
		t.Fatalf("结论正文应包含操作建议，实际:\n%s", conclusion.Content)
	}
}

// TestFromAnalysisResult_Rejects 缺失标识的报告无法去重也无法追溯，必须在入口拦住。
func TestFromAnalysisResult_Rejects(t *testing.T) {
	result := buildResult(t, map[string]string{"market": "技术面"})

	cases := map[string]struct {
		id     string
		userID uint64
		taskID string
	}{
		"缺少报告 ID": {"", 7, "task_1"},
		"缺少归属用户":  {"rpt_1", 0, "task_1"},
		"缺少任务来源":  {"rpt_1", 7, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromAnalysisResult(tc.id, tc.userID, tc.taskID, result); err == nil {
				t.Fatal("期望构造失败，实际成功")
			} else if custom_errors.CodeOf(err) != custom_errors.CodeInvalidArgument {
				t.Fatalf("期望 INVALID_ARGUMENT，实际 %s", custom_errors.CodeOf(err))
			}
		})
	}
}

// TestSectionOf 取不存在的章节返回 NotFound 而不是空章节：
// 静默返回空白会把一个拼错的路径伪装成「这一节确实没内容」。
func TestSectionOf(t *testing.T) {
	report, err := FromAnalysisResult("rpt_1", 7, "task_1", buildResult(t, map[string]string{"market": "技术面"}))
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}

	if _, err := report.SectionOf(value_objects.SectionMarket); err != nil {
		t.Fatalf("已有章节应可取到: %v", err)
	}
	if _, err := report.SectionOf(value_objects.SectionSentiment); custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
		t.Fatalf("缺席章节应返回 NOT_FOUND，实际 %v", err)
	}
	if _, err := report.SectionOf(""); custom_errors.CodeOf(err) != custom_errors.CodeInvalidArgument {
		t.Fatalf("空 key 应返回 INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestFromAnalysisResult_RaisesEvent 报告生成必须留下一个可被下游消费的事实。
func TestFromAnalysisResult_RaisesEvent(t *testing.T) {
	report, err := FromAnalysisResult("rpt_1", 7, "task_1", buildResult(t, map[string]string{"market": "技术面"}))
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}
	evts := report.GetAllPendingEvents()
	if len(evts) != 1 {
		t.Fatalf("期望 1 个领域事件，实际 %d", len(evts))
	}
	if evts[0].Name() != "report.report_generated" {
		t.Fatalf("事件名不符: %s", evts[0].Name())
	}
	// 取出即清空，保证同一个事件不会被发布两次。
	if len(report.GetAllPendingEvents()) != 0 {
		t.Fatal("事件取出后应清空")
	}
}
