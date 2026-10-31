package dtos

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysisvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stockvo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"go.mongodb.org/mongo-driver/bson"
)

func TestResearchRunBSONRoundTripAndLegacyCompatibility(t *testing.T) {
	code, _ := shared.NewStockCode("600519", shared.MarketCN)
	req, _ := analysisvo.NewRequest(code, shared.MustTradeDate("2024-03-01"), analysisvo.DepthStandard, nil, "")
	root := entities.NewAnalysisContext("research_dto", req)
	root.LoadMarketBrief(entities.MarketBrief{DataPreparations: []stockvo.DataPreparation{{Kind: stockvo.AnalysisNews, Provider: "eastmoney", Status: "failed", Message: "请求超时", DurationMS: 10000}}})
	if err := root.StartResearch("核查现金流反例", vo.DefaultResearchLimits()); err != nil {
		t.Fatal(err)
	}
	plan := vo.ResearchPlan{Questions: []vo.ResearchQuestion{{ID: "q1", Analyst: vo.KindFundamentalsAnalyst, Question: "现金流", Acceptance: []string{"检查反例"}}}}
	if err := root.PlanResearch(plan); err != nil {
		t.Fatal(err)
	}
	root.CommitTurn(vo.TurnRecord{Kind: vo.KindResearchManager, Purpose: "research:plan", Content: "plan"})
	dto := FromDomainAnalysisRun(root, time.Now(), "")
	raw, err := bson.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var restored AnalysisRunDto
	if err := bson.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	trace := restored.ToDomain()
	if !trace.Code.SameInstrument(code) || bson.Raw(raw).Lookup("symbol").StringValue() != "600519" {
		t.Fatal("run instrument lost by inline BSON mapping")
	}
	if len(trace.DataPreparations) != 1 || trace.DataPreparations[0].Provider != "eastmoney" || trace.DataPreparations[0].Message != "请求超时" {
		t.Fatal("lost provider preparation trace")
	}
	if trace.Research == nil || trace.Research.Question != "核查现金流反例" || trace.Research.Plan.Questions[0].ID != "q1" || trace.Turns[0].Purpose != "research:plan" {
		t.Fatalf("lost research state: %+v", trace)
	}
	restored.ResearchJSON = ""
	if restored.ToDomain().Research != nil {
		t.Fatal("legacy run treated as research")
	}
}
