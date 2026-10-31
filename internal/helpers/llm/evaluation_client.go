package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/shopspring/decimal"
	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

// CappedClient limits attempted requests across concurrent evaluation cases.
type CappedClient struct {
	Client svc.LLMClient
	Limit  int
	mu     sync.Mutex
	used   int
}

func (c *CappedClient) Provider() string { return c.Client.Provider() }
func (c *CappedClient) Complete(ctx context.Context, r vo.CompletionRequest) (*vo.CompletionResult, error) {
	if err := c.Claim(); err != nil {
		return nil, err
	}
	return c.Client.Complete(ctx, r)
}

// EvaluationMock is only used by explicit --mock runs. It measures plumbing, not quality.
type EvaluationMock struct{}

func (EvaluationMock) Provider() string { return "evaluation-mock" }
func (EvaluationMock) Complete(ctx context.Context, r vo.CompletionRequest) (*vo.CompletionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &vo.CompletionResult{Usage: vo.Usage{Calls: 1, PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CostStatus: "mock"}, FinishReason: "stop"}
	task := ""
	for _, m := range r.Messages {
		task += m.Content
	}
	preferred := vo.ToolGetQuote
	for _, p := range []struct {
		word string
		tool vo.ToolName
	}{{"基本面分析师", vo.ToolGetFinancials}, {"新闻面分析师", vo.ToolGetNews}, {"市场情绪分析师", vo.ToolGetSocialSentiment}, {"板块轮动分析师", vo.ToolGetSectorContext}, {"大盘环境分析师", vo.ToolGetIndexContext}} {
		if strings.Contains(task, p.word) {
			preferred = p.tool
		}
	}
	type evidence struct {
		ID      string      `json:"evidence_id"`
		Missing bool        `json:"missing"`
		Data    string      `json:"data"`
		Tool    vo.ToolName `json:"tool"`
	}
	respond := func(e evidence) *vo.CompletionResult {
		report := vo.AnalystReport{Status: "complete", Direction: "neutral", Findings: []vo.AnalystFinding{{Kind: "fact", Claim: e.Data, EvidenceIDs: []string{e.ID}, EvidenceQuotes: map[string]string{e.ID: e.Data}}}}
		if e.Missing {
			report = vo.AnalystReport{Status: "insufficient", Direction: "neutral", Missing: []string{"本职数据缺失"}}
		}
		raw, _ := json.Marshal(report)
		result.Content = string(raw)
		return result
	}
	baseline := len(r.Messages) > 0 && strings.HasPrefix(r.Messages[0].Content, "baseline:")
	for _, m := range r.Messages {
		if m.Role == vo.RoleTool {
			var e evidence
			if json.Unmarshal([]byte(m.Content), &e) == nil && e.ID != "" {
				return respond(e), nil
			}
		}
		if baseline && m.Role == vo.RoleUser {
			for _, line := range strings.Split(m.Content, "\n") {
				var e evidence
				if json.Unmarshal([]byte(line), &e) == nil && e.Tool == preferred {
					return respond(e), nil
				}
			}
		}
	}
	for _, tool := range r.Tools {
		if tool.Name == preferred {
			result.ToolCalls = []vo.ToolCall{{ID: "mock_tool", Name: preferred}}
			return result, nil
		}
	}
	result.Content = `{"status":"insufficient","direction":"neutral","findings":[],"missing":["无可用工具"]}`
	return result, nil
}

func (c *CappedClient) Claim() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used >= c.Limit {
		return fmt.Errorf("evaluation request cap reached")
	}
	c.used++
	return nil
}

func (EvaluationMock) EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool) {
	return decimal.Zero, true
}
func (c *CappedClient) EstimateCost(r vo.CompletionRequest) (decimal.Decimal, bool) {
	if e, ok := c.Client.(interface {
		EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool)
	}); ok {
		return e.EstimateCost(r)
	}
	return decimal.Zero, false
}
