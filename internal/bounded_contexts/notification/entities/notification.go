// Package entities 承载站内通知上下文的聚合。
//
// 本上下文只有一个聚合根：Notification。它刻意做得很小——一条通知没有子实体、
// 没有状态机（只有未读/已读这一次单向翻转）、也不聚合别的东西。
//
// 全部业务不变式都住在这里：标题不能为空、已读不能重复标记、去重键必须与来源一致。
// 上层（domain_services / application）一条都不重复判定。
package entities

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// MaxTitleRunes 按「字符数」而不是字节数计：一个 20 个汉字的标题是 60 字节，
	// 用 len() 去卡会把最常见的中文标题莫名其妙地拒掉。
	MaxTitleRunes = 64
	// MaxBodyRunes 同理。正文只是一句话摘要，详情去点链接看。
	MaxBodyRunes = 500
)

// Notification 是站内通知聚合根。
//
// ===========================================================================
// 它为什么只持有 (LinkType, LinkID) 而不是来源实体
// ===========================================================================
//
// 通知是「某一刻发生过某件事」的**存档**。持有 *analysis.Task 会带来两个后果：
// 编译期上 notification 依赖了 analysis 的 entities，两个上下文的边界当场消失；
// 运行期上用户三天后翻到这条通知，看到的是任务**现在**的状态，而正文停留在当时。
// 所以这里只按标识引用，详情由前端拿 LinkID 去调那个上下文自己的接口。
//
// ReadAt 用时刻而不是 bool：「什么时候读的」丢掉就补不回来，而它恰恰是排查
// 「红点为什么消失了」时唯一能查的线索。已读与否由 ReadAt == nil 判定，
// 不存在第二个真相来源，也就不可能出现 read=1 而 read_at 为空的脏数据。
type Notification struct {
	// EventRecorder 保持与本项目其余聚合一致的形状。
	// 本上下文目前不对外发布任何领域事件——它是事件的**终点**，通知送达之后
	// 没有下一个消费者。将来接入 WebSocket / App 推送时，推送触发点就挂在这里，
	// 而不是散落到各个调用方。
	domain_event.EventRecorder

	ID        uint64
	UserID    uint64
	Kind      value_objects.NotificationKind
	Level     value_objects.NotificationLevel
	Title     string
	Body      string
	DedupeKey value_objects.DedupeKey
	LinkType  value_objects.LinkType
	LinkID    string
	ReadAt    *time.Time
	CreatedAt time.Time
}

// NotifyParams 是创建一条通知所需的全部输入。
//
// 用结构体而不是八个位置参数：Title 与 Body 都是 string，LinkType 与 LinkID 相邻，
// 位置参数下把两者写反是编译器抓不到的错误，而它的表现是用户收到一条标题空白、
// 正文是标题的通知——没人会因此收到告警。
type NotifyParams struct {
	UserID uint64
	Kind   value_objects.NotificationKind
	Level  value_objects.NotificationLevel
	Title  string
	Body   string

	// SourceID 是**来源聚合**的标识（分析任务 ID、同步批次 ID、定时任务 ID）。
	// 去重键由 (Kind, SourceID) 推导，见 Notify。
	SourceID string

	LinkType value_objects.LinkType
	LinkID   string
}

// Notify 是通知进入系统的唯一入口。
//
// # 去重键在这里推导，而不是由调用方传进来
//
// 「去重键必须与 (Kind, 来源聚合) 一致」是一条不变式，因此它属于聚合。
// 如果开一个 dedupeKey 参数让调用方传，那么某个事件处理器迟早会传一个
// 掺了时间戳或事件 ID 的键——那条通知从此每次重放都会新增一条，
// 而唯一索引、仓储、处理器全都工作正常，问题只会以「用户收到三条重复通知」
// 的形式在几周后被发现。收进构造函数之后，这种键根本构造不出来。
func Notify(p NotifyParams) (*Notification, error) {
	if p.UserID == 0 {
		return nil, custom_errors.Invalid("通知必须有收件人")
	}
	if !p.Kind.Valid() {
		return nil, custom_errors.Invalid("非法的通知种类: %s", p.Kind.String())
	}
	level := p.Level
	if level.IsZero() {
		// 留空回落到种类的默认级别是**约定**而不是静默降级：调用方明确表达了
		// 「按这类通知的常规严重程度来」。传了一个非法值才是错误，见下一行。
		level = p.Kind.DefaultLevel()
	}
	if !level.Valid() {
		return nil, custom_errors.Invalid("非法的通知级别: %s", level.String())
	}

	title := strings.TrimSpace(p.Title)
	if title == "" {
		return nil, custom_errors.Invalid("通知标题不能为空")
	}
	if n := utf8.RuneCountInString(title); n > MaxTitleRunes {
		return nil, custom_errors.Invalid("通知标题不能超过 %d 个字符，当前 %d 个", MaxTitleRunes, n)
	}
	body := strings.TrimSpace(p.Body)
	if n := utf8.RuneCountInString(body); n > MaxBodyRunes {
		return nil, custom_errors.Invalid("通知正文不能超过 %d 个字符，当前 %d 个", MaxBodyRunes, n)
	}

	dedupeKey, err := value_objects.NewDedupeKey(p.Kind, p.SourceID)
	if err != nil {
		return nil, err
	}

	linkID := strings.TrimSpace(p.LinkID)
	switch {
	case p.LinkType.IsZero():
		// 没有跳转类型就不该留着一个孤零零的 ID：前端拿它也拼不出路由。
		linkID = ""
	case linkID == "":
		return nil, custom_errors.Invalid("通知声明了跳转类型 %s 却没有给出目标标识", p.LinkType.String())
	}

	return &Notification{
		UserID:    p.UserID,
		Kind:      p.Kind,
		Level:     level,
		Title:     title,
		Body:      body,
		DedupeKey: dedupeKey,
		LinkType:  p.LinkType,
		LinkID:    linkID,
		CreatedAt: time.Now(),
	}, nil
}

// IsRead 判断是否已读。已读与否只有 ReadAt 这一个真相来源。
func (n *Notification) IsRead() bool { return n.ReadAt != nil }

// OwnedBy 判断通知是否属于某个用户。
//
// 它只回答「是不是」，不抛错：该抛 NotFound 还是 Forbidden 是「谁能调用」的问题，
// 属于 domain_services，不属于聚合。
func (n *Notification) OwnedBy(userID uint64) bool { return n.UserID == userID && userID != 0 }

// MarkRead 把通知标为已读。
//
// 重复标记返回 Conflict 而不是静默成功：已读时刻是不可改写的事实，
// 把第二次标记当成功就意味着 ReadAt 被悄悄刷新，「用户什么时候第一次看到这条通知」
// 从此不可考。这与 User.Deactivate 对已停用账号返回 Conflict 是同一条理由。
//
// 注意：真正在并发下保证「只翻转一次」的不是这里，而是仓储那条
// `WHERE id=? AND user_id=? AND read_at IS NULL` 的条件 UPDATE。
// 本方法负责的是**单个聚合实例内**的语义正确，以及给出一句人话的错误。
func (n *Notification) MarkRead() error {
	if n.IsRead() {
		return custom_errors.Conflict("通知已是已读状态")
	}
	now := time.Now()
	n.ReadAt = &now
	return nil
}

// MarkUnread 把通知恢复为未读，供用户「标记为稍后再看」。
//
// 同样拒绝无操作迁移：对一条本来就未读的通知点「标为未读」是前端状态错乱的信号，
// 静默成功会让这个错乱一直藏着。
func (n *Notification) MarkUnread() error {
	if !n.IsRead() {
		return custom_errors.Conflict("通知已是未读状态")
	}
	n.ReadAt = nil
	return nil
}
