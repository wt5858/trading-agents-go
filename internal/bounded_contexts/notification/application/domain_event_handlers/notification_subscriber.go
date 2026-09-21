// Package domain_event_handlers 把别的上下文发出的领域事件接进站内通知上下文。
//
// 和 http_handlers 一样，这里只做「翻译 + 转调」：解事件、取字段、拼文案、调用
// domain_service。通知长什么样、去重键怎么算、什么算已读，一条都不在这里。
//
// 本包自己拿主意的只有两件事，它们都不是业务规则：
//
//	投递语义——一次重放到底算失败还是算成功；
//	降级策略——通知发不出去时，要不要把错误捅回给事件总线。
//
// 两者都是「事件总线保证什么」的问题，因此属于 application/ 而不是领域层。
package domain_event_handlers

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	analysis_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	scheduling_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_events"
	stock_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_events"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// maxReasonRunes 是失败原因进入通知正文时的截断长度。
//
// 上游的 Reason 可能是一段几百行的堆栈或一整个 HTTP 响应体。原样塞进通知，
// 结果是聚合的正文长度校验直接拒绝这条通知——用户于是在最需要知道
// 「我的分析失败了」的时候什么都收不到。截断是有损的，但比不送达好得多，
// 完整原因本来就该去任务详情页看。
const maxReasonRunes = 120

// Notifier 是本包所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *domain_services.NotificationService：本包的全部价值
// 就在「重放算不算成功」「失败要不要上抛」这两个判断上，而那正是最需要被测试
// 覆盖的几行。依赖具体服务意味着测一次重放要先有一个数据库。
type Notifier interface {
	Notify(ctx context.Context, in domain_services.NotifyInput) (*entities.Notification, error)
}

// Subscriber 是本包需要的订阅能力，由消费方声明，便于在测试里替换。
type Subscriber interface {
	RegisterSubscriber(h domain_event.Handler, prototype domain_event.DomainEvent)
}

// NotificationSubscriber 是通知上下文与其余上下文之间的那根线。
//
// 做成**一个**订阅者而不是每个事件一个处理器类型：这些处理器共享同一套投递语义
// （重放即成功）与同一套降级策略，拆开只会让那套语义被复制四遍，然后在第五个事件
// 接进来时被复制第五遍——而复制品总有一份会写错。
//
// 各个上下文与通知之间没有任何共享事务：业务方自己落库并发出事件，通知在这里
// 独立落库。代价是最终一致（通知会比事实晚几毫秒出现），换来的是任何一侧的故障
// 都不会回滚另一侧——一次通知写入失败不该让一个跑了十几分钟的分析任务作废。
type NotificationSubscriber struct {
	notifier Notifier
	log      *zap.Logger
}

func NewNotificationSubscriber(notifier Notifier, log *zap.Logger) *NotificationSubscriber {
	if log == nil {
		log = zap.NewNop()
	}
	return &NotificationSubscriber{notifier: notifier, log: log}
}

// Register 把本订阅者关心的全部事件挂到总线上。
//
// 这里是本上下文唯一一处「我消费了谁」的清单。放在一个方法里，是为了让
// 「通知模块现在会对哪些事情做出反应」成为一个一眼能读完的问题——
// 散在四个文件的 init() 里，这个问题就得靠全局搜索来回答。
func (s *NotificationSubscriber) Register(bus Subscriber) {
	bus.RegisterSubscriber(s.OnTaskCompleted, &analysis_events.OnTaskCompleted{})
	bus.RegisterSubscriber(s.OnTaskFailed, &analysis_events.OnTaskFailed{})
	bus.RegisterSubscriber(s.OnSyncFailed, &stock_events.OnSyncFailed{})
	bus.RegisterSubscriber(s.OnJobAutoPaused, &scheduling_events.OnJobAutoPaused{})
}

// ---------------------------------------------------------------------------
// 面向用户的事件
// ---------------------------------------------------------------------------

// OnTaskCompleted 在分析任务成功收尾时给发起人发一条完成通知。
//
// 收件人就是事件里的 UserID——这条事件的确知道「是谁的任务」，所以不需要
// 任何猜测。这与下面两个运维事件形成对照。
func (s *NotificationSubscriber) OnTaskCompleted(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*analysis_events.OnTaskCompleted)
	if !ok {
		// 订阅错了事件名是装配错误，不是运行期故障：静默忽略，别把总线搞崩。
		return nil
	}
	return s.deliver(ctx, e, domain_services.NotifyInput{
		UserID: evt.UserID,
		Kind:   value_objects.KindAnalysisCompleted,
		Level:  value_objects.LevelInfo,
		Title:  fmt.Sprintf("分析完成：%s", displaySymbol(evt.Symbol)),
		// 置信度在这里乘 100 只是**格式化**，不是重算：事件里带的就是这个既成事实，
		// 换个写法展示不会让它变成另一个数。真正禁止的是拿别的字段去推导它。
		//
		// 事件里的两个数是字符串（见 analysis 的 OnTaskCompleted），
		// 解析回 decimal 再格式化，全程不经过 float64。
		Body: fmt.Sprintf("建议「%s」，置信度 %s%%，耗时 %s 秒。",
			analysis_vo.Action(evt.Action).DisplayName(),
			decimalx.FromString(evt.Confidence).Mul(decimal.NewFromInt(100)).StringFixed(0),
			decimalx.FromString(evt.DurationS).StringFixed(0)),
		// 去重键的来源是任务 ID：同一个任务无论事件被重投多少次，都只有一条完成通知。
		SourceID: evt.TaskID,
		LinkType: value_objects.LinkAnalysisTask,
		LinkID:   evt.TaskID,
	})
}

// OnTaskFailed 在分析任务最终失败时给发起人发一条告警通知。
//
// # 为什么可重试的失败不发通知
//
// Retryable 为真意味着这次失败还会被自动重试，很可能下一次就成功了。
// 为它发通知有两重坏处：用户被一件系统自己能解决的事打扰；更糟的是，
// 去重键只认 (kind, taskID)，第一次失败写进去之后，后续真正的**最终**失败
// 会撞上唯一键而被当成重放丢弃——于是用户手上留着一条「失败」，
// 而任务其实早已重试成功，或者反过来。只在终局发通知，两种错位都不存在。
func (s *NotificationSubscriber) OnTaskFailed(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*analysis_events.OnTaskFailed)
	if !ok {
		return nil
	}
	if evt.Retryable {
		return nil
	}
	return s.deliver(ctx, e, domain_services.NotifyInput{
		UserID:   evt.UserID,
		Kind:     value_objects.KindAnalysisFailed,
		Level:    value_objects.LevelWarning,
		Title:    "分析任务失败",
		Body:     fmt.Sprintf("任务执行失败（已尝试 %d 次）：%s", evt.Attempts, truncate(evt.Reason, maxReasonRunes)),
		SourceID: evt.TaskID,
		LinkType: value_objects.LinkAnalysisTask,
		LinkID:   evt.TaskID,
	})
}

// ---------------------------------------------------------------------------
// 面向运维的事件
// ---------------------------------------------------------------------------

// OnSyncFailed 在数据同步整体失败时告警。
//
// ===========================================================================
// 这条事件目前**不产生站内通知**，只落一条 WARN 日志。这是刻意的。
// ===========================================================================
//
// 站内通知必须有收件人：user_id 是 notifications 表的必填列，也是去重唯一索引的
// 第一列。而 OnSyncFailed 的载荷是 (RunID, Kind, Market, Reason)——它压根不知道
// 是谁触发的这次同步，事实上通常没有人触发，它来自定时任务。
//
// 于是只剩三条路，前两条都不能走：
//
//	编一个 user_id（比如 1，或者「第一个管理员」）——这是在往一张以 user_id 为
//	    唯一索引前缀的表里写一个**猜出来的**主体。猜错了就是把运维告警塞进某个
//	    真实用户的通知列表，而且因为去重键固定，他还删不干净。
//	发给所有管理员——本项目此刻没有「广播」这个概念：没有收件人集合的建模，
//	    没有批量投递的入口，也没有「管理员变更时历史广播怎么办」的答案。
//	    为了一条告警临时造一个半成品广播机制，比不发更糟。
//	记日志并如实说明——也就是现在这条路。
//
// 日志不是敷衍：同步失败本来就是运维告警的范畴（Prometheus / 日志告警都订得到
// 这条 WARN），而站内通知是给**用户**看的东西。等真正引入管理员广播（一个
// user_id 为 NULL、按角色投递的通知类型，或者一张 admin_broadcasts 表）之后，
// 这里换成一次 deliver 调用即可，事件本身不需要任何改动。
func (s *NotificationSubscriber) OnSyncFailed(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*stock_events.OnSyncFailed)
	if !ok {
		return nil
	}
	s.log.Warn("数据同步失败（暂无站内通知收件人，仅记录日志）",
		zap.String("event", e.Name()),
		zap.String("event_id", e.EventID()),
		zap.String("run_id", evt.RunID),
		zap.String("kind", evt.Kind),
		zap.String("market", evt.Market),
		zap.String("reason", evt.Reason))
	return nil
}

// OnJobAutoPaused 在定时任务因连续失败被自动熔断时告警。
//
// 收件人的问题与 OnSyncFailed 完全相同：载荷是 (JobID, JobName, Kind,
// ConsecutiveFailures, LastReason)，没有任何用户身份，而定时任务本就不属于某个人。
// 同样不编造 user_id，理由见上。
//
// 这条日志用 Warn 而不是 Info，是因为熔断这件事**本身是安静的**：从那一刻起
// 任务不再执行，也就不再产生失败日志，没人去看的话它可以静静躺上几个月
// （scheduling 上下文在事件定义处写下的正是这个担忧）。在接入广播之前，
// 这条日志是它唯一的出口，必须足以触发日志告警规则。
func (s *NotificationSubscriber) OnJobAutoPaused(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*scheduling_events.OnJobAutoPaused)
	if !ok {
		return nil
	}
	s.log.Warn("定时任务已被自动熔断（暂无站内通知收件人，仅记录日志）",
		zap.String("event", e.Name()),
		zap.String("event_id", e.EventID()),
		zap.String("job_id", evt.JobID),
		zap.String("job_name", evt.JobName),
		zap.String("kind", evt.Kind),
		zap.Int("consecutive_failures", evt.ConsecutiveFailures),
		zap.String("last_reason", evt.LastReason))
	return nil
}

// ---------------------------------------------------------------------------
// 投递
// ---------------------------------------------------------------------------

// deliver 是全部通知投递共用的那一段，也是本包唯一真正做决定的地方。
//
// # 幂等：重放不能报错，也不能产出第二条通知
//
// 领域事件是**至少一次**投递的：总线重启后的补投、worker 可见性超时导致的重复
// 执行、以后换成 MQ 后的重投，都会让同一个事件到达这里不止一次。
//
// 这两件事被压到同一个东西上：(user_id, dedupe_key) 唯一索引。
//
//   - 不能产出第二条：第二次 INSERT 撞唯一键，数据库直接拒绝。
//     整条链路上刻意没有「先查有没有」——两次重放并发进来时，先查再插的两边
//     都会查到「还没有」，然后双双插入；唯一索引才是并发下真正成立的那个保证。
//   - 不能报错：仓储把 1062 翻成 AlreadyExists，这里把它当成功。
//     语义上这是对的——「这个事件的通知已经存在」正是本处理器想达成的状态，
//     谁把它写进去的并不重要。若照实返回错误，总线会把每一次正常重放
//     都记成一条处理失败的告警，真正的故障反而被淹没在噪音里。
//
// # 降级：通知失败绝不能弄坏引发它的业务操作
//
// 事件到达这里时，业务事实**已经落库**了：分析结果存好了，任务状态改完了。
// 通知只是这件事的一个副作用。因此这里按「重投还有没有意义」把错误分成两类：
//
//	不可能因重试而变好的（参数非法、文案超长、去重键构造失败）——记 ERROR 日志
//	    并返回 nil。返回错误只会换来无止境的重投，而它每次都会以同样的方式失败。
//	可能是暂时性的（数据库不可用、连接超时）——如实返回，让总线记录下来，
//	    也让将来换上 MQ 之后的重试策略有机会真的重投一次。
//
// 无论哪一类，业务操作都不会被回滚——InProcessBus.Publish 本身就只记录不传播。
// 这里的分类是为了让**日志里的每一条失败都值得看**，而不是为了控制事务。
func (s *NotificationSubscriber) deliver(ctx context.Context, e domain_event.DomainEvent, in domain_services.NotifyInput) error {
	if in.UserID == 0 {
		// 兜底：绝不把一条没有收件人的通知交给下游去猜。走到这里说明上游事件
		// 的 UserID 是空的，那是发布方的问题，记下来但不要让它变成一次失败重试。
		s.log.Warn("事件缺少收件人，跳过站内通知",
			zap.String("event", e.Name()),
			zap.String("event_id", e.EventID()))
		return nil
	}

	_, err := s.notifier.Notify(ctx, in)
	if err == nil {
		return nil
	}

	switch custom_errors.CodeOf(err) {
	case custom_errors.CodeAlreadyExists:
		// 事件重放，正是我们想要的状态。安静地当成成功。
		return nil
	case custom_errors.CodeInvalidArgument, custom_errors.CodeNotFound,
		custom_errors.CodeUnauthorized, custom_errors.CodeForbidden, custom_errors.CodeConflict:
		// 重投一百次结果也一样，别让它变成无限重试的噪音源；但必须留下痕迹，
		// 否则「用户没收到通知」会成为一个完全无从查起的问题。
		s.log.Error("站内通知构造失败，已跳过",
			zap.String("event", e.Name()),
			zap.String("event_id", e.EventID()),
			zap.Uint64("user_id", in.UserID),
			zap.String("kind", in.Kind.String()),
			zap.Error(err))
		return nil
	default:
		// Internal / Unavailable：可能只是数据库抖了一下，值得重投。
		return err
	}
}

// ---------------------------------------------------------------------------
// 文案小工具
// ---------------------------------------------------------------------------

// truncate 按**字符数**截断，不是字节数。
//
// 用 len() 切中文会把一个三字节的汉字切成半个，正文里从此出现一个乱码方块，
// 而它还会被当成合法 UTF-8 存进库里。
func truncate(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "未提供失败原因"
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}

// displaySymbol 兜一个空代码。事件理论上不会缺这个字段，但通知的标题是
// 「分析完成：」这种半截话时，用户会以为是系统坏了而不是数据缺失。
func displaySymbol(symbol string) string {
	if s := strings.TrimSpace(symbol); s != "" {
		return s
	}
	return "未知标的"
}
