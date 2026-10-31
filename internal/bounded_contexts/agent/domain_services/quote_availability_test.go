package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

func TestFrozenQuoteUsesAvailableHistoricalDailyPricesWithExplicitCoverage(t *testing.T) {
	turn := testTurn(t, entities.NewMarketAnalyst())
	k := stock.Kline{Code: turn.Snapshot.Code, Period: stock.PeriodDaily, TradeDate: turn.Snapshot.TradeDate.AddDays(-1), Open: decimal.NewFromInt(100), High: decimal.NewFromInt(125), Low: decimal.NewFromInt(99), Close: decimal.RequireFromString("123.456789"), Volume: decimal.RequireFromString("1000.12345"), Amount: decimal.RequireFromString("123000.12345"), Adjusted: true, Source: "stored-source"}
	turn.Snapshot.Market.Klines = []stock.Kline{k}
	reg, _ := freezeMarket(turn)
	tool, _ := reg.Lookup(vo.ToolGetQuote)
	raw, err := tool.Invoke(context.Background(), ToolInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	var page frozenEvidence
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if page.Missing || page.Total != 1 || page.DataKind != "daily_kline" || len(page.MissingFields) == 0 || page.Source != "stored-source" || page.Documents[0].AvailableOn != k.TradeDate.String() {
		t.Fatalf("price fallback/source missing: %s", raw)
	}
	for _, text := range []string{"历史日线摘要", "复权: true", "123.456789", "1000.12345", "123000.12345", "未计算或补零"} {
		if !strings.Contains(page.Data, text) {
			t.Fatalf("lost stored value or limitation %q: %s", text, page.Data)
		}
	}
	if strings.Contains(page.Data, "涨跌: 0") || strings.Contains(page.Data, "PE: 0") {
		t.Fatal("absent quote fields fabricated")
	}
	turn.Snapshot.Market.Klines[0].TradeDate = turn.Snapshot.TradeDate.AddDays(1)
	reg, _ = freezeMarket(turn)
	if !reg.tools[0].evidence.Missing {
		t.Fatal("future daily bar leaked through quote fallback")
	}
}

func TestFrozenMissingQuoteIsRetiredAndRepeatedRequestReusesResult(t *testing.T) {
	root := researchRoot(t)
	snapshot := root.Snapshot()
	snapshot.Market.Quote = stock.Quote{}
	turn := entities.Turn{Contract: entities.NewNewsAnalyst().Contract(), Snapshot: snapshot}
	reg, _ := freezeMarket(turn)
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "first", Name: vo.ToolGetQuote}}}, {ToolCalls: []vo.ToolCall{{ID: "retry", Name: vo.ToolGetQuote, Arguments: json.RawMessage(`{"offset":1}`)}}}, {Content: "done"}}}
	rt := NewRuntimeService(fixedRouter{client}, reg, nil, nil, RuntimeConfig{}, nil)
	resp, err := rt.chat(context.Background(), client, "test-model", vo.ChatRequest{Messages: []vo.Message{vo.UserMessage("read")}, Tools: reg.Specs(turn.Contract.Access), Access: turn.Contract.Access, MaxToolRounds: 5}, ToolInvocation{})
	if err != nil || client.rounds() != 3 || len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Reused || !resp.ToolCalls[1].Reused || resp.Content != "done" {
		t.Fatalf("missing tool kept executing: %+v %v", resp, err)
	}
	for _, spec := range client.round(1).Tools {
		if spec.Name == vo.ToolGetQuote {
			t.Fatal("missing quote still advertised")
		}
	}
	if len(client.round(1).Tools) == 0 {
		t.Fatal("available news tool incorrectly retired")
	}
	if len(client.round(2).Tools) != 0 {
		t.Fatal("duplicate-only tool round did not force conclusion")
	}
	if !toChainToolCalls(resp.ToolCalls)[1].Reused {
		t.Fatal("reuse trace lost in decision chain")
	}
}

func TestFrozenAllMissingToolsConcludeOnNextModelRequest(t *testing.T) {
	turn := testTurn(t, entities.NewMarketAnalyst())
	reg, _ := freezeMarket(turn)
	access := vo.NewDataAccess(vo.ToolGetQuote)
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "first", Name: vo.ToolGetQuote}}}, {Content: "insufficient"}}}
	rt := NewRuntimeService(fixedRouter{client}, reg, nil, nil, RuntimeConfig{}, nil)
	resp, err := rt.chat(context.Background(), client, "test-model", vo.ChatRequest{Messages: []vo.Message{vo.UserMessage("read")}, Tools: reg.Specs(access), Access: access, MaxToolRounds: 5}, ToolInvocation{})
	if err != nil || client.rounds() != 2 || len(client.round(1).Tools) != 0 || resp.Truncated {
		t.Fatalf("terminal evidence incorrectly retried/truncated: %+v %v", resp, err)
	}
}

func TestFrozenSearchMissDoesNotRetireOtherQueries(t *testing.T) {
	root := researchRoot(t)
	turn := entities.Turn{Contract: entities.NewNewsAnalyst().Contract(), Snapshot: root.Snapshot(), Research: &vo.ResearchTask{}}
	reg, _ := freezeMarket(turn)
	access := vo.NewDataAccess(vo.ToolSearchEvidence)
	client := &scriptedClient{replies: []vo.CompletionResult{{ToolCalls: []vo.ToolCall{{ID: "miss", Name: vo.ToolSearchEvidence, Arguments: json.RawMessage(`{"query":"no-match-at-all"}`)}}}, {ToolCalls: []vo.ToolCall{{ID: "hit", Name: vo.ToolSearchEvidence, Arguments: json.RawMessage(`{"query":"counterexample"}`)}}}, {Content: "done"}}}
	rt := NewRuntimeService(fixedRouter{client}, reg, nil, nil, RuntimeConfig{}, nil)
	resp, err := rt.chat(context.Background(), client, "test-model", vo.ChatRequest{Messages: []vo.Message{vo.UserMessage("read")}, Tools: reg.Specs(access), Access: access, MaxToolRounds: 5}, ToolInvocation{})
	if err != nil || len(client.round(1).Tools) != 1 || len(resp.ToolCalls) != 2 || resp.ToolCalls[1].Reused {
		t.Fatalf("query-specific miss retired search: %+v %v", resp, err)
	}
	var last frozenEvidence
	for _, m := range resp.Messages {
		if m.Role == vo.RoleTool {
			_ = json.Unmarshal([]byte(m.Content), &last)
		}
	}
	if last.Missing || last.Total == 0 {
		t.Fatal("changed query could not find available evidence")
	}
}

type failedQuoteReader struct {
	*asOfSpy
	quoteErr error
}

func (r failedQuoteReader) QuoteAsOf(context.Context, shared.StockCode, shared.TradeDate) (*stock.Quote, error) {
	return nil, r.quoteErr
}

func TestQuotePreparationRecordsWhyItIsMissing(t *testing.T) {
	turn := pilotTurn(t)
	req, _ := analysis.NewRequest(turn.Snapshot.Code, turn.Snapshot.TradeDate, analysis.DepthStandard, nil, "")
	for _, tc := range []struct {
		err  error
		want string
	}{{custom_errors.NotFound("no quote"), "未查到"}, {errors.New("private database details"), "读取失败"}, {nil, "空记录"}} {
		reader := failedQuoteReader{newAsOfSpy(stock.Quote{}), tc.err}
		engine := NewEngineService(nil, reader, nil, nil, nil, nil, nil, nil, EngineConfig{})
		brief, _ := engine.collect(context.Background(), req)
		if !strings.Contains(brief.QuoteMissingReason, tc.want) || strings.Contains(brief.QuoteMissingReason, "private") {
			t.Fatalf("missing reason not recorded safely: %q", brief.QuoteMissingReason)
		}
		found := false
		for _, missing := range brief.Missing {
			found = found || missing == "行情快照"
		}
		if !found {
			t.Fatal("nil quote was not recorded as missing")
		}
	}
}
