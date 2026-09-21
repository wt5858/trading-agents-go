package value_objects

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SettingScope 划定配置中心「管得着」的范围。
//
// 这个枚举本身就是一道护栏：数据库里只可能出现这四个域的配置项，
// 想往库里塞 mysql.password 连域都填不出来——基础设施连接参数不归配置中心管，
// 理由见 domain_services 的包注释。
type SettingScope string

const (
	ScopeLLM     SettingScope = "llm"     // 模型调用相关：默认模型、超时、并发
	ScopeMarket  SettingScope = "market"  // 行情数据源：开关与 token
	ScopeSync    SettingScope = "sync"    // 同步任务调优：窗口、批量、间隔
	ScopeFeature SettingScope = "feature" // 功能开关
)

func NewSettingScope(s string) (SettingScope, error) {
	switch sc := SettingScope(strings.ToLower(strings.TrimSpace(s))); sc {
	case ScopeLLM, ScopeMarket, ScopeSync, ScopeFeature:
		return sc, nil
	default:
		return "", custom_errors.Invalid("未知的配置域: %s（可选 llm / market / sync / feature）", s)
	}
}

// RehydrateSettingScope 跳过校验，仅用于从数据库回读。
func RehydrateSettingScope(s string) SettingScope { return SettingScope(s) }

func (s SettingScope) Valid() bool {
	switch s {
	case ScopeLLM, ScopeMarket, ScopeSync, ScopeFeature:
		return true
	}
	return false
}

func (s SettingScope) String() string { return string(s) }

// CarriesSecrets 标记这个域里可能出现密钥类配置项（llm 的 api key、market 的 token）。
// 领域事件据此决定要不要携带新旧值——宁可整域保守，也不要靠「这个键应该不是密钥吧」的判断。
func (s SettingScope) CarriesSecrets() bool {
	return s == ScopeLLM || s == ScopeMarket
}

// SettingKey 是点分配置键，形如 llm.default_model。
//
// 第一段必须是合法的 SettingScope：键自带域信息，才不会出现
// 「键叫 llm.timeout 却被归到 market 域」这种查不出来的错位。
type SettingKey struct{ v string }

const (
	settingKeyMaxLen      = 128
	settingKeyMaxSegments = 4
)

func NewSettingKey(s string) (SettingKey, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return SettingKey{}, custom_errors.Invalid("配置键不能为空")
	}
	if len(s) > settingKeyMaxLen {
		return SettingKey{}, custom_errors.Invalid("配置键不能超过 %d 个字符", settingKeyMaxLen)
	}
	segments := strings.Split(s, ".")
	if len(segments) < 2 || len(segments) > settingKeyMaxSegments {
		return SettingKey{}, custom_errors.Invalid("配置键必须是 2-%d 段点分格式，如 llm.default_model: %s", settingKeyMaxSegments, s)
	}
	for _, seg := range segments {
		if seg == "" {
			return SettingKey{}, custom_errors.Invalid("配置键不能包含空段: %s", s)
		}
		if !unicode.IsLower(rune(seg[0])) {
			return SettingKey{}, custom_errors.Invalid("配置键的每一段都必须以小写字母开头: %s", s)
		}
		for _, r := range seg {
			if !unicode.IsLower(r) && !unicode.IsDigit(r) && r != '_' {
				return SettingKey{}, custom_errors.Invalid("配置键只能包含小写字母、数字、下划线和点: %s", s)
			}
		}
	}
	if _, err := NewSettingScope(segments[0]); err != nil {
		return SettingKey{}, custom_errors.Invalid("配置键的第一段必须是合法的配置域（llm / market / sync / feature）: %s", s)
	}
	return SettingKey{v: s}, nil
}

// RehydrateSettingKey 跳过校验，仅用于从数据库回读。
func RehydrateSettingKey(s string) SettingKey { return SettingKey{v: s} }

func (k SettingKey) String() string { return k.v }
func (k SettingKey) IsZero() bool   { return k.v == "" }

// Scope 取键的第一段。键自己就知道自己属于哪个域，不需要调用方再传一遍。
func (k SettingKey) Scope() SettingScope {
	if i := strings.Index(k.v, "."); i > 0 {
		return SettingScope(k.v[:i])
	}
	return ""
}

// secretKeySuffixes 是密钥类配置项的命名特征。
var secretKeySuffixes = []string{"_key", "_token", "_secret", "_password", "_credential"}

// LooksSecret 判断这一项是否装着敏感值。
//
// 用命名约定而不是白名单：配置项是运行期可以新增的，白名单必然漏，
// 而漏判的代价是把一个 token 明文写进事件、再写进日志或消息队列。
// 命名约定会误判（把 market.enable_token_cache 也当成密钥），
// 但误判的代价只是少展示一个值，两边不对称得很明显。
func (k SettingKey) LooksSecret() bool {
	for _, suffix := range secretKeySuffixes {
		if strings.HasSuffix(k.v, suffix) {
			return true
		}
	}
	return false
}

// SettingValue 是配置项的值。
//
// 统一按字符串存，读的时候再按需要的类型解析：配置项的类型是消费方知道的事，
// 存储层没必要也没能力替它记住。所有访问器都返回 (值, 是否解析成功)，
// 强迫调用方对「库里躺着一个 'abc' 而你想要个 int」这件事显式表态——
// 静默回落到零值会让一次配置笔误变成一次「并发度悄悄变成 0」的线上事故。
type SettingValue struct{ raw string }

func NewSettingValue(s string) SettingValue { return SettingValue{raw: strings.TrimSpace(s)} }

// RehydrateSettingValue 跳过归一化，仅用于从数据库回读。
func RehydrateSettingValue(s string) SettingValue { return SettingValue{raw: s} }

func (v SettingValue) String() string { return v.raw }
func (v SettingValue) IsZero() bool   { return v.raw == "" }

func (v SettingValue) Equal(other SettingValue) bool { return v.raw == other.raw }

// MarshalJSON 统一输出字符串形态，避免 "3" 和 3 两种形状在管理台之间来回抖动。
func (v SettingValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.raw) }

func (v SettingValue) AsString() (string, bool) {
	if v.raw == "" {
		return "", false
	}
	return v.raw, true
}

func (v SettingValue) AsInt() (int, bool) {
	n, err := strconv.Atoi(v.raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// AsBool 在 strconv.ParseBool 之外额外认 on/off/yes/no：
// 这些写法在人工维护的配置里极常见，认下来比让管理员去记「只能写 true」更实际。
func (v SettingValue) AsBool() (bool, bool) {
	switch strings.ToLower(v.raw) {
	case "on", "yes", "y":
		return true, true
	case "off", "no", "n":
		return false, true
	}
	b, err := strconv.ParseBool(v.raw)
	if err != nil {
		return false, false
	}
	return b, true
}

// AsDuration 只认带单位的写法（30s / 5m / 1h）。
//
// 刻意不把裸数字当成秒：同一个库里 "30" 到底是 30 秒还是 30 毫秒，
// 取决于哪个消费方先读到它，这种歧义迟早会变成一次超时配置事故。
func (v SettingValue) AsDuration() (time.Duration, bool) {
	d, err := time.ParseDuration(v.raw)
	if err != nil {
		return 0, false
	}
	return d, true
}
