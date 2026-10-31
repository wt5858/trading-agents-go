package llm

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/shopspring/decimal"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

// ResearchMock validates orchestration only; its answers are not quality evidence.
type ResearchMock struct{}

func (ResearchMock) Provider() string { return "research-mock" }
func (ResearchMock) EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool) {
	return decimal.Zero, true
}
func (ResearchMock) Complete(ctx context.Context, r vo.CompletionRequest) (*vo.CompletionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(r.Messages) == 0 {
		return EvaluationMock{}.Complete(ctx, r)
	}
	stage := strings.Split(r.Messages[0].Content, "\n")[0]
	var output any
	switch stage {
	case "research:plan":
		var input struct {
			Question  string               `json:"question"`
			Available map[vo.AgentKind]int `json:"available_primary_segments"`
		}
		_ = json.Unmarshal([]byte(r.Messages[len(r.Messages)-1].Content), &input)
		kind := vo.KindFundamentalsAnalyst
		if strings.Contains(input.Question, "新闻") {
			kind = vo.KindNewsAnalyst
		} else if strings.Contains(input.Question, "行情") {
			kind = vo.KindMarketAnalyst
		}
		output = vo.ResearchPlan{Questions: []vo.ResearchQuestion{{ID: "q1", Analyst: kind, Question: input.Question, Acceptance: []string{"查询本职证据并检查反例；缺失资料必须说明"}}}}
	case "research:review":
		var trace vo.ResearchTrace
		_ = json.Unmarshal([]byte(r.Messages[len(r.Messages)-1].Content), &trace)
		review := vo.ResearchReview{Gaps: []vo.ResearchGap{}}
		for _, q := range trace.Plan.Questions {
			found := false
			for _, f := range trace.Findings {
				found = found || f.QuestionID == q.ID
			}
			if !found {
				review.Gaps = append(review.Gaps, vo.ResearchGap{QuestionID: q.ID, Reason: "冻结证据不足，无法回答研究问题"})
			}
		}
		output = review
	case "research:synthesis":
		var trace vo.ResearchTrace
		_ = json.Unmarshal([]byte(r.Messages[len(r.Messages)-1].Content), &trace)
		selection := vo.ResearchSelection{FindingIDs: []string{}}
		for _, f := range trace.Findings {
			selection.FindingIDs = append(selection.FindingIDs, f.ID)
		}
		output = selection
	default:
		return EvaluationMock{}.Complete(ctx, r)
	}
	raw, err := json.Marshal(output)
	return &vo.CompletionResult{Content: string(raw), Usage: vo.Usage{Calls: 1, TotalTokens: 30, PromptTokens: 10, CompletionTokens: 20, CostStatus: "mock"}, FinishReason: "stop"}, err
}
