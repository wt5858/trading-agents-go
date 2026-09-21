// Package repositories 承载通知上下文的持久化。
//
// 本层是本上下文唯一允许出现事务的地方，也是唯一知道 GORM 存在的地方。
// 对上的唯一契约是：只抛 custom_errors.Error，绝不把 gorm / driver 的错误类型漏出去——
// 一旦 gorm.ErrRecordNotFound 漏到接口层，「通知不存在」会被映射成 500。
//
// 每个聚合根一个仓储：NotificationRepository 管 Notification，本上下文也只有它一个聚合根。
package repositories

import (
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// mysqlDuplicateEntry 是 MySQL 唯一键冲突的错误号（ER_DUP_ENTRY）。
const mysqlDuplicateEntry = 1062

// translate 是技术错误到领域错误的边界。
//
// 其中 1062 → AlreadyExists 这一条是本上下文最关键的一行翻译：
// 事件处理器正是靠它判定「这条通知已经投过了」，从而把一次事件重放
// 安静地变成无操作。翻错（比如吞成 nil 或者笼统地报 Internal）的后果分别是
// 「重复通知」与「每次正常重放都刷一条 ERROR 日志」。
func translate(err error, subject string) error {
	if err == nil {
		return nil
	}
	// 已经是领域错误就原样返回：我们自己抛的 custom_errors 不该被降级成 Internal。
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de
	}
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return custom_errors.NotFound("%s 不存在", subject)
	case isDuplicateKey(err):
		return custom_errors.AlreadyExists("%s 已存在", subject).Wrap(err)
	default:
		return custom_errors.Internal("%s 数据库操作失败", subject).Wrap(err)
	}
}

func translatef(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return translate(err, fmt.Sprintf(format, args...))
}

// isDuplicateKey 三重判定：优先用 gorm 的规范化错误，其次直接看驱动错误号，
// 最后才退回字符串匹配。都留着是因为 TranslateError 只作用于 gorm 自己发起的调用，
// 手写 Exec / 原生 SQL 路径拿到的仍是裸驱动错误。
//
// 这里宁可多判几次也不能漏判：漏判一次唯一键冲突，事件重放就会从「幂等的无操作」
// 变成「处理器报错 + 总线告警 + 将来的无限重投」。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var me *driver.MySQLError
	if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
		return true
	}
	return strings.Contains(err.Error(), "Error 1062") || strings.Contains(err.Error(), "Duplicate entry")
}
