package value_objects

import (
	"net/url"
	"strings"
	"unicode"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ProviderKind 是接入协议族，决定装配时构造哪一种客户端。
//
// 它不是厂商名：OpenAI / DeepSeek / 通义 / 智谱 / 硅基流动 / OpenRouter / Ollama
// 说的是同一套 /v1/chat/completions 协议，差别只在 baseURL 和密钥，
// 所以它们共用 openai_compat 这一种 kind。
type ProviderKind string

const (
	KindOpenAICompat ProviderKind = "openai_compat"
	KindAnthropic    ProviderKind = "anthropic"
	KindGoogle       ProviderKind = "google"
)

// NewProviderKind 拒绝未知取值，绝不静默回落到 openai_compat。
//
// 静默回落的代价：把 kind 写成 "anthropi" 的那条记录会被当成 OpenAI 兼容协议注册，
// 一切看起来都正常，直到第一次真实调用时返回一个「404 / invalid request」——
// 报错点离配错的地方隔了几十个文件，排查成本高得离谱。
// 在写入的那一刻报错，问题就钉死在管理台的那一次提交上。
func NewProviderKind(s string) (ProviderKind, error) {
	switch k := ProviderKind(strings.ToLower(strings.TrimSpace(s))); k {
	case KindOpenAICompat, KindAnthropic, KindGoogle:
		return k, nil
	case "":
		// 空同样不放行：省略 kind 与写错 kind 的后果一模一样，
		// 都是让一次错误的协议选择潜伏到运行期。
		return "", custom_errors.Invalid("必须指定供应商协议类型（openai_compat / anthropic / google）")
	default:
		return "", custom_errors.Invalid("未知的供应商协议类型: %s（可选 openai_compat / anthropic / google）", s)
	}
}

// RehydrateProviderKind 跳过校验，仅用于从数据库回读。
func RehydrateProviderKind(s string) ProviderKind { return ProviderKind(s) }

func (k ProviderKind) Valid() bool {
	switch k {
	case KindOpenAICompat, KindAnthropic, KindGoogle:
		return true
	}
	return false
}

func (k ProviderKind) String() string { return string(k) }

// ProviderName 是供应商标识，同时是路由表的键与数据库唯一索引的值。
//
// 做成值对象而不是裸 string：它会被拼进模型路由（"deepseek/deepseek-chat"），
// 一个带空格或斜杠的名字会让路由解析出完全错误的 provider。
type ProviderName struct{ v string }

const providerNameMaxLen = 32

func NewProviderName(s string) (ProviderName, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ProviderName{}, custom_errors.Invalid("供应商名称不能为空")
	}
	if len(s) > providerNameMaxLen {
		return ProviderName{}, custom_errors.Invalid("供应商名称不能超过 %d 个字符", providerNameMaxLen)
	}
	for _, r := range s {
		// 斜杠必须挡住：模型路由用 "provider/model" 表达显式指定，
		// 名字里再带斜杠会让这条约定失效。
		if !unicode.IsLower(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return ProviderName{}, custom_errors.Invalid("供应商名称只能包含小写字母、数字、下划线和短横线")
		}
	}
	return ProviderName{v: s}, nil
}

// RehydrateProviderName 跳过校验，仅用于从数据库回读。
func RehydrateProviderName(s string) ProviderName { return ProviderName{v: s} }

func (n ProviderName) String() string { return n.v }
func (n ProviderName) IsZero() bool   { return n.v == "" }

// EndpointURL 是供应商接入地址。
//
// 允许为空：空表示「用客户端内置的官方默认地址」，这是绝大多数厂商的正常用法，
// 只有自建网关和代理才需要显式填写。
type EndpointURL struct{ v string }

func NewEndpointURL(s string) (EndpointURL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return EndpointURL{}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return EndpointURL{}, custom_errors.Invalid("接入地址格式不合法: %s", s)
	}
	// 只放行 http/https：写成 "api.openai.com/v1"（漏掉 scheme）会被 url.Parse
	// 当成一个相对路径而不报错，等到发请求时才炸，那时已经离配置现场很远了。
	if u.Scheme != "http" && u.Scheme != "https" {
		return EndpointURL{}, custom_errors.Invalid("接入地址必须以 http:// 或 https:// 开头: %s", s)
	}
	if u.Host == "" {
		return EndpointURL{}, custom_errors.Invalid("接入地址缺少主机名: %s", s)
	}
	return EndpointURL{v: strings.TrimRight(s, "/")}, nil
}

// RehydrateEndpointURL 跳过校验，仅用于从数据库回读。
func RehydrateEndpointURL(s string) EndpointURL { return EndpointURL{v: s} }

func (e EndpointURL) String() string { return e.v }
func (e EndpointURL) IsZero() bool   { return e.v == "" }
