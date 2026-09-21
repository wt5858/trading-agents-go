// Package repositories 承载股票上下文的持久化实现。
//
// 本包是本上下文唯一允许出现事务的地方，也是唯一知道 GORM 与 mongo-driver 存在的地方。
// 上层拿到的永远是实体或值对象，永远不是 dtos 里那些带标签的结构。
package repositories

import (
	"errors"
	"fmt"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"go.mongodb.org/mongo-driver/mongo"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// mysqlDuplicateEntry 是 MySQL 唯一键冲突的错误号（ER_DUP_ENTRY）。
const mysqlDuplicateEntry = 1062

// translateSQL 是 GORM/驱动错误到领域错误的边界。上层永远看不到 gorm 或 driver 的类型。
// 名字带 SQL 后缀是为了和 translateMongo 区分——两种存储的错误语义不同，
// 共用一个函数只会让判定分支越堆越乱。
func translateSQL(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	subject := fmt.Sprintf(format, args...)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return custom_errors.NotFound("%s 不存在", subject).Wrap(err)
	case isDuplicateKey(err):
		// 冲突原因（哪个唯一键）对调用方没有意义，但对排查有意义，所以 Wrap 保留原始错误。
		return custom_errors.AlreadyExists("%s 已存在", subject).Wrap(err)
	default:
		return custom_errors.Internal("%s 数据库操作失败", subject).Wrap(err)
	}
}

// translateMongo 把 mongo 驱动错误翻译成领域错误，语义与 translateSQL 保持一致。
// mongo.ErrNoDocuments 绝不允许漏给应用层——漏出去 HTTP 层会把「查不到」映射成 500。
func translateMongo(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	subject := fmt.Sprintf(format, args...)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return custom_errors.NotFound("%s 不存在", subject).Wrap(err)
	case mongo.IsDuplicateKeyError(err):
		return custom_errors.AlreadyExists("%s 已存在", subject).Wrap(err)
	default:
		return custom_errors.Internal("%s 数据库操作失败", subject).Wrap(err)
	}
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

// escapeLike 转义 LIKE 通配符。用户搜索框里输入的 % 必须当字面量处理，
// 否则一个 "%" 就是一次全表扫描。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
