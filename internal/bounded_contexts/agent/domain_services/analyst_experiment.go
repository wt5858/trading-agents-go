package domain_services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

type AnalystExperiment struct {
	SnapshotID    string
	Baseline      entities.TurnResult
	Agent         entities.TurnResult
	BaselineError string
	AgentError    string
}

func (s *RuntimeService) CompareMarketAnalyst(ctx context.Context, turn entities.Turn) (AnalystExperiment, error) {
	return s.CompareAnalyst(ctx, turn)
}
func (s *RuntimeService) CompareAnalyst(ctx context.Context, turn entities.Turn) (AnalystExperiment, error) {
	if !turn.Contract.Kind.IsAnalyst() || turn.Snapshot.TradeDate.IsZero() {
		return AnalystExperiment{}, fmt.Errorf("analyst and explicit date required")
	}
	if s.router == nil {
		return AnalystExperiment{}, fmt.Errorf("model router unavailable")
	}
	client, model, err := s.router.Resolve(turn.Snapshot.Model)
	if err != nil {
		return AnalystExperiment{}, err
	}
	turn.Snapshot.Model = model
	reg, id := freezeMarket(turn)
	var inline strings.Builder
	for _, e := range regEvidence(reg) {
		raw, _ := json.Marshal(e)
		inline.Write(raw)
		inline.WriteByte('\n')
	}
	messages := []vo.Message{vo.SystemMessage("baseline: 你是" + turn.Contract.Kind.DisplayName() + "。职责：" + vo.AnalystObjective(turn.Contract.Kind) + "。仅根据已给出的冻结证据写中文分析报告；数据缺失如实声明，不编造事实。"), vo.UserMessage(inline.String())}
	baseline := *s
	baseline.cache = nil
	req := vo.ChatRequest{Model: model, Messages: messages, MaxTokens: 2400, Temperature: 0.3}
	out := AnalystExperiment{SnapshotID: id}
	resp, err := baseline.chat(ctx, client, model, req, ToolInvocation{Code: turn.Snapshot.Code, TradeDate: turn.Snapshot.TradeDate})
	out.Baseline = entities.TurnResult{Content: resp.Content, Usage: resp.Usage, Model: resp.Model, ModelCalls: resp.ModelCalls}
	out.Baseline.PromptChars, out.Baseline.PromptDigest = digestRequest(req)
	if err != nil {
		out.BaselineError = err.Error()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Agent, err = s.executeMarketAgent(ctx, turn)
	if err != nil {
		out.AgentError = err.Error()
	}
	return out, nil
}
