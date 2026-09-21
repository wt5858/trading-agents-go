// Package dtos 是通知上下文的持久化形态。
//
// DTO 与它的两个方向的转换函数放在同一个文件里，本项目不设 mapper 包：
// 一个只有转换函数的包既没有自己的不变式，又让「加一个字段」变成要改三个文件的事，
// 而漏改的那一次不会有任何编译错误——字段只是安静地不再被保存。
//
// DTO 绝不离开 repositories/：仓储对上只吐 entities。一旦 DTO 流到领域服务，
// gorm 标签和列名就跟着流了过去，改一次表结构会波及整个上下文。
package dtos

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
)

// NotificationDto 是 notifications 表一行的形状。
type NotificationDto struct {
	ID     uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	UserID uint64 `gorm:"column:user_id;not null;uniqueIndex:uk_notifications_user_dedupe,priority:1;index:idx_notifications_user_read_created,priority:1"`

	Kind  string `gorm:"column:kind;type:varchar(32);not null"`
	Level string `gorm:"column:level;type:varchar(16);not null;default:info"`
	Title string `gorm:"column:title;type:varchar(128);not null;default:''"`
	Body  string `gorm:"column:body;type:varchar(1024);not null;default:''"`

	// DedupeKey 与 UserID 组成唯一索引，那是本上下文全部幂等保证的来源。
	DedupeKey string `gorm:"column:dedupe_key;type:varchar(160);not null;uniqueIndex:uk_notifications_user_dedupe,priority:2"`

	LinkType string `gorm:"column:link_type;type:varchar(32);not null;default:''"`
	LinkID   string `gorm:"column:link_id;type:varchar(64);not null;default:''"`

	// ReadAt 为 NULL 即未读。指针类型是必需的：值类型的 time.Time 零值会被写成
	// '0000-00-00'，那在 read_at IS NULL 的谓词下会被当成**已读**，未读红点直接失灵。
	ReadAt    *time.Time `gorm:"column:read_at;type:datetime(3);index:idx_notifications_user_read_created,priority:2"`
	CreatedAt time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false;index:idx_notifications_user_read_created,priority:3"`
}

func (NotificationDto) TableName() string { return "notifications" }

// ToDomain 重建聚合。
//
// 这里一律走 Rehydrate 而不走校验构造函数：已经落库的行是既成事实。
// 拿今天的规则去校验历史数据，会让一条早年写入的通知（比如某个已下线的 kind）
// 把整个通知列表接口打挂——而这是用户每次进首页都会拉的接口。校验属于写入路径。
func (dto NotificationDto) ToDomain() *entities.Notification {
	return &entities.Notification{
		ID:        dto.ID,
		UserID:    dto.UserID,
		Kind:      value_objects.RehydrateNotificationKind(dto.Kind),
		Level:     value_objects.RehydrateNotificationLevel(dto.Level),
		Title:     dto.Title,
		Body:      dto.Body,
		DedupeKey: value_objects.RehydrateDedupeKey(dto.DedupeKey),
		LinkType:  value_objects.RehydrateLinkType(dto.LinkType),
		LinkID:    dto.LinkID,
		ReadAt:    dto.ReadAt,
		CreatedAt: dto.CreatedAt,
	}
}

// FromDomainNotification 把聚合摊平成一行。
func FromDomainNotification(n *entities.Notification) *NotificationDto {
	return &NotificationDto{
		ID:        n.ID,
		UserID:    n.UserID,
		Kind:      n.Kind.String(),
		Level:     n.Level.String(),
		Title:     n.Title,
		Body:      n.Body,
		DedupeKey: n.DedupeKey.String(),
		LinkType:  n.LinkType.String(),
		LinkID:    n.LinkID,
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

// ToDomainNotifications 批量重建，供列表查询使用。
func ToDomainNotifications(rows []NotificationDto) []*entities.Notification {
	out := make([]*entities.Notification, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
