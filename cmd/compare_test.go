package cmd

import (
	"testing"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func pairOf(crew, solo analysis_vo.Action, done bool) comparePair {
	return comparePair{
		crew: analysis_vo.Decision{Action: crew},
		solo: analysis_vo.Decision{Action: solo},
		done: done,
	}
}

// TestTallyPairs 覆盖配对统计的每一种组合。
//
// 方向一致与动作一致必须分开数：「买入 vs 增持」是两个不同的动作、
// 同一个方向。只报动作一致率会把这种情况算成分歧，从而低估两者的吻合程度；
// 只报方向一致率则会漏掉「同向但力度判断不同」这一层。
func TestTallyPairs(t *testing.T) {
	pairs := []comparePair{
		// 动作与方向都一致。
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionBuy, true),
		// 方向一致、动作不同（买入 vs 增持都是看涨）。
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionIncrease, true),
		// 方向相反。
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionSell, true),
		// 仅实验组给了方向。
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionHold, true),
		// 仅对照组给了方向。
		pairOf(analysis_vo.ActionHold, analysis_vo.ActionSell, true),
		// 两边都没给方向。
		pairOf(analysis_vo.ActionHold, analysis_vo.ActionHold, true),
		// 未完成的格子不该进任何计数。
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionBuy, false),
	}

	got := tallyPairs(pairs)

	if got.Compared != 6 {
		t.Errorf("Compared = %d, 期望 6（未完成的那格不计）", got.Compared)
	}
	// 动作完全一致的只有第 1 格与最后那格「持有 vs 持有」。
	if got.SameAction != 2 {
		t.Errorf("SameAction = %d, 期望 2", got.SameAction)
	}
	// 方向一致：买入/买入、买入/增持、持有/持有。
	if got.SameDir != 3 {
		t.Errorf("SameDir = %d, 期望 3", got.SameDir)
	}
	if got.BothNoDir != 1 {
		t.Errorf("BothNoDir = %d, 期望 1", got.BothNoDir)
	}
	if got.CrewDirOnly != 1 {
		t.Errorf("CrewDirOnly = %d, 期望 1", got.CrewDirOnly)
	}
	if got.SoloDirOnly != 1 {
		t.Errorf("SoloDirOnly = %d, 期望 1", got.SoloDirOnly)
	}
}

// TestTallyPairs_BothNoDirIsCountedInSameDir 锁定一条容易读错的口径。
//
// 两边都说「持有」算方向一致——它们确实给出了同样的判断。
// 但这类样本必须同时进 BothNoDir 单独报出来：一批双方都弃权的格子
// 会把重合率推高，读起来却像是两种方法高度吻合，
// 而真相是「两种方法都没看出什么」。
func TestTallyPairs_BothNoDirIsCountedInSameDir(t *testing.T) {
	pairs := []comparePair{
		pairOf(analysis_vo.ActionHold, analysis_vo.ActionHold, true),
		pairOf(analysis_vo.ActionHold, analysis_vo.ActionHold, true),
	}
	got := tallyPairs(pairs)
	if got.SameDir != 2 {
		t.Errorf("SameDir = %d, 期望 2", got.SameDir)
	}
	if got.BothNoDir != got.SameDir {
		t.Errorf("BothNoDir = %d 应等于 SameDir = %d —— 此时的高一致率完全来自双方弃权，"+
			"必须能从数字上看出来", got.BothNoDir, got.SameDir)
	}
}

func TestTallyPairs_Empty(t *testing.T) {
	if got := tallyPairs(nil); got.Compared != 0 {
		t.Errorf("空输入应得到零计数: %+v", got)
	}
	// 全部未完成时同样是零。
	if got := tallyPairs([]comparePair{
		pairOf(analysis_vo.ActionBuy, analysis_vo.ActionBuy, false),
	}); got.Compared != 0 {
		t.Errorf("没有完成的格子时应得到零计数: %+v", got)
	}
}

// TestSoloRunIDIsDisjointFromBackfill 守住两个命名空间不相交。
//
// 实验组与对照组的轨迹都落在 agent_runs 里，用 _id 覆盖写。
// 前缀一旦撞上，对照组会直接覆盖掉实验组那一格——而两份数据
// 都还在的时候你根本不会发现，最后拿到的是一份自己和自己比的数据集，
// 重合率必然 100%，且那个数字看起来完全正常。
func TestSoloRunIDIsDisjointFromBackfill(t *testing.T) {
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	date := shared_vo.MustTradeDate("2026-03-02")

	solo := soloRunID(code, date)
	if solo != soloRunID(code, date) {
		t.Error("对照组 ID 必须是确定性的，否则无法断点续跑")
	}
	for _, depth := range []analysis_vo.Depth{
		analysis_vo.DepthQuick, analysis_vo.DepthStandard, analysis_vo.DepthExhaustive,
	} {
		if bf := backfillRunID(code, date, depth); bf == solo {
			t.Errorf("对照组 ID %q 与实验组 depth %d 的 ID 相同", solo, depth.Int())
		}
	}
}
