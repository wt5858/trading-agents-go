package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	agentvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const googleDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// GoogleClient 对接 Gemini 的 generateContent REST 接口。它的差异点在于：
// 角色叫 user/model、系统提示词走 systemInstruction、
// 工具是 functionDeclarations、调用结果以 part 的形式嵌在消息里。
type GoogleClient struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// NewGoogle 构造客户端。baseURL 留空走官方地址。
func NewGoogle(apiKey, baseURL string, httpClient *http.Client) *GoogleClient {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b == "" {
		b = googleDefaultBaseURL
	}
	c := &GoogleClient{baseURL: b, apiKey: apiKey}
	c.hc = ensureClient(httpClient, c.Provider())
	return c
}

func (c *GoogleClient) Provider() string { return "google" }

func (c *GoogleClient) Complete(ctx context.Context, req agentvo.CompletionRequest) (*agentvo.CompletionResult, error) {
	if req.Model == "" {
		return nil, custom_errors.Invalid("google: 未指定模型")
	}

	body := gRequest{Contents: toGoogleContents(req.Messages)}
	if sys := systemText(req.Messages); sys != "" {
		body.SystemInstruction = &gContent{Parts: []gPart{{Text: sys}}}
	}
	if decls := toGoogleTools(req.Tools); len(decls) > 0 {
		body.Tools = []gTool{{FunctionDeclarations: decls}}
	}
	if req.Temperature > 0 || req.MaxTokens > 0 {
		body.GenerationConfig = &gGenerationConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		}
	}

	// 模型名嵌在路径里，需要转义，防止配置里带斜杠的名字拼出错误 URL。
	endpoint := c.baseURL + "/models/" + url.PathEscape(req.Model) + ":generateContent"
	headers := map[string]string{
		"Content-Type": "application/json",
		// 用 header 传 key，避免密钥出现在访问日志的 query string 里。
		"x-goog-api-key": c.apiKey,
	}

	// 模型名已经在 path 里，这里再给一份 target，是为了让三家的日志字段保持一致，
	// 查询时不必按 provider 分别写检索条件。
	ctx = withOutboundTarget(ctx, req.Model)

	var resp gResponse
	if err := doJSON(ctx, c.hc, c.Provider(), endpoint, headers, &body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Candidates) == 0 {
		return nil, custom_errors.Unavailable("google 未返回任何候选结果")
	}

	cand := resp.Candidates[0]
	var text strings.Builder
	var calls []agentvo.ToolCall
	for _, p := range cand.Content.Parts {
		if p.Text != "" {
			text.WriteString(p.Text)
		}
		if p.FunctionCall != nil {
			// Gemini 的 functionCall 没有调用 ID，用函数名兜底，
			// 这样后续回灌 tool 结果时仍能对上号。
			calls = append(calls, agentvo.ToolCall{
				ID:        agentvo.ToolCallID(p.FunctionCall.Name),
				Name:      agentvo.ToolName(p.FunctionCall.Name),
				Arguments: stringToRaw(string(p.FunctionCall.Args)),
			})
		}
	}

	usage := agentvo.Usage{
		PromptTokens:     resp.UsageMetadata.PromptTokenCount,
		CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		Calls:            1,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	usage.CostUSD = CostOf(req.Model, usage)

	return &agentvo.CompletionResult{
		Content:      text.String(),
		ToolCalls:    calls,
		Usage:        usage,
		FinishReason: cand.FinishReason,
	}, nil
}

// ---- 线上协议结构体 ----

type gRequest struct {
	Contents          []gContent         `json:"contents"`
	SystemInstruction *gContent          `json:"systemInstruction,omitempty"`
	Tools             []gTool            `json:"tools,omitempty"`
	GenerationConfig  *gGenerationConfig `json:"generationConfig,omitempty"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gPart struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *gFunctionCall    `json:"functionCall,omitempty"`
	FunctionResponse *gFunctionRespone `json:"functionResponse,omitempty"`
}

type gFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gFunctionRespone struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type gTool struct {
	FunctionDeclarations []gFunctionDecl `json:"functionDeclarations"`
}

type gFunctionDecl struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Parameters  agentvo.ParamSchema `json:"parameters"`
}

type gGenerationConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type gResponse struct {
	Candidates []struct {
		Content      gContent `json:"content"`
		FinishReason string   `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// ---- 领域模型 -> 线上协议 ----

func systemText(msgs []agentvo.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == agentvo.RoleSystem && m.Content != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

func toGoogleContents(msgs []agentvo.Message) []gContent {
	out := make([]gContent, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case agentvo.RoleSystem:
			// 已经提到 systemInstruction，这里跳过避免重复。
			continue
		case agentvo.RoleTool:
			name := m.Name
			if name == "" {
				name = m.ToolCallID.String()
			}
			out = appendGParts(out, "user", gPart{FunctionResponse: &gFunctionRespone{
				Name: name,
				// Gemini 要求 response 是对象，工具返回的纯文本得包一层。
				Response: toolResultObject(m.Content),
			}})
		case agentvo.RoleAssistant:
			var parts []gPart
			if m.Content != "" {
				parts = append(parts, gPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, gPart{FunctionCall: &gFunctionCall{
					Name: tc.Name.String(),
					Args: json.RawMessage(rawToString(tc.Arguments)),
				}})
			}
			out = appendGParts(out, "model", parts...)
		default:
			if m.Content != "" {
				out = appendGParts(out, "user", gPart{Text: m.Content})
			}
		}
	}
	return out
}

// appendGParts 合并同角色的相邻消息，让并行工具结果落在同一个 content 里。
func appendGParts(contents []gContent, role string, parts ...gPart) []gContent {
	if len(parts) == 0 {
		return contents
	}
	if n := len(contents); n > 0 && contents[n-1].Role == role {
		contents[n-1].Parts = append(contents[n-1].Parts, parts...)
		return contents
	}
	return append(contents, gContent{Role: role, Parts: parts})
}

func toGoogleTools(tools []agentvo.ToolSpec) []gFunctionDecl {
	if len(tools) == 0 {
		return nil
	}
	out := make([]gFunctionDecl, 0, len(tools))
	for _, t := range tools {
		out = append(out, gFunctionDecl{
			Name:        t.Name.String(),
			Description: t.Description,
			Parameters:  normalizeSchema(t.Schema),
		})
	}
	return out
}

func toolResultObject(content string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"result": content})
	if err != nil {
		return json.RawMessage(`{"result":""}`)
	}
	return b
}
