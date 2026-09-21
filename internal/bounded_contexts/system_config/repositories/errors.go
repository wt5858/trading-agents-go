package repositories

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// translate 是技术错误与领域错误的边界。本层之上不会看到任何 gorm 或驱动类型。
func translate(err error, action string) error {
	if err == nil {
		return nil
	}
	var me *mysql.MySQLError
	// 1062 是唯一索引冲突。把它翻译成 AlreadyExists，正是「不做先查再插」
	// 这个决定得以成立的前提：唯一索引是真正的保证，这里只负责让它说人话。
	if errors.As(err, &me) && me.Number == 1062 {
		return custom_errors.AlreadyExists("%s失败：记录已存在", action).Wrap(err)
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return custom_errors.NotFound("%s失败：记录不存在", action).Wrap(err)
	}
	return custom_errors.Internal("%s失败", action).Wrap(err)
}

// asDomainError 在事务回调里保留已经构造好的领域错误码。
// 不这么做的话，事务包装会把一个 Conflict 冲刷成 Internal，
// 调用方就没法把「状态已变更」和「数据库挂了」区分开。
func asDomainError(err error, action string) error {
	if err == nil {
		return nil
	}
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de
	}
	return translate(err, action)
}
