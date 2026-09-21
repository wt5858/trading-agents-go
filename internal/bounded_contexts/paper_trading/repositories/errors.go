// Package repositories 承载模拟交易上下文的持久化。
//
// 本层是全上下文唯一允许出现事务的地方，也是唯一知道 GORM 存在的地方。
// 对上的唯一契约是：只抛 custom_errors.Error，绝不把 gorm / driver 的错误漏出去——
// 一旦 gorm.ErrRecordNotFound 漏到接口层，「账户不存在」就会被映射成 500。
//
// 只有聚合根配仓储：PaperAccountRepository 管 PaperAccount。
// 持仓与成交虽然各有一张表，但它们的读写完全是本仓储的内部细节，
// 外界没有任何直接操作它们的入口。
package repositories

import (
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// mysqlDuplicateEntry 是 MySQL 唯一键冲突错误号（ER_DUP_ENTRY）。
const mysqlDuplicateEntry = 1062

// translate 是技术错误到领域错误的边界。
func translate(err error, subject string) error {
	if err == nil {
		return nil
	}
	// 已经是领域错误就原样返回：我们自己抛的 Conflict 不该被降级成 Internal，
	// 否则乐观锁冲突会变成 500，调用方也就失去了重试的依据。
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
