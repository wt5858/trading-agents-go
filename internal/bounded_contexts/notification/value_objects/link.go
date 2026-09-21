package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// LinkType 标明通知指向的来源聚合属于哪个上下文的哪种聚合。
//
// ===========================================================================
// 这里是「跨上下文只按 ID 引用」这条规则的落点
// ===========================================================================
//
// 通知天然想说「点我跳到那个分析任务」。最省事的实现是让 Notification 持有一个
// *analysis.Task，那样标题、状态、股票代码全都现成。这条路本上下文明确不走：
//
//   - 编译期依赖：notification 会依赖 analysis 的 entities，两个上下文的边界当场消失，
//     analysis 改一次聚合字段，通知模块跟着重新编译甚至改写。
//   - 运行期错觉：通知是「某一刻发生过某件事」的存档。持有实体等于持有一个
//     还会继续变化的对象——用户三天后翻到这条通知，看到的会是任务**现在**的状态，
//     而通知的正文还停留在当时，两者当场打架。
//
// 所以这里只存 (LinkType, LinkID) 这一对标量：类型告诉前端该拼哪条路由，
// ID 告诉它拼哪一个。需要详情就去调那个上下文自己的接口——那才是它的职责。
type LinkType string

const (
	// LinkNone 表示这条通知没有可跳转的来源（系统公告）。
	LinkNone LinkType = ""
	// LinkAnalysisTask 指向 analysis 上下文的 Task 聚合。
	LinkAnalysisTask LinkType = "analysis_task"
	// LinkSyncRun 指向 stock 上下文的 SyncRun 聚合。
	LinkSyncRun LinkType = "sync_run"
	// LinkScheduledJob 指向 scheduling 上下文的 ScheduledJob 聚合。
	LinkScheduledJob LinkType = "scheduled_job"
)

// NewLinkType 解析跳转类型。空串合法，表示「无跳转」。
func NewLinkType(s string) (LinkType, error) {
	t := LinkType(strings.TrimSpace(s))
	switch t {
	case LinkNone, LinkAnalysisTask, LinkSyncRun, LinkScheduledJob:
		return t, nil
	}
	return "", custom_errors.Invalid("非法的通知跳转类型: %s", s)
}

// RehydrateLinkType 跳过校验，仅供从数据库重建使用。理由见 RehydrateNotificationKind。
func RehydrateLinkType(s string) LinkType { return LinkType(s) }

func (t LinkType) IsZero() bool { return t == LinkNone }

func (t LinkType) String() string { return string(t) }
