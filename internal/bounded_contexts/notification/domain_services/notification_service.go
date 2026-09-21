package domain_services

import (
	"context"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// NotificationService 编排站内通知的全部用例。
//
// 依赖仓储的具体类型而不是接口：仓储在本项目里只有一个实现，
// 多声明一层接口既不能换实现，又让「改一个方法要动三个文件」。
// （需要替身的是**调用方**——事件处理器在自己那一侧声明窄接口，见 application/。）
type NotificationService struct {
	notifications *repositories.NotificationRepository
}

func NewNotificationService(notifications *repositories.NotificationRepository) *NotificationService {
	return &NotificationService{notifications: notifications}
}

// ---------------------------------------------------------------------------
// 写入（内部用例）
// ---------------------------------------------------------------------------

// NotifyInput 是创建一条通知的入参。
//
// 字段是值对象而不是裸字符串，因为调用方是**本上下文自己的事件处理器**——
// 它们和本层在同一个限界上下文里，手上拿得到 value_objects 的常量，
// 没有理由先把 KindAnalysisCompleted 转成 "analysis_completed" 再在这里解析回去。
// 需要解析裸字符串的是 HTTP 入口，而通知没有对外的创建接口（见 Notify 的注释）。
type NotifyInput struct {
	UserID uint64
	Kind   value_objects.NotificationKind
	// Level 留零值表示按种类的默认级别，由聚合决定，见 entities.Notify。
	Level value_objects.NotificationLevel
	Title string
	Body  string

	// SourceID 是来源聚合的标识，去重键由 (Kind, SourceID) 推导。
	SourceID string

	LinkType value_objects.LinkType
	LinkID   string
}

// Notify 创建一条通知。**这是一个内部用例，没有对应的 HTTP 接口。**
//
// # 为什么不收 Operator
//
// 因为它的调用方不是人。通知由别的上下文的领域事件触发，事件里没有「谁在调用」
// 这个概念——OnTaskCompleted 是一件已经发生的事实，不是某个用户发起的请求。
// 硬塞一个 Operator 进来，只会让事件处理器不得不编造一个身份出来。
//
// 而对外不开放创建接口是刻意的：一个 POST /notifications 等于允许任何登录用户
// 给任意 user_id 塞通知，那是一个现成的骚扰与钓鱼通道。通知的来源必须是系统事实。
//
// # 返回 AlreadyExists 时该怎么办
//
// 这不是本层能决定的。同一个去重键第二次进来，可能是一次良性的事件重放，
// 也可能是同一件事被处理了两遍的 bug。本层如实把仓储的结论传上去，
// 由事件处理器按它所知的**投递语义**决定算不算成功——见
// application/domain_event_handlers。本层不替它做这个判断，否则任何一个
// 真正的重复消费 bug 都会被这里静默吞掉。
func (s *NotificationService) Notify(ctx context.Context, in NotifyInput) (*entities.Notification, error) {
	// 全部校验与去重键推导都在聚合里完成，本层一条都不重复判。
	n, err := entities.Notify(entities.NotifyParams{
		UserID:   in.UserID,
		Kind:     in.Kind,
		Level:    in.Level,
		Title:    in.Title,
		Body:     in.Body,
		SourceID: in.SourceID,
		LinkType: in.LinkType,
		LinkID:   in.LinkID,
	})
	if err != nil {
		return nil, err
	}
	if err := s.notifications.Create(ctx, n); err != nil {
		return nil, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// ListMine 分页列出当前用户的通知，rawStatus 为空表示不限已读状态。
//
// 没有「管理员看别人的通知」这个用例，因此也没有一个 targetUserID 参数
// （对比 analysis.ListByUser：那边管理员确实需要排查别人的任务）。
// 通知是个人数据，不是可运维的对象——与 watchlist 同一条口径。
func (s *NotificationService) ListMine(
	ctx context.Context,
	op Operator,
	rawStatus string,
	page shared_vo.Page,
) ([]*entities.Notification, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	// 形状校验属于本层：非法的筛选值要在打到数据库之前被挡掉，
	// 而不是变成一句「查不出任何通知」的哑故障。
	status, err := value_objects.NewReadStatus(rawStatus)
	if err != nil {
		return nil, 0, err
	}
	return s.notifications.ListByUser(ctx, op.UserID, status, page)
}

// UnreadCount 返回当前用户的未读条数，即前端红点上的数字。
func (s *NotificationService) UnreadCount(ctx context.Context, op Operator) (int64, error) {
	if err := requireLogin(op); err != nil {
		return 0, err
	}
	return s.notifications.CountUnread(ctx, op.UserID)
}

// ---------------------------------------------------------------------------
// 状态变更
// ---------------------------------------------------------------------------

// MarkRead 把一条通知标为已读。
//
// 这里没有「先加载通知、判断是不是我的、再调 n.MarkRead()、最后保存」。
// 归属与「当前未读」两个条件都被交给仓储写进同一条 UPDATE 的 WHERE 里：
// 那既避免了 check-then-act，也让两个客户端同时点开同一条通知时
// 首次已读时刻不会被覆盖。详见 repositories.NotificationRepository.MarkRead。
//
// 归属不符时仓储返回的是 NotFound 而不是 Forbidden：Forbidden 等于确认
// 「这个 id 确实存在」，会让通知 ID 空间变成可探测的信道。
func (s *NotificationService) MarkRead(ctx context.Context, op Operator, id uint64) error {
	if err := requireLogin(op); err != nil {
		return err
	}
	if id == 0 {
		return custom_errors.Invalid("通知 ID 不能为空")
	}
	return s.notifications.MarkRead(ctx, id, op.UserID, time.Now())
}

// MarkAllRead 一键已读，返回本次实际标记的条数。
//
// 一条 UPDATE 解决，绝不是「列出未读再逐条 MarkRead」——后者是 N+1，
// 而且中途新到的通知会被漏掉，红点不会归零。
func (s *NotificationService) MarkAllRead(ctx context.Context, op Operator) (int64, error) {
	if err := requireLogin(op); err != nil {
		return 0, err
	}
	return s.notifications.MarkAllRead(ctx, op.UserID, time.Now())
}

// Delete 删除当前用户的一条通知。
func (s *NotificationService) Delete(ctx context.Context, op Operator, id uint64) error {
	if err := requireLogin(op); err != nil {
		return err
	}
	if id == 0 {
		return custom_errors.Invalid("通知 ID 不能为空")
	}
	// op.UserID 作为归属谓词交给仓储，而不是先查出来比对，理由同 MarkRead。
	return s.notifications.Delete(ctx, id, op.UserID)
}

// ---------------------------------------------------------------------------
// 运维
// ---------------------------------------------------------------------------

// PurgeOlderThan 清理 before 之前的历史通知，返回删除条数。仅管理员可调用。
//
// 这是本上下文唯一一个真正用到 op.IsAdmin 的地方，也是唯一一个跨用户的操作：
// 它删的是**所有人**的过期通知，因此判的不是归属而是角色。返回 Forbidden 而不是
// NotFound——这里没有「某个 ID 是否存在」的信息需要隐藏，如实告诉调用方
// 「你不够权限」才是能帮上忙的回答。
func (s *NotificationService) PurgeOlderThan(ctx context.Context, op Operator, before time.Time) (int64, error) {
	if err := requireLogin(op); err != nil {
		return 0, err
	}
	if !op.IsAdmin {
		return 0, custom_errors.Forbidden("仅管理员可清理历史通知")
	}
	if before.IsZero() {
		return 0, custom_errors.Invalid("清理时间点不能为空")
	}
	return s.notifications.PurgeOlderThan(ctx, before)
}
