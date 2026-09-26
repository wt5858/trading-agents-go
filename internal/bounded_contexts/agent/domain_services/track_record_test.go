package domain_services

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
)

func sampleAt(date, action string, ret string, hit bool) repositories.TrackRecordSample {
	return repositories.TrackRecordSample{
		TradeDate: date,
		Action:    action,
		ReturnPct: decimal.RequireFromString(ret),
		Hit:       hit,
	}
}

// TestRenderTrackRecord_ExcludesCurrentAndFutureDates 是这个功能的承重测试。
//
// 战绩记的是「当时建议了什么、后来涨跌如何」。对历史交易日跑回填时，
// 库里存着**那一天之后**的评分结果——把它们喂回给正在做那一天决策的模型，
// 就是直接把答案告诉它。这不会报错，只会让回测结果准得可疑。
//
// 当天那条尤其危险：它评的正是本次要做的这个决策。
func TestRenderTrackRecord_ExcludesCurrentAndFutureDates(t *testing.T) {
	rec := repositories.TrackRecord{
		Symbol: "600519",
		Samples: []repositories.TrackRecordSample{
			sampleAt("2026-03-10", "buy", "5.00", true),   // 未来：必须排除
			sampleAt("2026-03-02", "sell", "-3.00", true), // 当天：必须排除
			sampleAt("2026-02-20", "buy", "-8.32", false), // 过去：保留
			sampleAt("2026-02-10", "buy", "6.10", true),   // 过去：保留
		},
	}

	got := renderTrackRecord(rec, "2026-03-02", 10)

	if strings.Contains(got, "2026-03-10") {
		t.Error("未来交易日的记录进了提示词——这是未来函数")
	}
	if strings.Contains(got, "2026-03-02") {
		t.Error("本次交易日当天的记录进了提示词——它评的正是这次决策，等于把答案告诉模型")
	}
	if !strings.Contains(got, "2026-02-20") || !strings.Contains(got, "2026-02-10") {
		t.Errorf("过去的记录应当保留:\n%s", got)
	}
	// 计数同样必须只含过去的两条，否则统计口径和列出的明细对不上。
	if !strings.Contains(got, "累计 2 次") {
		t.Errorf("计数应为 2（只含过去的记录）:\n%s", got)
	}
	if !strings.Contains(got, "判对 1 次") {
		t.Errorf("命中数应为 1:\n%s", got)
	}
}

// TestRenderTrackRecord_EmptyWhenNoPastSamples 全部记录都在当天或之后时返回空串。
//
// 返回空串而不是「暂无战绩」：模板用 {{if .TrackRecord}} 整段跳过，
// 而一句「暂无历史战绩」会白占十四份提示词的篇幅，还会诱导模型对它做出解读。
func TestRenderTrackRecord_EmptyWhenNoPastSamples(t *testing.T) {
	rec := repositories.TrackRecord{
		Symbol: "600519",
		Samples: []repositories.TrackRecordSample{
			sampleAt("2026-03-02", "buy", "1.00", true),
			sampleAt("2026-03-05", "buy", "2.00", true),
		},
	}
	if got := renderTrackRecord(rec, "2026-03-02", 10); got != "" {
		t.Errorf("没有过去的样本时应返回空串，得到:\n%s", got)
	}
	if got := renderTrackRecord(repositories.TrackRecord{}, "2026-03-02", 10); got != "" {
		t.Errorf("空战绩应返回空串，得到:\n%s", got)
	}
}

// TestRenderTrackRecord_EmptyCutoffKeepsAll 实时分析没有交易日，全部历史都是过去。
func TestRenderTrackRecord_EmptyCutoffKeepsAll(t *testing.T) {
	rec := repositories.TrackRecord{
		Samples: []repositories.TrackRecordSample{
			sampleAt("2026-03-10", "buy", "5.00", true),
			sampleAt("2026-02-20", "sell", "-2.00", false),
		},
	}
	got := renderTrackRecord(rec, "", 10)
	if !strings.Contains(got, "累计 2 次") {
		t.Errorf("cutoff 为空时应保留全部样本:\n%s", got)
	}
}

// TestRenderTrackRecord_LimitCapsShownButNotCounted 锁定「截断的是明细，不是统计」。
//
// 战绩会内联进十四位成员的每一份提示词，明细必须有上限。
// 但计数不能跟着截断——「累计 30 次判对 18 次」与「累计 8 次判对 5 次」
// 对模型的含义完全不同，而后者是把上限误当成样本量的结果。
func TestRenderTrackRecord_LimitCapsShownButNotCounted(t *testing.T) {
	var samples []repositories.TrackRecordSample
	for i := 0; i < 30; i++ {
		// 日期从 2026-01-01 起递增，全部早于 cutoff。
		day := "2026-01-" + string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10))
		samples = append(samples, sampleAt(day, "buy", "1.00", i%2 == 0))
	}
	rec := repositories.TrackRecord{Samples: samples}

	got := renderTrackRecord(rec, "2026-03-02", 8)

	if !strings.Contains(got, "累计 30 次") {
		t.Errorf("统计应覆盖全部 30 条，不受明细上限影响:\n%s", got)
	}
	if !strings.Contains(got, "判对 15 次") {
		t.Errorf("命中数应为 15:\n%s", got)
	}
	if n := strings.Count(got, "\n- "); n != 8 {
		t.Errorf("明细条数 = %d, 期望 8（受上限约束）", n)
	}
}

// TestRenderTrackRecord_MarksHitAndMiss 兑现与否必须写清楚。
//
// 只给收益率让模型自己判断是否兑现，等于把评分口径（横盘带宽）交给它现场发挥，
// 而那个口径在回测里是固定的——两边不一致时，战绩里的「对」和评测报告里的「对」
// 会是两回事。
func TestRenderTrackRecord_MarksHitAndMiss(t *testing.T) {
	rec := repositories.TrackRecord{
		Samples: []repositories.TrackRecordSample{
			sampleAt("2026-02-20", "buy", "-8.32", false),
			sampleAt("2026-02-10", "buy", "6.10", true),
		},
	}
	got := renderTrackRecord(rec, "2026-03-02", 10)
	if !strings.Contains(got, "未兑现") {
		t.Errorf("未命中的记录应标注未兑现:\n%s", got)
	}
	if !strings.Contains(got, "（兑现）") {
		t.Errorf("命中的记录应标注兑现:\n%s", got)
	}
	if !strings.Contains(got, "-8.32%") {
		t.Errorf("收益率应保留两位小数:\n%s", got)
	}
}
