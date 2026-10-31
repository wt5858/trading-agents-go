package db

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/config"
	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	agentrepo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stockrepo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/llm"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Explicit opt-in: reads archived data, never writes business collections. Real
// model calls share a $0.03 estimated admission budget and a ten-request cap.
func TestLiveAnalystDeliveryRegression(t *testing.T) {
	if os.Getenv("TA_RUN_LIVE_DELIVERY_REGRESSION") != "1" {
		t.Skip("opt-in real model/database regression")
	}
	output := "../../docs/agent/delivery-live-regression-v4.json"
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("reserve fresh report output:", err)
	}
	defer file.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal("configuration load failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.Mongo.URI).SetRegistry(newDecimalRegistry()).SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		t.Fatal("mongo unavailable")
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	database := client.Database(cfg.Mongo.Database)
	market := stockrepo.NewMarketDataRepository(database)
	code, _ := shared.NewStockCode("000547.SZ", shared.MarketCN)
	date := shared.MustTradeDate("2026-10-31")
	brief := entities.MarketBrief{}
	quote, err := market.QuoteAsOf(ctx, code, date)
	if err != nil {
		t.Fatal("archive quote unavailable")
	}
	brief.Quote = *quote
	brief.Klines, err = market.Klines(ctx, code, stock.PeriodDaily, shared.DateRange{Start: date.AddDays(-250), End: date}, 60)
	if err != nil {
		t.Fatal("archive Klines unavailable")
	}
	brief.News, err = market.News(ctx, code, shared.DateRange{Start: date.AddDays(-30), End: date}, 15)
	if err != nil || len(brief.News) == 0 {
		t.Fatal("archive news unavailable")
	}
	brief.Financials, err = market.Financials(ctx, code, date, 8)
	if err != nil {
		t.Fatal("archive financial read failed")
	}
	if indicator, err := agentrepo.NewIndicatorRepository(database).Find(ctx, code, stock.PeriodDaily, date); err == nil {
		brief.Indicators = indicator.Indicators
	}
	if trace, err := agentrepo.NewAnalysisRunRepository(database).FindByRunID(ctx, "01a190f16ca0780c8217f4a8d5f73571"); err == nil {
		brief.DataPreparations = trace.DataPreparations
	}
	router := llm.NewRouter("deepseek-chat")
	registered := false
	for _, p := range cfg.LLM.EnabledProviders() {
		if strings.Contains(strings.ToLower(p.Name), "deepseek") {
			router.Register(&llm.CappedClient{Client: llm.NewOpenAICompat(p.Name, p.BaseURL, p.APIKey, &http.Client{Timeout: 60 * time.Second}), Limit: 10}, "deepseek-chat")
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("configured DeepSeek provider unavailable")
	}
	var cases []svc.AnalystCase
	for _, kind := range []vo.AgentKind{vo.KindMarketAnalyst, vo.KindFundamentalsAnalyst, vo.KindNewsAnalyst} {
		cases = append(cases, svc.AnalystCase{ID: "archived-000547-" + kind.String(), Code: code.Symbol, Market: code.Market, TradeDate: date, Analyst: kind, Model: "deepseek-chat", Snapshot: brief})
	}
	runtime := svc.NewRuntimeService(router, nil, nil, nil, svc.RuntimeConfig{AnalystAgentsEnabled: true, AgentTimeout: 90 * time.Second}, nil)
	report, runErr := runtime.EvaluateAnalysts(svc.WithCostBudget(ctx, decimal.RequireFromString("0.03")), cases, 1, false)
	if err := json.NewEncoder(file).Encode(report); err != nil {
		t.Fatal(err)
	}
	t.Logf("contractPassed=%d failed=%d tokens=%d storedCostUSD=%s", report.Passed, report.Failed, report.TotalTokens, report.CostUSD)
	for _, sample := range report.Samples {
		status := ""
		if sample.Agent.AnalystReport != nil {
			status = sample.Agent.AnalystReport.Status
		}
		t.Logf("analyst=%s status=%s modelCalls=%d error=%s", sample.Analyst, status, len(sample.Agent.ModelCalls), sample.Error)
	}
	if runErr != nil || report.Failed != 0 || report.Passed != 3 {
		t.Fatalf("real delivery regression failed: %v", runErr)
	}
}
