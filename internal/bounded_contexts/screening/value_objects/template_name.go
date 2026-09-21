package value_objects

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// MaxTemplateNameRunes 按字符数而不是字节数计：一个 10 个汉字的模板名
	// 是 30 字节，用 len() 判会把中文用户最常用的取名长度莫名其妙地拒掉。
	MaxTemplateNameRunes = 32
	// MaxTemplateDescRunes 是模板说明的长度上限。
	MaxTemplateDescRunes = 200
)

// TemplateName 是选股模板名值对象。
//
// 提升成 VO 而不是用裸 string 的理由与 watchlist.GroupName 一致，其中
// **去空白规范化**这一条在这里尤其要紧：「同一用户下模板名唯一」由
// (user_id, name) 唯一索引保证，而 "低估值蓝筹" 与 "低估值蓝筹 " 在数据库看来
// 是两个不同的串。不在构造点归一，唯一索引就挡不住用户手滑造出的重复模板。
type TemplateName struct{ v string }

func NewTemplateName(s string) (TemplateName, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TemplateName{}, custom_errors.Invalid("模板名不能为空")
	}
	if n := utf8.RuneCountInString(s); n > MaxTemplateNameRunes {
		return TemplateName{}, custom_errors.Invalid(
			"模板名不能超过 %d 个字符，当前 %d 个", MaxTemplateNameRunes, n)
	}
	for _, r := range s {
		// 控制字符与换行会把前端的模板列表撑成两行，在构造点一次性挡掉。
		if unicode.IsControl(r) {
			return TemplateName{}, custom_errors.Invalid("模板名不能包含控制字符或换行")
		}
	}
	return TemplateName{v: s}, nil
}

// RehydrateTemplateName 跳过校验，仅供从数据库重建使用。
//
// 这里可以放心跳过（而 RehydrateFieldName 不行）：模板名只会被显示出来，
// 不会进入任何查询语句。重建时再跑一遍今天的规则，只会让一条规则收紧之前
// 存下的长名字把整个模板列表接口打挂。
func RehydrateTemplateName(s string) TemplateName { return TemplateName{v: s} }

func (n TemplateName) String() string            { return n.v }
func (n TemplateName) IsZero() bool              { return n.v == "" }
func (n TemplateName) Equal(o TemplateName) bool { return n.v == o.v }

// NormalizeDescription 归一化模板说明。
//
// 它不是一个类型而只是一个函数：说明文本没有任何跨字段不变式，也没有相等语义，
// 为它造一个 VO 只会让实体字段多一层 .String()。截断而不是报错，
// 是因为说明是可有可无的补充信息，为它失败掉整次保存不划算。
func NormalizeDescription(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= MaxTemplateDescRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:MaxTemplateDescRunes])
}
