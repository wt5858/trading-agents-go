package value_objects

import (
	"testing"
	"time"
)

func TestDateRangeDaysIsInclusive(t *testing.T) {
	r, err := NewDateRange("2026-09-18", "2026-09-21")
	if err != nil {
		t.Fatalf("构造区间失败: %v", err)
	}
	got := r.Days()
	want := []string{"2026-09-18", "2026-09-19", "2026-09-20", "2026-09-21"}
	if len(got) != len(want) {
		t.Fatalf("应当摊成 %d 天，实际 %d 天: %v", len(want), len(got), got)
	}
	for i, d := range got {
		if d.String() != want[i] {
			t.Fatalf("第 %d 天应为 %s，实际 %s", i, want[i], d.String())
		}
	}
}

// 跨月跨年是最容易被日期加减写错的地方：手写 +1 天时用 AddDate(0,0,1) 之外的
// 任何算法（比如加 24h 再截断）都会在夏令时或闰年上翻车。
func TestDateRangeDaysCrossesMonthAndYear(t *testing.T) {
	r, err := NewDateRange("2026-12-30", "2027-01-02")
	if err != nil {
		t.Fatalf("构造区间失败: %v", err)
	}
	got := r.Days()
	want := []string{"2026-12-30", "2026-12-31", "2027-01-01", "2027-01-02"}
	if len(got) != len(want) {
		t.Fatalf("应当摊成 %d 天，实际 %v", len(want), got)
	}
	for i, d := range got {
		if d.String() != want[i] {
			t.Fatalf("第 %d 天应为 %s，实际 %s", i, want[i], d.String())
		}
	}
}

func TestDateRangeDaysSingleDay(t *testing.T) {
	r, err := NewDateRange("2026-09-21", "2026-09-21")
	if err != nil {
		t.Fatalf("构造区间失败: %v", err)
	}
	if got := r.Days(); len(got) != 1 || got[0].String() != "2026-09-21" {
		t.Fatalf("单日区间应当只有一天，实际 %v", got)
	}
}

// 非法区间返回 nil 而不是 panic 或报错：调用方拿到空序列自然什么都不做。
func TestDateRangeDaysRejectsIncomplete(t *testing.T) {
	for name, r := range map[string]DateRange{
		"两端为空":   {},
		"缺起点":    {End: MustTradeDate("2026-09-21")},
		"缺终点":    {Start: MustTradeDate("2026-09-21")},
		"起点晚于终点": {Start: MustTradeDate("2026-09-22"), End: MustTradeDate("2026-09-21")},
	} {
		if got := r.Days(); got != nil {
			t.Fatalf("%s 应当返回 nil，实际 %v", name, got)
		}
	}
}

// LastNDays 摊开后的天数必须是 n+1（闭区间含首尾）。
// K 线按日批量拉的调用次数直接等于这个数字，差一天就是差一次外部调用。
func TestLastNDaysSpansInclusiveRange(t *testing.T) {
	got := LastNDays(30).Days()
	if len(got) != 31 {
		t.Fatalf("近 30 天的闭区间应当有 31 天，实际 %d", len(got))
	}
	last, ok := got[len(got)-1].Time()
	if !ok {
		t.Fatal("最后一天应当可解析")
	}
	if today := time.Now().Format("2006-01-02"); last.Format("2006-01-02") != today {
		t.Fatalf("最后一天应为今天 %s，实际 %s", today, last.Format("2006-01-02"))
	}
}
