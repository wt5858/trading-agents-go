package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// NotificationLevel 是通知的提示级别，决定前端的图标与配色。
//
// 刻意只有三档。再细分（success / debug / critical）听起来更精确，实际结果是
// 每个事件处理器都要为「这条算 warning 还是算 critical」做一次没有依据的判断，
// 而用户在通知中心里只关心一件事：这条要不要我现在去处理。
type NotificationLevel string

const (
	// LevelInfo 告知既成事实，不需要用户做任何事。
	LevelInfo NotificationLevel = "info"
	// LevelWarning 有东西没按预期完成，用户可能想去看一眼。
	LevelWarning NotificationLevel = "warning"
	// LevelError 有东西坏了并且不会自愈，需要人介入。
	LevelError NotificationLevel = "error"
)

// NewNotificationLevel 解析提示级别。
//
// 非法值报错而不是回落到 info：静默降级会把一条本该刺眼的错误通知变成一条
// 混在列表里的普通消息，而这种「降级」恰恰发生在最需要被看见的时候。
func NewNotificationLevel(s string) (NotificationLevel, error) {
	l := NotificationLevel(strings.TrimSpace(s))
	if l.Valid() {
		return l, nil
	}
	return "", custom_errors.Invalid("非法的通知级别: %s", s)
}

// RehydrateNotificationLevel 跳过校验，仅供从数据库重建使用。理由见 RehydrateNotificationKind。
func RehydrateNotificationLevel(s string) NotificationLevel { return NotificationLevel(s) }

func (l NotificationLevel) Valid() bool {
	switch l {
	case LevelInfo, LevelWarning, LevelError:
		return true
	}
	return false
}

func (l NotificationLevel) IsZero() bool { return l == "" }

func (l NotificationLevel) String() string { return string(l) }

func (l NotificationLevel) DisplayName() string {
	switch l {
	case LevelInfo:
		return "提示"
	case LevelWarning:
		return "警告"
	case LevelError:
		return "错误"
	}
	return "未知级别"
}
