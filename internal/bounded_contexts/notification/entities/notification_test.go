package entities

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// newTestNotification 造一条最小可用的未读通知。
func newTestNotification(t *testing.T) *Notification {
	t.Helper()
	n, err := Notify(NotifyParams{
		UserID:   7,
		Kind:     value_objects.KindAnalysisCompleted,
		Title:    "分析完成：600519",
		Body:     "建议「买入」，置信度 82%。",
		SourceID: "task-1",
		LinkType: value_objects.LinkAnalysisTask,
		LinkID:   "task-1",
	})
	if err != nil {
		t.Fatalf("构造通知失败: %v", err)
	}
	return n
}

// ===========================================================================
// 已读是一次性的翻转，不是一个可以反复写入的字段
// ===========================================================================

func TestMarkRead_首次标记成功(t *testing.T) {
	n := newTestNotification(t)
	if n.IsRead() {
		t.Fatal("新建的通知不该是已读")
	}

	before := time.Now()
	if err := n.MarkRead(); err != nil {
		t.Fatalf("首次标记已读不该失败: %v", err)
	}
	if !n.IsRead() {
		t.Fatal("标记后 IsRead 仍为 false")
	}
	if n.ReadAt == nil || n.ReadAt.Before(before) {
		t.Fatalf("已读时刻 = %v，期望不早于 %v", n.ReadAt, before)
	}
}

func TestMarkRead_对已读通知再次标记返回_Conflict(t *testing.T) {
	n := newTestNotification(t)
	if err := n.MarkRead(); err != nil {
		t.Fatalf("首次标记已读失败: %v", err)
	}
	firstReadAt := *n.ReadAt

	err := n.MarkRead()
	if err == nil {
		t.Fatal("对已读通知再次标记应当报错，静默成功会悄悄刷新首次已读时刻")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
		t.Fatalf("错误码 = %s，期望 %s", code, custom_errors.CodeConflict)
	}
	// 关键断言：失败的那次不能有任何副作用。已读时刻是不可改写的事实。
	if !n.ReadAt.Equal(firstReadAt) {
		t.Fatalf("首次已读时刻被覆盖：%v -> %v", firstReadAt, *n.ReadAt)
	}
}

func TestMarkUnread_对未读通知标记未读返回_Conflict(t *testing.T) {
	n := newTestNotification(t)

	err := n.MarkUnread()
	if err == nil {
		t.Fatal("对未读通知标记未读应当报错：那是前端状态错乱的信号，不该被吞掉")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
		t.Fatalf("错误码 = %s，期望 %s", code, custom_errors.CodeConflict)
	}
}

func TestMarkUnread_对已读通知可以恢复未读(t *testing.T) {
	n := newTestNotification(t)
	if err := n.MarkRead(); err != nil {
		t.Fatalf("标记已读失败: %v", err)
	}
	if err := n.MarkUnread(); err != nil {
		t.Fatalf("恢复未读不该失败: %v", err)
	}
	if n.IsRead() || n.ReadAt != nil {
		t.Fatalf("恢复未读后 ReadAt = %v，期望 nil", n.ReadAt)
	}
}

// ===========================================================================
// 构造：去重键由聚合推导，调用方无从插手
// ===========================================================================

func TestNotify_去重键由种类与来源推导(t *testing.T) {
	n := newTestNotification(t)
	want, err := value_objects.NewDedupeKey(value_objects.KindAnalysisCompleted, "task-1")
	if err != nil {
		t.Fatalf("构造期望键失败: %v", err)
	}
	if n.DedupeKey != want {
		t.Fatalf("去重键 = %q，期望 %q", n.DedupeKey.String(), want.String())
	}
}

func TestNotify_级别留空时按种类取默认值(t *testing.T) {
	cases := []struct {
		kind value_objects.NotificationKind
		want value_objects.NotificationLevel
	}{
		{kind: value_objects.KindAnalysisCompleted, want: value_objects.LevelInfo},
		{kind: value_objects.KindAnalysisFailed, want: value_objects.LevelWarning},
		{kind: value_objects.KindSyncFailed, want: value_objects.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			n, err := Notify(NotifyParams{
				UserID: 1, Kind: tc.kind, Title: "标题", SourceID: "src-1",
			})
			if err != nil {
				t.Fatalf("构造失败: %v", err)
			}
			if n.Level != tc.want {
				t.Fatalf("级别 = %s，期望 %s", n.Level, tc.want)
			}
		})
	}
}

func TestNotify_拒绝构造不出通知的输入(t *testing.T) {
	cases := []struct {
		name   string
		params NotifyParams
	}{
		{
			name:   "没有收件人",
			params: NotifyParams{Kind: value_objects.KindSystem, Title: "标题", SourceID: "s-1"},
		},
		{
			name:   "种类非法",
			params: NotifyParams{UserID: 1, Kind: value_objects.NotificationKind("bogus"), Title: "标题", SourceID: "s-1"},
		},
		{
			name:   "标题为空",
			params: NotifyParams{UserID: 1, Kind: value_objects.KindSystem, Title: "   ", SourceID: "s-1"},
		},
		{
			name:   "没有来源标识就没有去重键",
			params: NotifyParams{UserID: 1, Kind: value_objects.KindSystem, Title: "标题"},
		},
		{
			name: "声明了跳转类型却没给目标",
			params: NotifyParams{
				UserID: 1, Kind: value_objects.KindAnalysisCompleted, Title: "标题",
				SourceID: "s-1", LinkType: value_objects.LinkAnalysisTask,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Notify(tc.params); err == nil {
				t.Fatal("期望构造失败")
			} else if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码 = %s，期望 %s", code, custom_errors.CodeInvalidArgument)
			}
		})
	}
}

func TestOwnedBy(t *testing.T) {
	n := newTestNotification(t)
	if !n.OwnedBy(7) {
		t.Fatal("应当属于用户 7")
	}
	if n.OwnedBy(8) {
		t.Fatal("不该属于用户 8")
	}
	// 未登录（0）不该匹配上任何通知，哪怕是一条脏数据里 user_id 为 0 的行。
	if (&Notification{UserID: 0}).OwnedBy(0) {
		t.Fatal("user_id 为 0 不该被判定为归属成立")
	}
}
