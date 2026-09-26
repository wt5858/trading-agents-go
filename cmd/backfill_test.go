package cmd

import (
	"testing"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// TestBackfillRunIDIsDeterministicAndDistinguishesDepth 守住断点续跑的两条前提。
//
// 轨迹落库走的是按 _id 的 upsert，因此 runID 决定了「重跑覆盖谁」：
// 不确定就没有续跑（每次都是新记录，集合里堆出一批重复样本）；
// 而深度不进 ID，则是一个更隐蔽的坑——对照实验的第二臂会把第一臂的结果
// 原地覆盖掉，两份数据都还在的时候你根本不会发现，
// 最后拿到的是一份看起来正常、实际只有单臂的数据集。
func TestBackfillRunIDIsDeterministicAndDistinguishesDepth(t *testing.T) {
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	date := shared_vo.MustTradeDate("2026-03-02")

	first := backfillRunID(code, date, analysis_vo.DepthStandard)
	again := backfillRunID(code, date, analysis_vo.DepthStandard)
	if first != again {
		t.Errorf("同一格两次生成的 ID 不同: %q vs %q", first, again)
	}

	if other := backfillRunID(code, date, analysis_vo.DepthQuick); other == first {
		t.Errorf("不同深度必须是不同的 ID，否则对照实验的臂会互相覆盖: %q", other)
	}

	otherDate := backfillRunID(code, shared_vo.MustTradeDate("2026-03-03"), analysis_vo.DepthStandard)
	if otherDate == first {
		t.Error("不同交易日必须是不同的 ID")
	}

	second, err := shared_vo.NewStockCode("000001", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	if otherCode := backfillRunID(second, date, analysis_vo.DepthStandard); otherCode == first {
		t.Error("不同标的必须是不同的 ID")
	}
}

func TestSampleEvery(t *testing.T) {
	days := make([]shared_vo.TradeDate, 0, 10)
	for _, s := range []string{
		"2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06",
		"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13",
	} {
		days = append(days, shared_vo.MustTradeDate(s))
	}

	cases := []struct {
		name  string
		every int
		want  []string
	}{
		{"每个都取", 1, []string{
			"2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06",
			"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13",
		}},
		{"每 5 个取一个", 5, []string{"2026-03-02", "2026-03-09"}},
		{"每 3 个取一个", 3, []string{"2026-03-02", "2026-03-05", "2026-03-10", "2026-03-13"}},
		// 0 与负数来自命令行手滑，退化为全取而不是取空——
		// 「一个都不跑」在任何语境下都不是用户的意图，而它的表现是一句
		// 「没有需要跑的格子」，看起来像是数据有问题。
		{"every=0 退化为全取", 0, []string{
			"2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06",
			"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13",
		}},
		{"负数退化为全取", -3, []string{
			"2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06",
			"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13",
		}},
		{"步长超过总数只取首个", 99, []string{"2026-03-02"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sampleEvery(days, tc.every)
			if len(got) != len(tc.want) {
				t.Fatalf("取到 %d 个，期望 %d 个: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].String() != tc.want[i] {
					t.Errorf("第 %d 个 = %s, 期望 %s", i, got[i].String(), tc.want[i])
				}
			}
		})
	}

	if got := sampleEvery(nil, 5); len(got) != 0 {
		t.Errorf("空输入应返回空: %v", got)
	}
}

func TestParseUSD(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		// 空串表示「不设限」，必须解析成零值而不是报错：
		// 这两个 flag 都是可选的，不传是最常见的用法。
		{"空串即不限", "", "0", false},
		{"只有空白也算不限", "   ", "0", false},
		{"正常金额", "12.50", "12.5", false},
		{"零", "0", "0", false},
		{"负数必须报错", "-1", "", true},
		{"非法字符必须报错", "abc", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUSD(tc.raw, "--test-flag")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，却得到 %s", got.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("= %s, 期望 %s", got.String(), tc.want)
			}
		})
	}
}

// TestBackfillAnalystList 覆盖分析师列表的解析。
//
// 留空返回 nil 这一条不是细节：nil 会让请求走该深度的默认阵容，
// 而默认阵容在 depth 1 与 depth 3 之间是不同的（2 位 vs 4 位）。
// 做跨深度对照时必须显式传 analysts 把阵容钉死，否则变的不止一个量。
func TestBackfillAnalystList(t *testing.T) {
	defer func(old string) { backfillAnalysts = old }(backfillAnalysts)

	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"market", []string{"market"}},
		{"market,fundamentals", []string{"market", "fundamentals"}},
		{" market , fundamentals ", []string{"market", "fundamentals"}},
		{"market,,fundamentals", []string{"market", "fundamentals"}},
		{",", nil},
	}
	for _, tc := range cases {
		backfillAnalysts = tc.raw
		got := backfillAnalystList()
		if len(got) != len(tc.want) {
			t.Errorf("%q -> %v, 期望 %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q 第 %d 项 = %s, 期望 %s", tc.raw, i, got[i], tc.want[i])
			}
		}
	}
}
