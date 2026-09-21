package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	agentvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	anthropicDefaultURL = "https://api.anthropic.com/v1/messages"
	anthropicVersion    = "2023-06-01"
	// anthropicDefaultMaxTokens 是兜底值：Anthropic 的 max_tokens 是必填字段，
	// 领域层不填会直接 400。
	anthropicDefaultMaxTokens = 4096
)

// AnthropicClient 对接 Anthropic Messages API。它和 OpenAI 协议的差异是结构性的，
// 无法靠改 baseURL 复用：system 是顶层字段、工具用 input_schema、
// 工具调用与结果都以 content block 形式往返。
type AnthropicClient struct {
	endpoint string
	apiKey   string
	hc       *http.Client
}

// NewAnthropic 构造客户端。baseURL 留空走官方地址，填了则用于自建网关/代理。
func NewAnthropic(apiKey, baseURL string, httpClient *http.Client) *AnthropicClient {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = anthropicDefaultURL
	}
	c := &AnthropicClient{endpoint: endpoint, apiKey: apiKey}
	c.hc = ensureClient(httpClient, c.Provider())
	return c
}

func (c *AnthropicClient) Provider() string { return "anthropic" }

func (c *AnthropicClient) Complete(ctx context.Context, req agentvo.CompletionRequest) (*agentvo.CompletionResult, error) {
	if req.Model == "" {
		return nil, custom_errors.Invalid("anthropic: 未指定模型")
	}

	system, messages := toAnthropicMessages(req.Messages)
	body := anRequest{
		Model:     req.Model,
		System:    system,
		Messages:  messages,
		Tools:     toAnthropicTools(req.Tools),
		MaxTokens: req.MaxTokens,
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = anthropicDefaultMaxTokens
	}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}

	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         c.apiKey,
		"anthropic-version": anthropicVersion,
	}

	// 模型名是配置项不是密钥，拿它当出站日志的目标标识（path 恒为 /v1/messages）。
	ctx = withOutboundTarget(ctx, req.Model)

	var resp anResponse
	if err := doJSON(ctx, c.hc, c.Provider(), c.endpoint, headers, &body, &resp); err != nil {
		return nil, err
	}

	var text strings.Builder
	var calls []agentvo.ToolCall
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			calls = append(calls, agentvo.ToolCall{
				ID:        agentvo.ToolCallID(blk.ID),
				Name:      agentvo.ToolName(blk.Name),
				Arguments: stringToRaw(string(blk.Input)),
			})
		}
	}

	usage := agentvo.Usage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		Calls:            1,
	}
	usage.CostUSD = CostOf(req.Model, usage)

	return &agentvo.CompletionResult{
		Content:      text.String(),
		ToolCalls:    calls,
		Usage:        usage,
		FinishReason: resp.StopReason,
	}, nil
}

// ---- 线上协议结构体 ----

type anRequest struct {
	Model       string      `json:"model"`
	System      string      `json:"system,omitempty"`
	Messages    []anMessage `json:"messages"`
	Tools       []anTool    `json:"tools,omitempty"`
	MaxTokens   int         `json:"max_tokens"`
	Temperature *float64    `json:"temperature,omitempty"`
}

type anMessage struct {
	Role    string    `json:"role"`
	Content []anBlock `json:"content"`
}

// anBlock 是 Anthropic 的多态 content block。用一个结构体加 omitempty 承载
// text / tool_use / tool_result 三种形态，比三个类型再套 union 更好读。
type anBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type anTool struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema agentvo.ParamSchema `json:"input_schema"`
}

type anResponse struct {
	Content    []anBlock `json:"content"`
	StopReason string    `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ---- 领域模型 -> 线上协议 ----

// toAnthropicMessages 把扁平的消息列表翻译成 Anthropic 的形态：
// system 抽成顶层字符串；tool 结果降级成 user 消息里的 tool_result block。
func toAnthropicMessages(msgs []agentvo.Message) (string, []anMessage) {
	var systems []string
	out := make([]anMessage, 0, len(msgs))

	for _, m := range msgs {
		switch m.Role {
		case agentvo.RoleSystem:
			if m.Content != "" {
				systems = append(systems, m.Content)
			}
		case agentvo.RoleTool:
			out = appendAnBlocks(out, "user", anBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID.String(),
				Content:   m.Content,
			})
		case agentvo.RoleAssistant:
			var blocks []anBlock
			if m.Content != "" {
				blocks = append(blocks, anBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anBlock{
					Type:  "tool_use",
					ID:    tc.ID.String(),
					Name:  tc.Name.String(),
					Input: json.RawMessage(rawToString(tc.Arguments)),
				})
			}
			out = appendAnBlocks(out, "assistant", blocks...)
		default:
			if m.Content != "" {
				out = appendAnBlocks(out, "user", anBlock{Type: "text", Text: m.Content})
			}
		}
	}
	return strings.Join(systems, "\n\n"), out
}

// appendAnBlocks 把同角色的连续消息合并成一条。Anthropic 要求 user/assistant
// 严格交替，多条并行工具结果必须挤进同一条 user 消息里。
func appendAnBlocks(msgs []anMessage, role string, blocks ...anBlock) []anMessage {
	if len(blocks) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, anMessage{Role: role, Content: blocks})
}

func toAnthropicTools(tools []agentvo.ToolSpec) []anTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anTool{
			Name:        t.Name.String(),
			Description: t.Description,
			InputSchema: normalizeSchema(t.Schema),
		})
	}
	return out
}
