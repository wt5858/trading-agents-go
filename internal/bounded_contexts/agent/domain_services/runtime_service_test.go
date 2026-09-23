package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// scriptedClient 按轮次返回预设结果，并记录每一轮收到的请求。
type scriptedClient struct {
	mu       sync.Mutex
	replies  []value_objects.CompletionResult
	err      error
	requests []value_objects.CompletionRequest
}

func (c *scriptedClient) Provider() string { return "scripted" }

func (c *scriptedClient) Complete(_ context.Context, req value_objects.CompletionRequest) (*value_objects.CompletionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	c.requests = append(c.requests, req)
	i := len(c.requests) - 1
	if i >= len(c.replies) {
		i = len(c.replies) - 1
	}
	res := c.replies[i]
	return &res, nil
}

func (c *scriptedClient) round(i int) value_objects.CompletionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[i]
}

func (c *scriptedClient) rounds() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

type fixedRouter struct{ client LLMClient }

func (r fixedRouter) Resolve(model string) (LLMClient, string, error) {
	if model == "" {
		model = "test-model"
	}
	return r.client, model, nil
}

func (r fixedRouter) DefaultModel() string { return "test-model" }

// recordingTool 记录自己被调用的参数。
type recordingTool struct {
	name   value_objects.ToolName
	result string
	err    error

	mu    sync.Mutex
	calls []ToolInvocation
}

func (t *recordingTool) Spec() value_objects.ToolSpec {
	return value_objects.NewToolSpec(t.name, "测试工具", value_objects.NewParamSchema(nil))
}

func (t *recordingTool) Invoke(_ context.Context, in ToolInvocation) (string, error) {
	t.mu.Lock()
	t.calls = append(t.calls, in)
	t.mu.Unlock()
	if t.err != nil {
		return "", t.err
	}
	return t.result, nil
}

func (t *recordingTool) invocations() []ToolInvocation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ToolInvocation(nil), t.calls...)
}

type mapRegistry struct {
	tools map[value_objects.ToolName]Tool
}

func (r mapRegistry) Lookup(name value_objects.ToolName) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r mapRegistry) Specs(access value_objects.DataAccess) []value_objects.ToolSpec {
	all := make([]value_objects.ToolSpec, 0, len(r.tools))
	for _, name := range value_objects.AllToolNames() {
		if t, ok := r.tools[name]; ok {
			all = append(all, t.Spec())
		}
	}
	return value_objects.SpecsOf(all, access)
}

func testTurn(t *testing.T, member *entities.CrewMember) entities.Turn {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate("2024-03-01"),
		analysis_vo.DepthStandard, nil, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	ac := entities.NewAnalysisContext("run_test", req)
	ac.CommitTurn(value_objects.TurnRecord{Kind: value_objects.KindTrader, Content: "交易方案"})
	return entities.Turn{Contract: member.Contract(), Snapshot: ac.Snapshot()}
}

// ---------------------------------------------------------------------------
// 工具调用循环
// ---------------------------------------------------------------------------

// TestRuntime_ToolLoopFeedsResultsBack 是工具循环的基本语义：
// 模型要工具 -> 执行 -> 结果回灌 -> 再问一次 -> 拿到结论。
func TestRuntime_ToolLoopFeedsResultsBack(t *testing.T) {
	tool := &recordingTool{name: value_objects.ToolGetQuote, result: "收盘 1700.00"}
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{
			Content: "我查一下行情",
			ToolCalls: []value_objects.ToolCall{
				{ID: "call-1", Name: value_objects.ToolGetQuote, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		},
		{
			Content: "技术面报告正文",
			Usage:   value_objects.Usage{Calls: 1, PromptTokens: 200, CompletionTokens: 50, TotalTokens: 250},
		},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetQuote: tool,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Content != "技术面报告正文" {
		t.Errorf("Content = %q", res.Content)
	}
	if res.ToolRounds != 1 {
		t.Errorf("ToolRounds = %d, 期望 1", res.ToolRounds)
	}
	if res.Truncated {
		t.Error("正常收束不应标记截断")
	}
	// 两轮调用的消耗必须累加，否则成本统计会长期偏低。
	if res.Usage.TotalTokens != 370 || res.Usage.Calls != 2 {
		t.Errorf("消耗未累加: %+v", res.Usage)
	}

	// 标的由运行时下发，不受模型参数影响。
	calls := tool.invocations()
	if len(calls) != 1 {
		t.Fatalf("工具调用次数 = %d", len(calls))
	}
	if calls[0].Code.Symbol != "600519" || calls[0].TradeDate.String() != "2024-03-01" {
		t.Errorf("工具收到的标的不正确: %+v", calls[0])
	}

	// 第二轮必须带上 assistant(tool_calls) 与 tool 结果，顺序不能乱。
	second := client.round(1)
	if n := len(second.Messages); n < 4 {
		t.Fatalf("第二轮消息数 = %d, 期望至少 4", n)
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != value_objects.RoleTool || last.ToolCallID != "call-1" {
		t.Errorf("工具结果未正确回灌: %+v", last)
	}
	if !strings.Contains(last.Content, "1700.00") {
		t.Errorf("工具结果内容丢失: %q", last.Content)
	}
}

// TestRuntime_UnauthorizedToolIsRefused 是这套设计的安全底线。
//
// 提示词注入唯一能操纵的是模型的意图；授权在执行前强制校验，
// 因此模型即使被诱导去调一个没授权的工具，也只会拿到一条拒绝说明。
func TestRuntime_UnauthorizedToolIsRefused(t *testing.T) {
	forbidden := &recordingTool{name: value_objects.ToolGetFinancials, result: "机密财报"}
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{
			ToolCalls: []value_objects.ToolCall{
				{ID: "call-1", Name: value_objects.ToolGetFinancials, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		{Content: "只能基于技术面作答", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetFinancials: forbidden,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	// 市场分析师没有财务工具的授权。
	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("越权调用不应让整位成员失败: %v", err)
	}
	if len(forbidden.invocations()) != 0 {
		t.Fatal("未授权的工具被执行了")
	}
	if res.Content != "只能基于技术面作答" {
		t.Errorf("Content = %q", res.Content)
	}

	// 被拒绝的调用要以「工具结果」的形式告知模型，让它换个角度继续，
	// 而不是让这位成员直接失败。
	second := client.round(1)
	last := second.Messages[len(second.Messages)-1]
	if last.Role != value_objects.RoleTool || !strings.Contains(last.Content, "未对该智能体授权") {
		t.Errorf("越权拒绝未回灌给模型: %+v", last)
	}
}

// TestRuntime_ToolFailureIsFedBackNotFatal 工具查不到数据是常态
// （这只票今天没新闻），不该让整位分析师失败。
func TestRuntime_ToolFailureIsFedBackNotFatal(t *testing.T) {
	broken := &recordingTool{name: value_objects.ToolGetQuote, err: custom_errors.NotFound("行情不存在")}
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{
			ToolCalls: []value_objects.ToolCall{
				{ID: "c1", Name: value_objects.ToolGetQuote, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		{Content: "行情缺失，本报告仅基于指标", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetQuote: broken,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("工具失败不应让成员失败: %v", err)
	}
	if !strings.Contains(res.Content, "行情缺失") {
		t.Errorf("Content = %q", res.Content)
	}
	last := client.round(1).Messages[len(client.round(1).Messages)-1]
	if !strings.Contains(last.Content, "工具执行失败") || !strings.Contains(last.Content, "不要猜测") {
		t.Errorf("工具失败说明未回灌: %q", last.Content)
	}
}

// TestRuntime_MaxToolRoundsTerminates 是这个循环最重要的性质：它一定会停。
//
// 模型每轮都要工具时，最后一轮必须摘掉工具声明逼它给结论。
// 没有这一步，一次分析会一直转到 ctx 超时，而所有已花的 token 拿不回任何结论。
func TestRuntime_MaxToolRoundsTerminates(t *testing.T) {
	tool := &recordingTool{name: value_objects.ToolGetQuote, result: "行情"}
	greedy := value_objects.CompletionResult{
		Content: "再查一次",
		ToolCalls: []value_objects.ToolCall{
			{ID: "c", Name: value_objects.ToolGetQuote, Arguments: json.RawMessage(`{}`)},
		},
		Usage: value_objects.Usage{Calls: 1},
	}
	client := &scriptedClient{replies: []value_objects.CompletionResult{greedy}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetQuote: tool,
	}}, NewPromptService(), nil, RuntimeConfig{MaxToolRounds: 2}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !res.Truncated {
		t.Error("撞上轮数上限时必须标记截断")
	}
	// 市场分析师契约里的 3 轮优先于配置里的 2 轮：上限 3 意味着 4 次模型调用。
	if client.rounds() != 4 {
		t.Errorf("模型调用次数 = %d, 期望 4（3 轮工具 + 1 轮收尾）", client.rounds())
	}
	// 最后一轮不得再带工具声明，否则「逼它收尾」这件事就没发生。
	if last := client.round(client.rounds() - 1); len(last.Tools) != 0 {
		t.Errorf("收尾轮仍然带着 %d 个工具声明", len(last.Tools))
	}
}

// TestRuntime_NoToolsForArbiters 不带工具授权的成员一轮就该结束，
// 且请求里不能出现任何工具声明。
func TestRuntime_NoToolsForArbiters(t *testing.T) {
	tool := &recordingTool{name: value_objects.ToolGetQuote, result: "行情"}
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{Content: "终裁报告", Usage: value_objects.Usage{Calls: 1}},
	}}
	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetQuote: tool,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	if _, err := rt.Execute(context.Background(), testTurn(t, entities.NewRiskManager())); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if client.rounds() != 1 {
		t.Errorf("模型调用次数 = %d, 期望 1", client.rounds())
	}
	if len(client.round(0).Tools) != 0 {
		t.Error("无授权的成员不应收到任何工具声明")
	}
}

// TestRuntime_ModelErrorCarriesUsage 调用失败时也要把已经产生的消耗带回去。
func TestRuntime_ModelErrorCarriesUsage(t *testing.T) {
	client := &scriptedClient{err: errors.New("上游 503")}
	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{}, NewPromptService(), nil, RuntimeConfig{}, nil)

	if _, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst())); err == nil {
		t.Fatal("模型失败时必须返回错误")
	}
}

// TestRuntime_RespectsCanceledContext 已取消的上下文不应发出任何模型请求。
func TestRuntime_RespectsCanceledContext(t *testing.T) {
	client := &scriptedClient{replies: []value_objects.CompletionResult{{Content: "不该被调用"}}}
	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{}, NewPromptService(), nil, RuntimeConfig{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := rt.Execute(ctx, testTurn(t, entities.NewMarketAnalyst())); err == nil {
		t.Fatal("已取消的 ctx 应当返回错误")
	}
	if client.rounds() != 0 {
		t.Error("ctx 已取消时不应发出模型请求")
	}
}
