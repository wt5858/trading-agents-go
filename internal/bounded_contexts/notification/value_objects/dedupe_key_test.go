package value_objects

import (
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ===========================================================================
// 去重键的稳定性
// ===========================================================================
//
// 这是本上下文唯一一条「算错了不会有任何报错，只会让用户多收到几条通知」的规则，
// 因此它必须被钉死在测试里：同一个来源事件，无论算多少次、隔多久，结果必须逐字节相同。

func TestDedupeKey_同一来源事件永远得到同一个键(t *testing.T) {
	const taskID = "8e7f0c2a-5b31-4d9e-9f10-2c4a6b8d0e12"

	// 模拟一次事件重放：同一件事被处理两遍，中间隔了任意长的时间。
	first, err := NewDedupeKey(KindAnalysisCompleted, taskID)
	if err != nil {
		t.Fatalf("首次构造去重键失败: %v", err)
	}
	second, err := NewDedupeKey(KindAnalysisCompleted, taskID)
	if err != nil {
		t.Fatalf("重放时构造去重键失败: %v", err)
	}

	if first != second {
		t.Fatalf("同一来源事件算出了两个键: %q vs %q", first.String(), second.String())
	}
	// 顺带钉住「键里不掺时间与随机数」这条性质：若实现里混入 time.Now 或 uuid，
	// 上面的相等断言在同一毫秒内仍可能偶然通过，而这里的前缀断言会直接失败。
	if want := KindAnalysisCompleted.String() + dedupeSeparator + taskID; first.String() != want {
		t.Fatalf("去重键 = %q，期望 %q（键只能由种类与来源标识拼成）", first.String(), want)
	}
}

func TestDedupeKey_种类不同则键不同(t *testing.T) {
	const taskID = "task-1"

	completed, err := NewDedupeKey(KindAnalysisCompleted, taskID)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	failed, err := NewDedupeKey(KindAnalysisFailed, taskID)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	// 若两者相同，一个任务失败重试后成功，用户就只会收到先到的那一条——
	// 「失败」会把后来的「完成」挡在唯一索引外面。
	if completed == failed {
		t.Fatalf("同一任务的完成与失败算出了同一个键 %q，后到的那条通知会被当成重放丢弃", completed.String())
	}
}

func TestDedupeKey_来源不同则键不同(t *testing.T) {
	a, err := NewDedupeKey(KindAnalysisCompleted, "task-1")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	b, err := NewDedupeKey(KindAnalysisCompleted, "task-2")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if a == b {
		t.Fatalf("两个不同的任务算出了同一个键 %q", a.String())
	}
}

func TestDedupeKey_首尾空白不影响稳定性(t *testing.T) {
	clean, err := NewDedupeKey(KindJobPaused, "job-9")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 事件载荷经过一次 JSON 往返后多出空白是完全可能的，两者必须归一，
	// 否则同一个任务会因为一个空格产生两条通知。
	padded, err := NewDedupeKey(KindJobPaused, "  job-9\n")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if clean != padded {
		t.Fatalf("空白未被归一: %q vs %q", clean.String(), padded.String())
	}
}

func TestDedupeKey_超长来源标识退化成哈希且依然稳定(t *testing.T) {
	longID := strings.Repeat("x", MaxDedupeKeyLen*2)

	first, err := NewDedupeKey(KindSystem, longID)
	if err != nil {
		t.Fatalf("超长来源标识不该报错: %v", err)
	}
	second, err := NewDedupeKey(KindSystem, longID)
	if err != nil {
		t.Fatalf("超长来源标识不该报错: %v", err)
	}

	if first != second {
		t.Fatalf("哈希退化路径不稳定: %q vs %q", first.String(), second.String())
	}
	if len(first.String()) > MaxDedupeKeyLen {
		t.Fatalf("键长 %d 超过列宽上限 %d，会被数据库静默截断", len(first.String()), MaxDedupeKeyLen)
	}

	// 截断会让两个前缀相同的长 ID 撞成同一个键；哈希不会。这正是不用截断的原因。
	other, err := NewDedupeKey(KindSystem, longID+"-different-suffix")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if first == other {
		t.Fatalf("两个仅后缀不同的超长来源撞成了同一个键 %q", first.String())
	}
}

func TestDedupeKey_非法输入报错而不是给出一个能用的键(t *testing.T) {
	cases := []struct {
		name     string
		kind     NotificationKind
		sourceID string
	}{
		{name: "种类非法", kind: NotificationKind("not_a_kind"), sourceID: "task-1"},
		{name: "种类为空", kind: "", sourceID: "task-1"},
		{name: "来源标识为空", kind: KindAnalysisCompleted, sourceID: ""},
		{name: "来源标识只有空白", kind: KindAnalysisCompleted, sourceID: "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewDedupeKey(tc.kind, tc.sourceID)
			if err == nil {
				t.Fatalf("期望报错，却得到键 %q——一个没有来源的键等于放弃去重", got.String())
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码 = %s，期望 %s", code, custom_errors.CodeInvalidArgument)
			}
		})
	}
}
