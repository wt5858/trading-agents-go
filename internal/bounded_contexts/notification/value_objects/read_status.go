package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ReadStatus 是通知的已读状态。
//
// 它是一个**筛选与展示用的值对象**，不是落库的列：库里存的是 read_at（时刻），
// 已读与否由 read_at IS NULL 判定。之所以不落一个 read bool 列，是因为
// 「什么时候读的」这个事实一旦丢掉就补不回来，而它恰恰是「红点为什么消失了」
// 这类问题唯一能查的线索；两者并存则必然出现 read=1 而 read_at 为空的脏数据。
type ReadStatus string

const (
	// StatusUnread 未读，即 read_at IS NULL。
	StatusUnread ReadStatus = "unread"
	// StatusRead 已读，即 read_at IS NOT NULL。
	StatusRead ReadStatus = "read"
)

// NewReadStatus 解析已读状态筛选条件。
//
// 空串合法，语义是「不限」，这是通知列表默认的查询口径。
// 其余非法值一律报错：静默当成「不限」会让前端一次笔误表现成「筛选按钮没反应」——
// 接口照常返回 200，列表照常有数据，只是筛选悄悄失效了，这种哑故障最难查。
func NewReadStatus(s string) (ReadStatus, error) {
	switch ReadStatus(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case StatusUnread:
		return StatusUnread, nil
	case StatusRead:
		return StatusRead, nil
	}
	return "", custom_errors.Invalid("非法的通知已读状态: %s", s)
}

func (s ReadStatus) Valid() bool { return s == StatusUnread || s == StatusRead }

// IsZero 表示「不限已读状态」。
func (s ReadStatus) IsZero() bool { return s == "" }

func (s ReadStatus) String() string { return string(s) }

func (s ReadStatus) DisplayName() string {
	switch s {
	case StatusUnread:
		return "未读"
	case StatusRead:
		return "已读"
	}
	return "全部"
}
