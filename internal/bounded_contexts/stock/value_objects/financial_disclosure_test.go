package value_objects

import (
	"testing"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func finWith(report, announce string) Financial {
	f := Financial{ReportDate: shared_vo.MustTradeDate(report)}
	f.PeriodType = PeriodTypeOfReportDate(f.ReportDate)
	if announce != "" {
		f.AnnounceDate = shared_vo.MustTradeDate(announce)
	}
	return f
}

// TestDisclosedBy_UsesAnnounceDateWhenPresent 有披露日时一切以它为准。
//
// 这是这个功能的主路径：2023 年的年报在 2024-04-25 才公布，
// 那么 2024-03-01 的分析就不该看到它——哪怕它的报告期早已是过去。
func TestDisclosedBy_UsesAnnounceDateWhenPresent(t *testing.T) {
	annual := finWith("2023-12-31", "2024-04-25")

	if annual.DisclosedBy(shared_vo.MustTradeDate("2024-03-01")) {
		t.Error("披露日之前就放行了这份年报——这正是要防的未来函数")
	}
	if annual.DisclosedBy(shared_vo.MustTradeDate("2024-04-24")) {
		t.Error("披露前一天不应放行")
	}
	if !annual.DisclosedBy(shared_vo.MustTradeDate("2024-04-25")) {
		t.Error("披露当天应当放行")
	}
	if !annual.DisclosedBy(shared_vo.MustTradeDate("2024-06-01")) {
		t.Error("披露之后应当放行")
	}
}

// TestDisclosedBy_MissingAnnounceDateFallsBackConservatively 是这里最要紧的一条。
//
// 现有库里的财报**全部**没有披露日（这个字段是后加的）。如果缺失时直接放行，
// 这道防护对存量数据完全不生效——而它看起来是生效的，
// 因为新同步进来的数据带着披露日、测试也是拿那种数据写的。
func TestDisclosedBy_MissingAnnounceDateFallsBackConservatively(t *testing.T) {
	annual := finWith("2023-12-31", "")
	if annual.PeriodType != PeriodTypeAnnual {
		t.Fatalf("前置条件不成立：12-31 应推断为年报，得到 %s", annual.PeriodType)
	}

	// 年报法定披露期限是次年 4-30，约 120 天。
	if annual.DisclosedBy(shared_vo.MustTradeDate("2024-03-01")) {
		t.Error("缺披露日时，年报在报告期后两个月就被放行了——保守回退没有生效")
	}
	if !annual.DisclosedBy(shared_vo.MustTradeDate("2024-05-31")) {
		t.Error("报告期后五个月仍不放行，回退过于保守，会丢掉大量可用数据")
	}

	// 半年报 06-30 的法定期限是 8-31，约 62 天。
	half := finWith("2023-06-30", "")
	if half.DisclosedBy(shared_vo.MustTradeDate("2023-07-15")) {
		t.Error("缺披露日时，半年报在半个月后就被放行了")
	}
	if !half.DisclosedBy(shared_vo.MustTradeDate("2023-10-01")) {
		t.Error("报告期后三个月仍不放行，回退过于保守")
	}
}

// TestDisclosedBy_ZeroAsOfDisablesFilter 实时分析不做披露过滤。
//
// asOf 为零值意味着「现在」，而一切已经入库的财报按定义都已经公布过了。
// 在这里也套用保守回退，只会让前端的财务页凭空少掉最近一期。
func TestDisclosedBy_ZeroAsOfDisablesFilter(t *testing.T) {
	// 连报告期都是未来的极端数据，实时场景下同样放行——
	// 过滤未来数据是回测的事，实时查询的职责是如实展示库里有什么。
	f := finWith("2026-12-31", "")
	if !f.DisclosedBy(shared_vo.TradeDate{}) {
		t.Error("asOf 为零值时不应做任何过滤")
	}
}

// TestDisclosedBy_NoReportDateIsRejected 连报告期都没有的记录无从判断，不放行。
func TestDisclosedBy_NoReportDateIsRejected(t *testing.T) {
	var f Financial
	if f.DisclosedBy(shared_vo.MustTradeDate("2024-06-01")) {
		t.Error("既无披露日也无报告期的记录不该被放行——无从判断时宁可不用")
	}
}

// TestDisclosedBy_AnnounceDateOverridesFallback 有披露日时不走保守回退。
//
// 实务中财报常常提前披露：报告期 2023-12-31 的年报在 2024-01-20 就公布的情况很常见。
// 此时若仍按 120 天回退，这份数据会被白白丢掉三个月。
func TestDisclosedBy_AnnounceDateOverridesFallback(t *testing.T) {
	early := finWith("2023-12-31", "2024-01-20")
	if !early.DisclosedBy(shared_vo.MustTradeDate("2024-02-01")) {
		t.Error("有披露日时应以它为准，不该再套用保守滞后期")
	}
}
