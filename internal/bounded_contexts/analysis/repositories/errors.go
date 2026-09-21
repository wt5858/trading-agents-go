// Package repositories 承载分析上下文的持久化：聚合仓储与外部存储适配。
//
// 本层是全系统唯一允许出现事务的地方，也是唯一知道 GORM 与 Redis 存在的地方。
// 对上的唯一契约是：只抛 custom_errors.Error，绝不把 gorm / redis 的错误类型漏出去——
// 一旦 gorm.ErrRecordNotFound 漏到接口层，「任务不存在」就会被映射成 500。
//
// 每个聚合根一个仓储：TaskRepository 管 Task，BatchRepository 管 Batch。
// 没有任何一个方法会在同一个事务里同时写这两张表。
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

// translate 是技术错误到领域错误的边界。上层永远看不到 gorm 或 driver 的类型。
// subject 是「主语」，用于拼出可读消息，例如 "分析任务(id=xx) 不存在"。
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
		// 冲突原因（哪个唯一键）对调用方没有意义，但对排查有意义，所以 Wrap 保留原始错误。
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
