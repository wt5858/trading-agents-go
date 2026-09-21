// Package value_objects 是配置中心的值对象层：构造即校验，构造后不可变。
//
// 本包最重要的类型是 SecretValue。配置中心天然要保管第三方密钥，
// 而密钥泄露的绝大多数事故不是被人拖库，而是被自己人打进了日志、
// 塞进了错误信息、或者顺手 json.Marshal 进了某个响应体。
// 所以这里不把密钥当成 string 存，而是用一个「默认形态就是掩码」的类型：
// 想拿到明文必须显式调用 Expose()，代码评审时一眼能看见。
package value_objects

import (
	"encoding/json"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// maskThreshold 是「允许露出首尾」的最短长度。
	// 低于它就整体打码：一把 8 位的密钥再露出 3+4 位，等于只剩一位没说。
	maskThreshold = 12
	maskPrefixLen = 3
	maskSuffixLen = 4

	// maskFull 用于短密钥；maskEllipsis 用于长密钥的中段。
	maskFull     = "****"
	maskEllipsis = "…"
)

// SecretValue 是一把密钥（LLM API Key、行情源 token 等）。
//
// 内部字段不导出，是为了让「打印它」这件事无法绕过 String()：
// 只要字段导出，fmt 的 %+v 就会直接把明文铺开，reflect 也拦不住。
type SecretValue struct{ v string }

// NewSecret 从调用方输入构造密钥。
//
// 它会拒绝「看起来像掩码」的输入。这不是洁癖：管理台先 GET 一次配置拿到
// sk-…abcd，再把整个对象 PUT 回来保存，是最常见的一种交互；
// 如果这里照单全收，真密钥就被字符串 "sk-…abcd" 覆盖了，
// 而故障现场是几小时后某次模型调用返回 401，没人会联想到那次保存。
func NewSecret(s string) (SecretValue, error) {
	s = strings.TrimSpace(s)
	if isMaskLike(s) {
		return SecretValue{}, custom_errors.Invalid("密钥看起来是掩码而非明文，请填写真实密钥")
	}
	return SecretValue{v: s}, nil
}

// RehydrateSecret 跳过校验，用于从数据库回读。
// 库里的行是既成事实，读路径再跑一次写入期校验，只会让一条脏数据把整个列表接口打挂。
func RehydrateSecret(s string) SecretValue { return SecretValue{v: s} }

// isMaskLike 判断一个字符串是否是本包产出的掩码。
func isMaskLike(s string) bool {
	return s == maskFull || strings.Contains(s, maskEllipsis)
}

// Expose 交出明文。
//
// 名字刻意取得刺眼：全仓库允许调用它的地方只有两处——
//  1. repositories/dtos，把密钥写进数据库；
//  2. domain_services 里那个唯一的 expose()，把密钥交给 HTTP 客户端。
//
// 出现第三处调用就应当在评审时被拦下。
func (s SecretValue) Expose() string { return s.v }

// Masked 返回可以安全展示、安全落日志的形态。
//
// 保留首尾是有实际用途的：管理员要能分辨「我配的是那一把」，
// 露出 3 位前缀（sk- / AIza 这类厂商标识）和 4 位后缀刚好够做这个区分，
// 又不足以让拿到日志的人推回原值。
func (s SecretValue) Masked() string {
	switch {
	case s.v == "":
		// 空值没有任何可泄露的内容，直接返回空串，
		// 让管理台能如实显示「未配置」而不是显示一串假的星号。
		return ""
	case len(s.v) < maskThreshold:
		return maskFull
	default:
		return s.v[:maskPrefixLen] + maskEllipsis + s.v[len(s.v)-maskSuffixLen:]
	}
}

// String 让 fmt 的 %v / %s / %q 都只能拿到掩码。
func (s SecretValue) String() string { return s.Masked() }

// GoString 兜住 %#v。fmt 对 %#v 走 GoStringer，不实现它就会打出内部字段的明文。
func (s SecretValue) GoString() string { return `value_objects.SecretValue(` + s.Masked() + `)` }

// MarshalJSON 保证密钥无论被塞进哪个响应结构体，序列化出去的都是掩码。
// 这是「响应里绝不出现明文密钥」这条要求的最后一道、也是唯一一道无需人工遵守的防线。
func (s SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(s.Masked()) }

// MarshalText 覆盖走 encoding.TextMarshaler 的编码器（yaml、部分日志库）。
func (s SecretValue) MarshalText() ([]byte, error) { return []byte(s.Masked()), nil }

func (s SecretValue) IsZero() bool { return s.v == "" }

// Equal 用于判断密钥是否真的变了，避免把一次「没改动的保存」记成一次轮换。
func (s SecretValue) Equal(other SecretValue) bool { return s.v == other.v }
