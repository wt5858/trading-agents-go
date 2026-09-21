// Package llm 是 agent 上下文 LLMClient / ModelRouter 端口的基础设施实现。
//
// 这里只关心「怎么把领域层的 Message / ToolSpec 翻译成各家厂商的线上协议」，
// 工具调用循环、提示词、成本预算等业务决策全部留在领域层
// （internal/bounded_contexts/agent/domain_services）。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	agentsvc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	agentvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/httpx"
)

var (
	_ agentsvc.LLMClient   = (*OpenAICompatClient)(nil)
	_ agentsvc.LLMClient   = (*AnthropicClient)(nil)
	_ agentsvc.LLMClient   = (*GoogleClient)(nil)
	_ agentsvc.ModelRouter = (*Router)(nil)
)

// OpenAICompatClient 覆盖所有说 OpenAI /v1/chat/completions 协议的厂商：
// OpenAI、DeepSeek、DashScope(兼容模式)、Zhipu、SiliconFlow、OpenRouter、Ollama。
// 它们之间只差 baseURL 与 apiKey，没必要为每家写一个实现。
type OpenAICompatClient struct {
	provider string
	endpoint string
	apiKey   string
	hc       *http.Client
}

// NewOpenAICompat 构造一个兼容客户端。baseURL 可以给到 ".../v1" 这一层，
// 也可以直接给完整的 chat/completions 地址，这里统一归一化，避免调用方配错。
func NewOpenAICompat(provider, baseURL, apiKey string, httpClient *http.Client) *OpenAICompatClient {
	return &OpenAICompatClient{
		provider: provider,
		endpoint: chatCompletionsURL(baseURL),
		apiKey:   apiKey,
		hc:       ensureClient(httpClient, provider),
	}
}

func (c *OpenAICompatClient) Provider() string { return c.provider }

func (c *OpenAICompatClient) Complete(ctx context.Context, req agentvo.CompletionRequest) (*agentvo.CompletionResult, error) {
	if req.Model == "" {
		return nil, custom_errors.Invalid("%s: 未指定模型", c.provider)
	}

	body := oaRequest{
		Model:       req.Model,
		Messages:    toOAMessages(req.Messages),
		Tools:       toOATools(req.Tools),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	// 只有挂了工具才允许模型自己决定是否调用，否则某些厂商会对 tool_choice 报错。
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}

	headers := map[string]string{"Content-Type": "application/json"}
	// Ollama 之类的本地服务不需要鉴权，留空即可跳过。
	if c.apiKey != "" {
		headers["Authorization"] = "Bearer " + c.apiKey
	}

	// 模型名是配置项不是密钥，拿它当出站日志的目标标识；
	// 三家的 path 都是固定的，不给标签就看不出这次调用打的是哪个模型。
	ctx = withOutboundTarget(ctx, req.Model)

	var resp oaResponse
	if err := doJSON(ctx, c.hc, c.provider, c.endpoint, headers, &body, &resp); err != nil {
		return nil, err
	}
	// 部分厂商在 HTTP 200 里塞业务错误，不检查会得到一个内容为空的假成功。
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, custom_errors.Unavailable("%s 返回错误: %s", c.provider, resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return nil, custom_errors.Unavailable("%s 未返回任何候选结果", c.provider)
	}

	choice := resp.Choices[0]
	usage := agentvo.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		Calls:            1,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	usage.CostUSD = CostOf(req.Model, usage)

	return &agentvo.CompletionResult{
		Content:      choice.Message.Content,
		ToolCalls:    fromOAToolCalls(choice.Message.ToolCalls),
		Usage:        usage,
		FinishReason: choice.FinishReason,
	}, nil
}

// ---- 线上协议结构体 ----

type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	ToolChoice  string      `json:"tool_choice,omitempty"`
	Temperature float64     `json:"temperature,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	Name       string       `json:"name,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

// oaFunction 的 Arguments 在 OpenAI 协议里是「被字符串包了一层的 JSON」，
// 领域层用的是 json.RawMessage，两边要来回转。
type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string       `json:"type"`
	Function oaToolSchema `json:"function"`
}

type oaToolSchema struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Parameters  agentvo.ParamSchema `json:"parameters"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ---- 领域模型 <-> 线上协议 ----

func toOAMessages(msgs []agentvo.Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := oaMessage{Role: m.Role.String(), Content: m.Content, Name: m.Name}
		switch m.Role {
		case agentvo.RoleTool:
			om.ToolCallID = m.ToolCallID.String()
		case agentvo.RoleAssistant:
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, oaToolCall{
					ID:       tc.ID.String(),
					Type:     "function",
					Function: oaFunction{Name: tc.Name.String(), Arguments: rawToString(tc.Arguments)},
				})
			}
		}
		out = append(out, om)
	}
	return out
}

// toOATools 把领域侧的工具声明翻译成 OpenAI 的 function 声明。
// 入参是 ToolSpec（纯数据）而不是可执行的 Tool：客户端只需要把声明序列化出去，
// 执行工具是领域层运行时的事，给它执行能力只会让这层多一个不该有的权限。
func toOATools(tools []agentvo.ToolSpec) []oaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]oaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, oaTool{
			Type: "function",
			Function: oaToolSchema{
				Name:        t.Name.String(),
				Description: t.Description,
				Parameters:  normalizeSchema(t.Schema),
			},
		})
	}
	return out
}

func fromOAToolCalls(calls []oaToolCall) []agentvo.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]agentvo.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, agentvo.ToolCall{
			ID:        agentvo.ToolCallID(c.ID),
			Name:      agentvo.ToolName(c.Function.Name),
			Arguments: stringToRaw(c.Function.Arguments),
		})
	}
	return out
}

// ---- 以下为三家 Provider 共用的 HTTP 与 Schema 工具 ----

const (
	// defaultTimeout 兜底单次请求超时。推理模型一轮可能跑很久，给足余量。
	defaultTimeout = 120 * time.Second
	// maxRetries 是 429/5xx 之外再加的重试次数，总计最多 1+3 次尝试。
	maxRetries = 3
	// bodyLimit 控制错误信息里回显的响应体长度，避免把整页 HTML 灌进日志。
	bodyLimit = 512
)

// ensureClient 保证注入的 http.Client 一定带超时，否则连接挂死会拖垮整条分析流水线；
// 同时挂上出站日志 Transport（见 outbound_log.go）。
//
// 无论如何都复制一份，而不是像以前那样带超时就原样返回：三家 provider 在装配处
// 拿的是**同一个**注入的 *http.Client，就地设 Transport 会让它们互相盖掉
// provider 名，还会顺手污染调用方。复制只拷指针字段，底层连接池原样保留。
func ensureClient(hc *http.Client, provider string) *http.Client {
	c := &http.Client{Timeout: defaultTimeout}
	if hc != nil {
		copied := *hc
		c = &copied
		if c.Timeout == 0 {
			c.Timeout = defaultTimeout
		}
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &loggingTransport{base: base, provider: provider}
	return c
}

// doJSON 发一次 JSON 请求并解码 JSON 响应，内建指数退避重试。
func doJSON(ctx context.Context, hc *http.Client, provider, url string, headers map[string]string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return custom_errors.Internal("%s 请求序列化失败", provider).Wrap(err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := waitBackoff(ctx, attempt); err != nil {
				return custom_errors.Unavailable("%s 重试等待被中断", provider).Wrap(err)
			}
		}

		status, body, err := doOnce(ctx, hc, url, headers, payload)
		if err != nil {
			// ctx 被取消是调用方主动放弃，继续重试没有意义。
			if ctx.Err() != nil {
				return custom_errors.Unavailable("%s 请求已取消", provider).Wrap(ctx.Err())
			}
			// Do 失败返回的 *url.Error 带着完整 URL（含 query），
			// 而这个错误会被上层原样打进日志。
			lastErr = custom_errors.Unavailable("%s 请求失败", provider).Wrap(httpx.RedactError(err))
			continue
		}

		if status >= 200 && status < 300 {
			if err := json.Unmarshal(body, out); err != nil {
				return custom_errors.Internal("%s 响应解析失败: %s", provider, truncate(body, headers)).Wrap(err)
			}
			return nil
		}

		lastErr = custom_errors.Unavailable("%s 返回 HTTP %d: %s", provider, status, truncate(body, headers))
		// 限流和服务端故障是暂时的，其余（鉴权、参数错误）重试只会浪费配额。
		if status != http.StatusTooManyRequests && status < 500 {
			return lastErr
		}
	}
	return lastErr
}

func doOnce(ctx context.Context, hc *http.Client, url string, headers map[string]string, payload []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	// 出错时也要读完 body，错误信息里要带上厂商返回的原因。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// waitBackoff 做指数退避，同时响应 ctx 取消。
func waitBackoff(ctx context.Context, attempt int) error {
	delay := 500 * time.Millisecond << (attempt - 1)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// truncate 把上游响应体处理成可以安全拼进错误信息的字符串。
//
// 上游的错误响应是排查时最有价值的线索（「余额不足」「模型不存在」只在里面），
// 所以不能整段丢掉；但它同时是上游完全可控的内容，而部分网关会把收到的鉴权头
// 原样回显在错误体里——那就等于我们自己把密钥抄进了日志。
//
// headers 是本次实际发出去的请求头，密钥就在其中。把里面的凭证值交给
// SanitizeBody 做逐字替换，正好堵住上面那条路径：能被回显的密钥，
// 只可能是我们自己刚发出去的那一个，精确匹配不需要猜。
func truncate(body []byte, headers map[string]string) string {
	return httpx.SanitizeBody(body, bodyLimit, credentialValues(headers)...)
}

// credentialHeaders 是各家厂商放密钥的头名，全部小写比对。
//
// 按**头名**挑，而不是把所有头值一股脑当密钥：Content-Type 这种值拿去全文替换，
// 会把响应体里正常出现的 application/json 打成占位符，平白毁掉排查线索。
var credentialHeaders = map[string]bool{
	"authorization":  true,
	"x-api-key":      true,
	"x-goog-api-key": true,
	"api-key":        true,
}

func credentialValues(headers map[string]string) []string {
	out := make([]string, 0, 2)
	for k, v := range headers {
		if !credentialHeaders[strings.ToLower(k)] || v == "" {
			continue
		}
		out = append(out, v)
		// Authorization 的值是 "Bearer sk-xxx"。上游回显时未必带 Bearer 前缀，
		// 所以裸密钥也要单独加一条，否则精确匹配会整条落空。
		if rest := strings.TrimPrefix(v, "Bearer "); rest != v {
			out = append(out, rest)
		}
	}
	return out
}

func chatCompletionsURL(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return u + "/chat/completions"
}

// normalizeSchema 补齐空字段。ParamSchema 零值会序列化出 "properties": null，
// 而多数厂商的 function-calling 校验会直接拒绝。
func normalizeSchema(s agentvo.ParamSchema) agentvo.ParamSchema {
	if s.Type == "" {
		s.Type = "object"
	}
	if s.Properties == nil {
		s.Properties = map[string]agentvo.ParamField{}
	}
	return s
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func stringToRaw(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
