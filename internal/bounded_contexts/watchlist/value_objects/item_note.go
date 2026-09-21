package value_objects

import (
	"strings"
	"unicode/utf8"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MaxNoteRunes 是自选股备注的长度上限，同样按字符数计。
//
// 定在 100：备注是「为什么盯这只票」的一句话，不是研报。上限存在的意义
// 是让 note 列可以安全地用 varchar 而不是 text，从而与 group_id 一起
// 进同一个索引页，列表查询不需要回表读大字段。
const MaxNoteRunes = 100

// ItemNote 是自选项备注值对象。空备注是合法的，因此零值可用。
type ItemNote struct{ v string }

// NewItemNote 校验并规范化备注。空串返回零值而不是错误——
// 「没写备注」是绝大多数自选项的常态，把它当成非法输入会让添加自选必须填一个字。
func NewItemNote(s string) (ItemNote, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ItemNote{}, nil
	}
	if n := utf8.RuneCountInString(s); n > MaxNoteRunes {
		return ItemNote{}, custom_errors.Invalid("备注不能超过 %d 个字符，当前 %d 个", MaxNoteRunes, n)
	}
	return ItemNote{v: s}, nil
}

// RehydrateItemNote 跳过校验，仅供从数据库重建使用。理由同 RehydrateGroupName。
func RehydrateItemNote(s string) ItemNote { return ItemNote{v: s} }

func (n ItemNote) String() string { return n.v }

func (n ItemNote) IsZero() bool { return n.v == "" }

func (n ItemNote) Equal(o ItemNote) bool { return n.v == o.v }
