// Package mcp_tools 把分析上下文暴露成 MCP 工具。
//
// 它与 application/http_handlers 是同一层的两个传输适配器：都只做
// 「取出调用者身份 -> 调一个现成的 domain_services 方法 -> 投影成对外形状」，
// 业务规则一条都不住在这里。
//
// 本包不认识 MCP 服务器怎么装配，也不认识令牌怎么变成身份——它只要一个
// 能从 context 取到 Operator 的函数。这与 http_handlers 声明 OperatorResolver、
// 由组装根注入具体实现是同一个结构。
package mcp_tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
)

// OperatorResolver 从 context 里取出调用者身份。
//
// 与 http_handlers 的同名类型只差入参：那个吃 *gin.Context，这个吃
// context.Context。两者都由组装根（di/providers）提供实现，
// 因此本上下文既不依赖 gin，也不依赖 identity。
type OperatorResolver func(ctx context.Context) (analysis_services.Operator, error)

// Tools 是分析上下文的 MCP 工具集。
type Tools struct {
	svc     *analysis_services.AnalysisService
	resolve OperatorResolver
}

func NewTools(svc *analysis_services.AnalysisService, resolve OperatorResolver) *Tools {
	return &Tools{svc: svc, resolve: resolve}
}

type submitAnalysisInput struct {
	Code   string `json:"code" jsonschema:"股票代码，如 600519；必填"`
	Market string `json:"market,omitempty" jsonschema:"市场，CN/HK/US；留空则从代码推断"`
	// TradeDate 留空表示最近一个交易日。这个默认值在领域层，不在这里——
	// 这里写死一个「今天」会让 MCP 与 REST 两条入口的默认行为分叉。
	TradeDate string   `json:"tradeDate,omitempty" jsonschema:"交易日 YYYY-MM-DD，留空取最近交易日"`
	Depth     int      `json:"depth,omitempty" jsonschema:"分析深度：1 快速（仅分析师）、3 标准（含多空辩论与交易决策）、5 深度（含风控评估）；留空取 3"`
	Analysts  []string `json:"analysts,omitempty" jsonschema:"指定分析师，可选 market/fundamentals/news/sentiment/sector/index；留空取全部"`
	LLMModel  string   `json:"llmModel,omitempty" jsonschema:"指定模型，留空走默认路由"`
}

type submitAnalysisOutput struct {
	TaskID string `json:"taskId" jsonschema:"分析任务 ID，用 get_analysis_task 轮询进度与结论"`
	Status string `json:"status" jsonschema:"任务状态"`
	Symbol string `json:"symbol" jsonschema:"标的代码"`
	// 明确告诉模型这是个异步任务，免得它拿到 taskId 就开始编造分析结论。
	Hint string `json:"hint" jsonschema:"后续操作提示"`
}

type getAnalysisTaskInput struct {
	TaskID string `json:"taskId" jsonschema:"分析任务 ID，来自 submit_analysis"`
}

type analysisDecisionOut struct {
	Action     string `json:"action" jsonschema:"交易建议：buy/increase/hold/reduce/sell/undecided"`
	ActionText string `json:"actionText" jsonschema:"交易建议的中文说明"`
	Confidence string `json:"confidence,omitempty" jsonschema:"置信度 0-1"`
	RiskScore  string `json:"riskScore,omitempty" jsonschema:"风险评分 0-10，越高越危险"`
	Summary    string `json:"summary,omitempty" jsonschema:"一句话结论"`
	Reasoning  string `json:"reasoning,omitempty" jsonschema:"决策依据"`
}

type getAnalysisTaskOutput struct {
	TaskID     string `json:"taskId"`
	Status     string `json:"status" jsonschema:"queued/running/completed/failed/canceled"`
	StatusText string `json:"statusText" jsonschema:"状态的中文说明"`
	Symbol     string `json:"symbol"`
	TradeDate  string `json:"tradeDate"`
	Percent    string `json:"percent,omitempty" jsonschema:"进度百分比"`
	// Decision 只在 completed 时出现。未完成就给一个空决策，
	// 会让模型把 action="" 当成「系统建议不操作」。
	Decision *analysisDecisionOut `json:"decision,omitempty" jsonschema:"终局决策，仅任务完成时出现"`
	Error    string               `json:"error,omitempty" jsonschema:"失败原因，仅任务失败时出现"`
	// Disclaimer 随每一份结论一起返回，不是可选项：
	// 这是投资相关内容，下游模型很可能把它直接转述给终端用户。
	Disclaimer string `json:"disclaimer,omitempty" jsonschema:"风险提示"`
}

const analysisDisclaimer = "本结论由大模型生成，仅供研究，不构成投资建议；" +
	"过往表现不代表未来收益，投资有风险，可能损失本金。"

// RegisterTools 把本上下文的工具注册到 MCP 服务器上。
//
// 方法名与签名是 mcpserver 那边声明的窄接口的形状。本包不 import 那个接口：
// Go 的结构化类型让「满足接口」不需要依赖声明方，而这正是 mcpserver
// 能够完全不认识本上下文的原因。
func (t *Tools) RegisterTools(srv *mcp.Server) {
	svc := t.svc

	mcp.AddTool(srv, &mcp.Tool{
		Name: "submit_analysis",
		Description: "对一只股票发起一次多智能体分析。这是一个异步任务：" +
			"本工具立刻返回任务 ID，分析本身要跑几分钟，" +
			"之后用 get_analysis_task 查询进度和结论。不要凭空编造分析结果。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in submitAnalysisInput,
	) (*mcp.CallToolResult, submitAnalysisOutput, error) {
		op, err := t.resolve(ctx)
		if err != nil {
			return nil, submitAnalysisOutput{}, err
		}
		// 入参一字不改地交给领域服务：代码规整、市场推断、深度与分析师的
		// 默认值全在 buildRequest 里。在这里补默认值会让两条入口分叉。
		task, err := svc.Submit(ctx, op, analysis_services.SubmitInput{
			Code:      in.Code,
			Market:    in.Market,
			TradeDate: in.TradeDate,
			Depth:     in.Depth,
			Analysts:  in.Analysts,
			LLMModel:  in.LLMModel,
		})
		if err != nil {
			return nil, submitAnalysisOutput{}, err
		}
		return nil, submitAnalysisOutput{
			TaskID: task.ID,
			Status: task.Status.String(),
			Symbol: task.Request.Code.FullSymbol(),
			Hint:   "分析通常需要数分钟。请用 get_analysis_task 查询该任务的进度与结论。",
		}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_analysis_task",
		Description: "查询一次分析任务的状态、进度与最终结论。" +
			"只能查询属于当前用户的任务。任务未完成时不会返回决策。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getAnalysisTaskInput,
	) (*mcp.CallToolResult, getAnalysisTaskOutput, error) {
		op, err := t.resolve(ctx)
		if err != nil {
			return nil, getAnalysisTaskOutput{}, err
		}
		// 归属校验在 GetTask 里，这里不重复。
		task, err := svc.GetTask(ctx, op, in.TaskID)
		if err != nil {
			return nil, getAnalysisTaskOutput{}, err
		}

		out := getAnalysisTaskOutput{
			TaskID:     task.ID,
			Status:     task.Status.String(),
			StatusText: task.Status.DisplayName(),
			Symbol:     task.Request.Code.FullSymbol(),
			TradeDate:  task.Request.TradeDate.String(),
			Percent:    task.Progress.Percent.String(),
			Error:      task.ErrMsg,
		}
		if task.Result != nil {
			d := task.Result.Decision
			out.Decision = &analysisDecisionOut{
				Action:     d.Action.String(),
				ActionText: d.Action.DisplayName(),
				Confidence: d.Confidence.String(),
				RiskScore:  d.RiskScore.String(),
				Summary:    d.Summary,
				Reasoning:  d.Reasoning,
			}
			out.Disclaimer = analysisDisclaimer
		}
		return nil, out, nil
	})
}
