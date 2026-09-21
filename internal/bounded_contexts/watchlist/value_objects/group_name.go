// Package value_objects 提供自选股上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不出现 gorm 标签：持久化格式的演进属于 repositories/，由 DTO 承载。
// 本包也不引用 entities：值对象是实体的构件，反向依赖会让两者绕成一个环。
package value_objects

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// MaxGroupNameRunes 按「字符数」而不是字节数计。
	// 用 len() 的话，一个 10 个汉字的分组名是 30 字节，会被莫名其妙地拒掉，
	// 而这正是中文用户最常用的取名长度。
	MaxGroupNameRunes = 16
)

// GroupName 是分组名值对象。
//
// 提升成 VO 而不是用裸 string 的理由有三条，都不是洁癖：
//   - 去空白规范化只发生在构造点。"科技股" 与 "科技股 " 若不归一，
//     (user_id, name) 唯一索引就挡不住用户手滑打出的重复分组。
//   - 长度按 rune 计（见上）。
//   - 控制字符与换行会把前端的分组 tab 撑成两行，在构造点一次性挡掉。
type GroupName struct{ v string }

// NewGroupName 校验并规范化分组名。
func NewGroupName(s string) (GroupName, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return GroupName{}, custom_errors.Invalid("分组名不能为空")
	}
	if n := utf8.RuneCountInString(s); n > MaxGroupNameRunes {
		return GroupName{}, custom_errors.Invalid("分组名不能超过 %d 个字符，当前 %d 个", MaxGroupNameRunes, n)
	}
	for _, r := range s {
		// unicode.IsControl 覆盖 \n \r \t 与各类控制符。
		if unicode.IsControl(r) {
			return GroupName{}, custom_errors.Invalid("分组名不能包含控制字符或换行")
		}
	}
	return GroupName{v: s}, nil
}

// RehydrateGroupName 跳过校验，仅供从数据库重建使用。
//
// 理由与 identity.RehydrateUsername 完全相同：已经落库的行是既成事实，
// 读路径再跑一遍今天的校验规则，会让一条历史数据（比如规则收紧前存下的长名字）
// 把整个自选股列表接口打挂。校验属于写入路径。
func RehydrateGroupName(s string) GroupName { return GroupName{v: s} }

func (n GroupName) String() string { return n.v }

func (n GroupName) IsZero() bool { return n.v == "" }

// Equal 用于「改名成了原来的名字」这类判断，避免调用方到处写 a.String() == b.String()。
func (n GroupName) Equal(o GroupName) bool { return n.v == o.v }
