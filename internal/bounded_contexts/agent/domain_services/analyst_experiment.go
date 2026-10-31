package domain_services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

type AnalystExperiment struct {
	ArmBudgetUSD       decimal.Decimal
	BaselineDurationMS int64
	AgentDurationMS    int64
	SnapshotID         string
	Baseline           entities.TurnResult
	Agent              entities.TurnResult
	BaselineError      string
	AgentError         string
}

func (s *RuntimeService) CompareMarketAnalyst(ctx context.Context, turn entities.Turn) (AnalystExperiment, error) {
	return s.CompareAnalyst(ctx, turn)
}
func (s *RuntimeService) CompareAnalyst(ctx context.Context, turn entities.Turn) (AnalystExperiment, error) {
	if reason := turn.Snapshot.AnalystSkipReason(turn.Contract.Kind); reason != "" {
		result := entities.TurnResult{Skipped: true, SkipReason: reason}
		return AnalystExperiment{Agent: result, Baseline: result}, nil
	}
	if !turn.Contract.Kind.IsAnalyst() || turn.Snapshot.TradeDate.IsZero() {
		return AnalystExperiment{}, fmt.Errorf("analyst and explicit date required")
	}
	reg, id := freezeMarket(turn)
	if report, pages, missing := analystMissingPrimary(turn, reg); missing {
		raw, _ := json.Marshal(report)
		result := entities.TurnResult{AnalystReport: &report, Content: report.Markdown(), AgentArtifact: makeAnalystArtifact(turn.Contract.Kind, id, string(raw), pages, true)}
		return AnalystExperiment{SnapshotID: id, Agent: result, Baseline: result}, nil
	}
	if s.router == nil {
		return AnalystExperiment{}, fmt.Errorf("model router unavailable")
	}
	client, model, err := s.router.Resolve(turn.Snapshot.Model)
	if err != nil {
		return AnalystExperiment{}, err
	}
	turn.Snapshot.Model = model
	var inline strings.Builder
	pages := allFrozenPages(reg)
	for _, e := range pages {
		raw, _ := json.Marshal(e)
		inline.Write(raw)
		inline.WriteByte('\n')
	}
	messages := []vo.Message{vo.SystemMessage("baseline: " + analystSystemPrompt + " 身份:" + turn.Contract.Kind.DisplayName() + "。职责:" + vo.AnalystObjective(turn.Contract.Kind) + "。证据已内联，不调用工具。"), vo.UserMessage(inline.String())}
	baseline := *s
	baseline.cache = nil
	req := vo.ChatRequest{Model: model, Messages: messages, MaxTokens: 3600, Temperature: 0.3}
	allowance := armAllowance(ctx)
	out := AnalystExperiment{SnapshotID: id, ArmBudgetUSD: allowance}
	started := time.Now()
	resp, err := baseline.chat(armContext(ctx, allowance), client, model, req, ToolInvocation{Code: turn.Snapshot.Code, TradeDate: turn.Snapshot.TradeDate})
	out.BaselineDurationMS = time.Since(started).Milliseconds()
	out.Baseline = entities.TurnResult{Content: resp.Content, Usage: resp.Usage, Model: resp.Model, ModelCalls: resp.ModelCalls}
	out.Baseline.PromptChars, out.Baseline.PromptDigest = digestRequest(req)

	valid := false
	if err == nil {
		report, validationErr := validateAnalystOutput(turn, reg, resp.Content, pages)
		err = validationErr
		if err == nil {
			out.Baseline.AnalystReport = &report
			out.Baseline.Content = report.Markdown()
			valid = true
		}
	}
	out.Baseline.AgentArtifact = makeAnalystArtifact(turn.Contract.Kind, id, resp.Content, pages, valid)
	if err != nil {
		out.BaselineError = err.Error()
		out.Baseline.Content = ""
	}

	if err := ctx.Err(); err != nil {
		return out, err
	}
	started = time.Now()
	out.Agent, err = s.executeMarketAgent(armContext(ctx, allowance), turn)
	out.AgentDurationMS = time.Since(started).Milliseconds()
	if err != nil {
		out.AgentError = err.Error()
	}
	return out, nil
}
