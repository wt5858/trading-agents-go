// Package repositories 承载自选股上下文的持久化。
//
// 本层是本上下文唯一允许出现事务的地方，也是唯一知道 GORM 存在的地方。
// 对上的唯一契约是：只抛 custom_errors.Error，绝不把 gorm / driver 的错误类型漏出去——
// 一旦 gorm.ErrRecordNotFound 漏到接口层，「分组不存在」就会被映射成 500。
//
// # 本上下文只有一个仓储
//
// WatchlistGroupRepository 管聚合根 WatchlistGroup。子实体 WatchlistItem
// **没有**仓储，它的增删改全部藏在 Save 内部，对调用方不可见——调用方手上
// 永远只有整个聚合根。这正是「只有聚合根配仓储」这条规则的另一半：
// 不只是「不给子实体建仓储文件」，而是连子实体级别的持久化 API 都不对外存在。
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

// 两个唯一索引承载着两条不变式，它们的冲突要翻译成完全不同的提示，
// 所以必须能从错误里分辨是哪一个。索引名在这里出现，是本层与迁移脚本之间
// 刻意保留的一处耦合——它换名字的时候，这里的提示语必须跟着换。
const (
	idxGroupUserName = "uk_watchlist_groups_user_name"
	idxItemGroupCode = "uk_watchlist_items_group_code"
)

// translate 是技术错误到领域错误的边界。上层永远看不到 gorm 或 driver 的类型。
// subject 是「主语」，用于拼出可读消息，例如 "自选分组(id=3) 不存在"。
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
// 最后才退回字符串匹配。理由与 scheduling 上下文相同：TranslateError 只作用于
// gorm 自己发起的调用，手写 Exec / 原生 SQL 路径拿到的仍是裸驱动错误。
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

// violatedIndex 从唯一键冲突里抠出索引名。
//
// MySQL 的报文形如：Duplicate entry '7-科技股' for key 'watchlist_groups.uk_...'。
// 索引名是报文里唯一能区分「组名重了」和「组内这只票已经有了」的信息，
// 而这两者对用户是两句完全不同的话。抠不出来时返回空串，调用方回落到通用提示——
// 宁可给一句略笼统的提示，也不要因为 MySQL 换了报文格式就 panic。
func violatedIndex(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, idxGroupUserName):
		return idxGroupUserName
	case strings.Contains(msg, idxItemGroupCode):
		return idxItemGroupCode
	default:
		return ""
	}
}
