package value_objects

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ToolName 是工具标识值对象。
//
// 提升成类型而不是用裸 string，是因为工具名同时出现在三个地方：契约里的授权白名单、
// 发给模型的函数声明、模型回传的 tool_call。三处任意一处写错，表现都是
// 「模型调了个不存在的工具然后开始编数据」，而裸 string 让编译器无从帮忙。
type ToolName string

const (
	ToolGetQuote           ToolName = "get_quote"
	ToolGetKlines          ToolName = "get_klines"
	ToolGetTechnicalIndics ToolName = "get_technical_indicators"
	ToolGetFinancials      ToolName = "get_financials"
	ToolGetNews            ToolName = "get_news"
	ToolGetSocialSentiment ToolName = "get_social_sentiment"
)

// AllToolNames 返回全部内置工具名，顺序固定，便于展示与测试。
func AllToolNames() []ToolName {
	return []ToolName{
		ToolGetQuote, ToolGetKlines, ToolGetTechnicalIndics,
		ToolGetFinancials, ToolGetNews, ToolGetSocialSentiment,
	}
}

// NewToolName 解析工具名。未知工具报错：模型幻觉出的工具名必须在边界被挡掉，
// 而不是一路传到注册表再返回一个语焉不详的 nil。
func NewToolName(s string) (ToolName, error) {
	n := ToolName(strings.TrimSpace(s))
	if n.Valid() {
		return n, nil
	}
	return "", custom_errors.Invalid("未知的工具: %s", s)
}

func (n ToolName) Valid() bool {
	switch n {
	case ToolGetQuote, ToolGetKlines, ToolGetTechnicalIndics,
		ToolGetFinancials, ToolGetNews, ToolGetSocialSentiment:
		return true
	}
	return false
}

func (n ToolName) String() string { return string(n) }

// DataAccess 是一位成员的工具授权白名单。
//
// 为什么要有它：市场分析师拿到财报工具、新闻分析师拿到 K 线工具，除了浪费 token
// 之外还会让两份报告的论据互相污染——多空辩论的前提是各方视角确实不同。
// 更实际的理由是安全：提示词注入唯一能操纵的是模型的意图，而授权在 Runtime 里
// 按白名单强制执行，模型再怎么被诱导也调不出白名单外的工具。
//
// 内部切片不导出：DataAccess 一旦构造就不可变，否则调用方一次 append
// 就能从外部给自己扩权。
type DataAccess struct {
	allowed []ToolName
}

// NewDataAccess 构造授权集合，去重并保序；非法工具名直接丢弃。
func NewDataAccess(names ...ToolName) DataAccess {
	seen := make(map[ToolName]struct{}, len(names))
	out := make([]ToolName, 0, len(names))
	for _, n := range names {
		if !n.Valid() {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return DataAccess{allowed: out}
}

// NoDataAccess 是空授权，给那些只做推理不取数的成员（研究经理、风控经理）。
func NoDataAccess() DataAccess { return DataAccess{} }

// Allows 判定是否授权了某个工具。
func (a DataAccess) Allows(n ToolName) bool {
	for _, x := range a.allowed {
		if x == n {
			return true
		}
	}
	return false
}

// Names 返回授权列表的副本。
func (a DataAccess) Names() []ToolName { return append([]ToolName(nil), a.allowed...) }

func (a DataAccess) IsEmpty() bool { return len(a.allowed) == 0 }

func (a DataAccess) Len() int { return len(a.allowed) }

// ParamField 是工具参数的单个字段声明，对齐 JSON Schema 的子集。
type ParamField struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     any      `json:"default,omitempty"`
}

// ParamSchema 是工具的入参 JSON Schema。
//
// 只实现 JSON Schema 的一个子集：object + 一层扁平属性。
// 各家 function-calling 的校验严格程度不一，嵌套结构在不同厂商间的兼容性很差，
// 而工具入参本来也只需要几个标量。
type ParamSchema struct {
	Type       string                `json:"type"`
	Properties map[string]ParamField `json:"properties"`
	Required   []string              `json:"required,omitempty"`
}

// NewParamSchema 构造对象型 schema。
func NewParamSchema(props map[string]ParamField, required ...string) ParamSchema {
	if props == nil {
		props = map[string]ParamField{}
	}
	return ParamSchema{Type: "object", Properties: props, Required: required}
}

// ToolSpec 是发给模型的工具声明（名称 + 说明 + 入参 schema）。
//
// 它刻意只有数据没有行为：能「执行」的 Tool 是 domain_services 的端口，
// 而这份声明要被序列化进 HTTP 请求体，必须是纯值对象。
// 两者分开还有一个实际收益——Runtime 可以只把白名单内的 Spec 发给模型，
// 而执行权限的校验独立进行，不依赖「没发给它就不会调」这种脆弱假设。
type ToolSpec struct {
	Name        ToolName    `json:"name"`
	Description string      `json:"description"`
	Schema      ParamSchema `json:"schema"`
}

// NewToolSpec 构造工具声明。
func NewToolSpec(name ToolName, description string, schema ParamSchema) ToolSpec {
	return ToolSpec{Name: name, Description: description, Schema: schema}
}

func (s ToolSpec) IsZero() bool { return s.Name == "" }

// ToolCall 是模型发起的一次工具调用。
//
// Arguments 用 json.RawMessage 而不是 map：模型给的参数经常带着多余字段或错误类型，
// 在这里就解析等于把解析失败的责任揽到编排层；保持原样透传，
// 由每个工具用自己的结构体去解析，解析失败也只影响这一次调用。
type ToolCall struct {
	ID        ToolCallID      `json:"id"`
	Name      ToolName        `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCallID 是一次工具调用的回执 ID。
// 它必须被原样带回给模型（OpenAI 的 tool_call_id / Anthropic 的 tool_use_id），
// 否则多个并行工具结果无法对号入座。
type ToolCallID string

func (id ToolCallID) String() string { return string(id) }

func (c ToolCall) IsZero() bool { return c.Name == "" }

// Args 返回参数原文，空参数归一成 "{}"，省得每个工具都判一次空。
func (c ToolCall) Args() json.RawMessage {
	if len(c.Arguments) == 0 {
		return json.RawMessage("{}")
	}
	return c.Arguments
}

// SpecsOf 按授权过滤出可用的工具声明，顺序按名称排序保证提示词可复现。
//
// 顺序固定这件事不是洁癖：工具声明是提示词的一部分，顺序抖动会让
// provider 侧的前缀缓存全部失效，同样的分析成本可能翻倍。
func SpecsOf(all []ToolSpec, access DataAccess) []ToolSpec {
	if access.IsEmpty() {
		return nil
	}
	out := make([]ToolSpec, 0, access.Len())
	for _, s := range all {
		if access.Allows(s.Name) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
