package entities

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
)

type gatedRuntime struct {
	status                    string
	analystCalls, traderCalls int
}

func (r *gatedRuntime) Execute(_ context.Context, turn Turn) (TurnResult, error) {
	if turn.Contract.Kind.IsAnalyst() {
		r.analystCalls++
		report := vo.AnalystReport{Status: r.status, Direction: "neutral", SupportVerified: true}
		if r.status == "complete" || r.status == "hypothesis" {
			report.Findings = []vo.AnalystFinding{{Kind: "inference", Claim: "已核验", EvidenceIDs: []string{"source"}}}
		} else {
			report.Missing = []string{"缺少核心证据"}
		}
		if r.status == "hypothesis" {
			report.Status = "complete"
			report.Findings[0].Kind = "hypothesis"
		}
		return TurnResult{Content: report.Markdown(), AnalystReport: &report}, nil
	}
	r.traderCalls++
	return TurnResult{Content: "交易方案"}, nil
}
func TestInsufficientAndPartialReportsNeverUnlockTrade(t *testing.T) {
	for _, status := range []string{"insufficient", "partial", "hypothesis", "complete"} {
		t.Run(status, func(t *testing.T) {
			ac := newTestContext(t, analysis.DepthStandard)
			rt := &gatedRuntime{status: status}
			crew := NewCrew()
			plan := Plan{Stages: []Stage{{Phase: vo.PhaseAnalyst, Members: []Agent{crew.Member(vo.KindMarketAnalyst)}, MinSuccess: 1}, {Phase: vo.PhaseTrading, Members: []Agent{crew.Member(vo.KindTrader)}, MinSuccess: 1}}}
			_, err := NewOrchestrator(plan, nil).Run(context.Background(), rt, ac)
			if err != nil {
				t.Fatal(err)
			}
			if status != "complete" {
				if rt.traderCalls != 0 || ac.FinalDecision().Decision.Action != analysis.ActionUndecided {
					t.Fatal("insufficient analysis reached trading")
				}
				if len(ac.Reports()) != 1 {
					t.Fatal("non-actionable report lost")
				}
			} else if rt.traderCalls != 1 {
				t.Fatal("verified complete report failed to unlock trading")
			}
		})
	}
}
func TestExecutionIdentityAndPendingAccounting(t *testing.T) {
	a := newTestContext(t, analysis.DepthStandard)
	b := newTestContext(t, analysis.DepthStandard)
	if a.ExecutionID() == b.ExecutionID() || a.RunID() != b.RunID() {
		t.Fatal("retry identity collision")
	}
	usage := vo.Usage{Calls: 1, TotalTokens: 12, CostUSD: decimal.RequireFromString("0.01")}
	a.RecordPending(vo.KindMarketAnalyst, TurnResult{Usage: usage, ModelCalls: []vo.ModelCallRecord{{Pending: true}}})
	snap := a.TraceSnapshot(a.StartedAt(), "")
	if len(snap.PendingTurns) != 1 || snap.Usage.TotalTokens != 12 {
		t.Fatal("pending costs missing")
	}
	a.CommitTurn(vo.TurnRecord{Kind: vo.KindMarketAnalyst, Usage: usage, Content: "report"})
	snap = a.TraceSnapshot(a.StartedAt(), "")
	if len(snap.PendingTurns) != 0 || snap.Usage.TotalTokens != 12 || !snap.Usage.CostUSD.Equal(usage.CostUSD) {
		t.Fatal("committed pending costs counted twice")
	}
}
