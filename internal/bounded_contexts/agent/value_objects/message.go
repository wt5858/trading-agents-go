package value_objects

import (
	"strings"

	"github.com/shopspring/decimal"
)

// MessageRole 是对话角色值对象。
//
// 四个取值是三家厂商协议的最小公约数：OpenAI 有全部四个；Anthropic 把 system
// 抽成顶层字段、把 tool 结果塞进 user 的 content block；Gemini 叫 user/model。
// 领域层只认这一套，各家的怪癖由 helpers/llm 就地翻译。
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

func (r MessageRole) Valid() bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

func (r MessageRole) String() string { return string(r) }

// Message 是一条对话消息。
//
// 它是工具调用循环的状态载体：循环每转一圈就往消息列表里追加
// 「assistant(带 tool_calls) + 若干条 tool 结果」，下一轮把整个列表重新发给模型。
// 因此 Message 必须同时容纳文本、工具调用与工具结果三种形态。
type Message struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content,omitempty"`
	// Name 是工具结果所属的工具名。Gemini 的 functionResponse 用它对号入座
	// （它的 functionCall 不带 ID）。
	Name string `json:"name,omitempty"`
	// ToolCalls 只在 assistant 消息上有值。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 tool 消息上有值，对应它回应的那次调用。
	ToolCallID ToolCallID `json:"tool_call_id,omitempty"`
}

func SystemMessage(content string) Message {
	return Message{Role: RoleSystem, Content: content}
}

func UserMessage(content string) Message {
	return Message{Role: RoleUser, Content: content}
}

// AssistantToolCallMessage 构造「模型决定调用工具」这一轮的助手消息。
func AssistantToolCallMessage(content string, calls []ToolCall) Message {
	return Message{Role: RoleAssistant, Content: content, ToolCalls: append([]ToolCall(nil), calls...)}
}

// ToolResultMessage 构造一条工具结果消息。
func ToolResultMessage(call ToolCall, result string) Message {
	return Message{
		Role:       RoleTool,
		Name:       call.Name.String(),
		ToolCallID: call.ID,
		Content:    result,
	}
}

func (m Message) IsZero() bool { return m.Role == "" && m.Content == "" }

// Usage 是一次或多次模型调用的消耗汇总。
//
// CostUSD 由 helpers/llm 的报价表在调用现场算好后随 Usage 一起返回，
// 领域层只做加法、不做单价换算：单价是会过期的外部事实，
// 让它渗进领域层意味着改一次报价要动领域代码。
type Usage struct {
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	TotalTokens      int             `json:"total_tokens"`
	Calls            int             `json:"calls"`
	CostUSD          decimal.Decimal `json:"cost_usd"`
}

// Plus 返回两份消耗相加后的新值。
// 不原地累加：同一份 Usage 会被并行的六位分析师同时读取，
// 原地修改等于在没有锁的地方制造数据竞争。
func (u Usage) Plus(o Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
		TotalTokens:      u.TotalTokens + o.TotalTokens,
		Calls:            u.Calls + o.Calls,
		CostUSD:          u.CostUSD.Add(o.CostUSD),
	}
}

func (u Usage) IsZero() bool { return u.Calls == 0 && u.TotalTokens == 0 }

// CompletionRequest 是「一次」模型调用的入参，直接对应 LLMClient.Complete。
//
// Tools 是 ToolSpec（纯数据）而不是可执行的 Tool 端口：
// 客户端的职责到「把声明序列化进请求体」为止，执行工具是 Runtime 的事。
type CompletionRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolSpec
	// Temperature 保持 float64：它是发给 Anthropic/OpenAI/Google 的请求体字段，
	// 对方的 schema 要求 JSON 数字，而 decimal 的 MarshalJSON 默认输出带引号的
	// 字符串，送过去会被拒。它也不是金额或参与账目的计算量，
	// 留在 float64 不违反本仓库「金额与计算一律 decimal」的约定。
	Temperature float64
	MaxTokens   int
}

// CompletionResult 是一次模型调用的产出。
//
// Content 与 ToolCalls 可以同时有值：模型经常先说一句「我查一下财报」再发起调用。
type CompletionResult struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string
}

// HasToolCalls 判定本轮是否需要继续工具循环。
func (r CompletionResult) HasToolCalls() bool { return len(r.ToolCalls) > 0 }

// ChatRequest 是「一整轮对话」的入参，对应 Runtime 的一次 Execute。
//
// 它和 CompletionRequest 的区别就是工具调用循环：CompletionRequest 是单次往返，
// ChatRequest 描述的是「一直聊到模型不再要工具为止，最多 MaxToolRounds 轮」。
type ChatRequest struct {
	Model         string
	Messages      []Message
	Tools         []ToolSpec
	Temperature   float64
	MaxTokens     int
	MaxToolRounds int
	// Access 随请求一起下发，Runtime 在执行工具前再校验一次。
	// 这是刻意的重复：只靠「没把 spec 发给模型」来限制工具，
	// 在模型幻觉出一个没见过的工具名时就失效了。
	Access DataAccess
}

// ChatResponse 是一整轮对话的产出。
type ChatResponse struct {
	// Content 是模型最后一条不含工具调用的回复，也就是这位成员的报告正文。
	Content string
	// Messages 是完整的对话记录（含工具往返），便于排查「模型到底看到了什么」。
	Messages []Message
	Usage    Usage
	// ToolRounds 是实际发生的工具轮数，0 表示模型一次就给出了结论。
	ToolRounds int
	// Truncated 表示循环是因为撞到 MaxToolRounds 上限才结束的。
	// 这种回答通常不完整，调用方据此决定要不要在报告里加一句提示。
	Truncated bool
	// Model 是路由解析之后真正计费的模型名。
	// 它必须由这一层回填：请求里的 Model 可以是空串或别名，
	// 而执行轨迹要记的是实际落到哪个模型上——换了模型却记着旧名字，
	// 事后按模型归因的成本统计就是错的。
	Model string
}

func (r ChatResponse) IsZero() bool { return strings.TrimSpace(r.Content) == "" && r.Usage.IsZero() }
