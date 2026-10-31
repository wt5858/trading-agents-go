package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

type analystValidationAttempt struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// This is a deterministic abstention, not a model-authored finding. Preparation
// already tried to recover missing data; auxiliary quotes cannot replace core data.
func analystMissingPrimary(turn entities.Turn, reg frozenRegistry) (vo.AnalystReport, []frozenEvidence, bool) {
	var missing []string
	var pages []frozenEvidence
	for _, tool := range reg.tools {
		if !vo.IsPrimaryAnalystTool(turn.Contract.Kind, tool.spec.Name) {
			continue
		}
		if !tool.evidence.Missing && len(tool.documents) > 0 {
			return vo.AnalystReport{}, nil, false
		}
		reason := tool.evidence.MissingReason
		if reason == "" {
			reason = "本次冻结快照没有可用的 " + tool.spec.Name.String() + " 记录"
		}
		missing = append(missing, reason)
		pages = append(pages, tool.evidence)
	}
	if len(missing) == 0 {
		return vo.AnalystReport{}, nil, false
	}
	return vo.AnalystReport{Status: "insufficient", Direction: "neutral", Findings: []vo.AnalystFinding{}, Missing: missing, SupportVerified: true}, pages, true
}

func (s *RuntimeService) repairAnalyst(ctx context.Context, client LLMClient, model string, turn entities.Turn, pages []frozenEvidence, original string, validation error, base entities.TurnResult, tokenBudget int) (vo.ChatResponse, error) {
	if base.Usage.TotalTokens >= tokenBudget {
		return vo.ChatResponse{}, fmt.Errorf("analyst token budget exhausted before repair")
	}
	prior := []rune(original)
	if len(prior) > 3600 {
		prior = prior[:3600]
	}
	payload, _ := json.Marshal(map[string]any{"analyst": turn.Contract.Kind, "objective": vo.AnalystObjective(turn.Contract.Kind), "validation_error": validation.Error(), "previous_output": string(prior), "evidence": pages})
	req := vo.ChatRequest{Model: model, Messages: []vo.Message{vo.SystemMessage(analystSystemPrompt + analystRepairPrompt), vo.UserMessage(string(payload))}, MaxTokens: 3200, Temperature: 0.1, TokenBudget: tokenBudget - base.Usage.TotalTokens}
	if observer, ok := ctx.Value(modelObserverKey{}).(modelObserver); ok {
		ctx = context.WithValue(ctx, modelObserverKey{}, modelObserver(func(partial entities.TurnResult) error {
			partial.Usage = base.Usage.Plus(partial.Usage)
			partial.ModelCalls = append(append([]vo.ModelCallRecord(nil), base.ModelCalls...), shiftRepairCalls(partial.ModelCalls, len(base.ModelCalls))...)
			partial.ToolCalls = append(append([]vo.ToolCallRecord(nil), base.ToolCalls...), partial.ToolCalls...)
			return observer(partial)
		}))
	}
	response, err := s.chat(ctx, client, model, req, ToolInvocation{Code: turn.Snapshot.Code, TradeDate: turn.Snapshot.TradeDate})
	response.ModelCalls = shiftRepairCalls(response.ModelCalls, len(base.ModelCalls))
	return response, err
}
func shiftRepairCalls(calls []vo.ModelCallRecord, offset int) []vo.ModelCallRecord {
	out := append([]vo.ModelCallRecord(nil), calls...)
	for i := range out {
		out[i].Round += offset
	}
	return out
}

func analystValidationMessage(err error) string {
	var failure *vo.AnalystValidationError
	if !errors.As(err, &failure) {
		return "输出未满足证据契约"
	}
	message := map[string]string{"invalid_json": "输出被截断或不是有效 JSON", "invalid_contract": "输出字段不符合分析报告契约", "unavailable_evidence": "引用了未读取或不可用的证据", "primary_evidence": "结论缺少本职证据", "quote_mismatch": "引用原文与证据不匹配", "fact_not_extract": "事实结论不是原文摘录"}[failure.Code]
	if failure.Finding >= 0 {
		return fmt.Sprintf("第%d条%s", failure.Finding+1, message)
	}
	return message
}

const analystRepairPrompt = `
上一份输出未通过校验，本次仅允许修正交付，不允许调用工具或补造资料。原始工具证据和错误原因在用户消息中。
必须遵守：1. 严格JSON，不使用Markdown代码块。2. 最多3条findings，每条claim不超过100字，引用最多2个ID，每段引文不超过120字；risks与missing各最多3条。
3. missing=true的页、missing_reason、preparation只描述取数状态，不是可引用的事实证据，写入missing即可。没有本职证据时输出insufficient、neutral、findings=[]，不要使用辅助行情凑结论。
4. 每条finding必须包含本职资料的引用。只基于辅助行情的观察移入risks，不要放入findings。
5. fact.claim必须是引文中的逐字原文；若包含概括或推理，应如实标为inference，且引文必须确实支持该结论。不得只更改标签为无关结论通关。
6. 引文必须逐字出现在对应ID的data/content中。缩短引文、删除无支持论断，不得改写引文或创造ID。`
