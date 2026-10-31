package domain_services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
)

func TestLongEvidenceRemainsRetrievableWithProvenance(t *testing.T) {
	turn := pilotTurn(t)
	turn.Contract = entities.NewNewsAnalyst().Contract()
	content := strings.Repeat("完整原文", 1000) + "末尾独有证据"
	turn.Snapshot.Market.News = []stock.News{{Code: turn.Snapshot.Code, Source: "issuer", URL: "https://example.test/filing", PublishedAt: time.Date(2024, 3, 1, 8, 0, 0, 0, time.UTC), Title: "公告", Content: content}}
	reg, _ := freezeMarket(turn)
	tool, _ := reg.Lookup(vo.ToolGetNews)
	var joined strings.Builder
	offset := 0
	for i := 0; i < 10; i++ {
		args, _ := json.Marshal(map[string]int{"offset": offset, "limit": 1})
		raw, err := tool.Invoke(context.Background(), ToolInvocation{Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var page frozenEvidence
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Documents) != 1 || page.Documents[0].URL != "https://example.test/filing" {
			t.Fatal("source identity lost")
		}
		joined.WriteString(page.Documents[0].Content)
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if !strings.Contains(joined.String(), "末尾独有证据") || joined.String() != "公告\n"+content {
		t.Fatal("long source was silently cut")
	}
}
func TestConflictingSourcesAreNotDeduplicatedAsOneReference(t *testing.T) {
	turn := pilotTurn(t)
	turn.Contract = entities.NewIndexAnalyst().Contract()
	for _, source := range []string{"source-a", "source-b"} {
		turn.Snapshot.Market.Contexts = append(turn.Snapshot.Market.Contexts, stock.MarketContext{Code: turn.Snapshot.Code, Scope: "index", Reference: "CSI300", AsOf: turn.Snapshot.TradeDate, AvailableOn: turn.Snapshot.TradeDate, Source: source, Data: "close differs by source"})
	}
	reg, _ := freezeMarket(turn)
	tool, _ := reg.Lookup(vo.ToolGetIndexContext)
	f := tool.(frozenTool)
	if len(f.documents) != 2 || f.documents[0].Source == f.documents[1].Source {
		t.Fatal("conflicting sources suppressed")
	}
}
func TestModelCheckpointPrecedesCallAndPreservesUsage(t *testing.T) {
	turn := pilotTurn(t)
	req, _ := analysis.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysis.DepthStandard, nil, "")
	root := entities.NewAnalysisContext("checkpoint-test", req)
	var snapshots []vo.RunTrace
	rt := NewRuntimeService(fixedRouter{&scriptedClient{replies: []vo.CompletionResult{{Content: "legacy report", Usage: vo.Usage{Calls: 1, TotalTokens: 10}}}}}, nil, nil, nil, RuntimeConfig{}, nil)
	wrapped := checkpointRuntime{delegate: rt, root: root, save: func(context.Context) error {
		snapshots = append(snapshots, root.TraceSnapshot(time.Now(), ""))
		return nil
	}}
	if err := entities.NewMarketAnalyst().Act(context.Background(), wrapped, root); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || len(snapshots[0].PendingTurns) != 1 || !snapshots[0].PendingTurns[0].ModelCalls[0].Pending || snapshots[1].Usage.TotalTokens != 10 || root.Usage().TotalTokens != 10 {
		t.Fatal("model call not checkpointed exactly once")
	}
}
func TestFactAndPerFindingSupportValidation(t *testing.T) {
	r := vo.AnalystReport{Findings: []vo.AnalystFinding{{Kind: "fact", Claim: "利润翻倍", EvidenceIDs: []string{"q"}, EvidenceQuotes: map[string]string{"q": "收盘100"}}}}
	if r.VerifySupport(map[string]string{"q": "收盘100"}) == nil {
		t.Fatal("unrelated fact passed")
	}
	r.Findings[0].Kind = "inference"
	if err := r.VerifySupport(map[string]string{"q": "收盘100"}); err != nil {
		t.Fatal(err)
	}
	r.Findings = append(r.Findings, vo.AnalystFinding{Claim: "无本职证据", EvidenceIDs: []string{"other"}})
	if r.RequirePrimaryEvidence(map[string]bool{"q": true}) == nil {
		t.Fatal("one primary citation licensed all findings")
	}
}
func TestEqualArmBudgetPreventsBaselineStarvation(t *testing.T) {
	ctx := WithCostBudget(context.Background(), decimal.RequireFromString("0.2"))
	allowance := armAllowance(ctx)
	client := estimatedScript{&scriptedClient{}}
	settle, _, err := reserveCost(armContext(ctx, allowance), client, vo.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	settle(&vo.CompletionResult{Usage: vo.Usage{CostStatus: "estimated", CostUSD: decimal.RequireFromString("0.1")}})
	if _, _, err := reserveCost(armContext(ctx, allowance), client, vo.CompletionRequest{}); err != nil {
		t.Fatal("baseline consumed agent allowance", err)
	}
}

func TestKlinePaginationKeepsWholeRowsAndAllSixtyDays(t *testing.T) {
	turn := pilotTurn(t)
	for i := 0; i < 60; i++ {
		turn.Snapshot.Market.Klines = append(turn.Snapshot.Market.Klines, stock.Kline{Code: turn.Snapshot.Code, Period: stock.PeriodDaily, TradeDate: turn.Snapshot.TradeDate.AddDays(-i), Close: decimal.NewFromInt(100)})
	}
	reg, _ := freezeMarket(turn)
	tool, _ := reg.Lookup(vo.ToolGetKlines)
	f := tool.(frozenTool)
	var rows int
	for _, doc := range f.documents {
		for _, line := range strings.Split(doc.Content, "\n") {
			if strings.HasPrefix(line, "2024-") {
				if len(strings.Split(line, "|")) != 7 {
					t.Fatal("partial Kline row")
				}
				rows++
			}
		}
	}
	if rows != 60 || len(f.documents) > 6 {
		t.Fatalf("unusable Kline pagination: rows=%d pages=%d", rows, len(f.documents))
	}
}

func TestPromptDigestIncludesActualToolContract(t *testing.T) {
	req := vo.ChatRequest{Messages: []vo.Message{vo.UserMessage("same input")}, Access: vo.NewDataAccess(vo.ToolGetQuote), Tools: []vo.ToolSpec{vo.NewToolSpec(vo.ToolGetQuote, "first description", vo.NewParamSchema(nil))}}
	_, a := digestRequest(req)
	req.Tools[0].Description = "changed meaning"
	_, b := digestRequest(req)
	if a == b {
		t.Fatal("tool semantics changed without invalidating cache/digest")
	}
}
