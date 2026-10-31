package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func attemptRoot(t *testing.T) *entities.AnalysisContext {
	t.Helper()
	code, _ := shared.NewStockCode("600519", shared.MarketCN)
	req, err := analysis.NewRequest(code, shared.MustTradeDate("2024-03-01"), analysis.DepthStandard, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return entities.NewAnalysisContext("same-task", req)
}
func TestCheckpointsAndRetriesWriteDistinctExecutionDocuments(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("attempts", func(mt *mtest.T) {
		repo := NewAnalysisRunRepository(mt.DB)
		a, b := attemptRoot(mt.T), attemptRoot(mt.T)
		mt.AddMockResponses(mtest.CreateSuccessResponse(), mtest.CreateSuccessResponse(), mtest.CreateSuccessResponse())
		if err := repo.SaveCheckpoint(context.Background(), a); err != nil {
			mt.Fatal(err)
		}
		command := mt.GetStartedEvent().Command
		updates, _ := command.Lookup("updates").Array().Values()
		first := updates[0].Document()
		if first.Lookup("q").Document().Lookup("_id").StringValue() != a.ExecutionID() || !first.Lookup("u").Document().Lookup("in_progress").Boolean() {
			mt.Fatal("checkpoint not stored under execution id")
		}
		a.CommitTurn(vo.TurnRecord{Kind: vo.KindMarketAnalyst, Content: "completed"})
		if err := repo.Save(context.Background(), a, time.Now(), ""); err != nil {
			mt.Fatal(err)
		}
		command = mt.GetStartedEvent().Command
		updates, _ = command.Lookup("updates").Array().Values()
		if updates[0].Document().Lookup("q").Document().Lookup("_id").StringValue() != a.ExecutionID() {
			mt.Fatal("final save did not replace same checkpoint")
		}
		if err := repo.Save(context.Background(), b, time.Now(), "failed retry"); err != nil {
			mt.Fatal(err)
		}
		command = mt.GetStartedEvent().Command
		updates, _ = command.Lookup("updates").Array().Values()
		root := updates[0].Document().Lookup("u").Document()
		if root.Lookup("_id").StringValue() == a.ExecutionID() || root.Lookup("logical_run_id").StringValue() != a.RunID() {
			mt.Fatal("retry overwrote earlier attempt")
		}
	})
}
func TestLogicalLookupAndResumeRemainBackwardCompatible(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("logical ids", func(mt *mtest.T) {
		repo := NewAnalysisRunRepository(mt.DB)
		ns := mt.DB.Name() + "." + collAnalysisRuns
		mt.AddMockResponses(mtest.CreateCursorResponse(0, ns, mtest.FirstBatch, bson.D{{Key: "_id", Value: "execution-a"}, {Key: "logical_run_id", Value: "same-task"}}))
		ids, err := repo.ExistingRunIDs(context.Background(), []string{"same-task"})
		if err != nil {
			mt.Fatal(err)
		}
		if _, ok := ids["same-task"]; !ok {
			mt.Fatal("resume returned execution id instead of task id")
		}
		mt.AddMockResponses(mtest.CreateCursorResponse(0, ns, mtest.FirstBatch, bson.D{{Key: "_id", Value: "execution-a"}, {Key: "logical_run_id", Value: "same-task"}, {Key: "trade_date", Value: "2024-03-01"}}))
		trace, err := repo.FindByRunID(context.Background(), "same-task")
		if err != nil {
			mt.Fatal(err)
		}
		if trace.RunID != "same-task" || trace.ExecutionID != "execution-a" {
			mt.Fatal("logical/physical identities confused")
		}
		mt.AddMockResponses(mtest.CreateCursorResponse(0, ns, mtest.FirstBatch, bson.D{{Key: "_id", Value: "legacy-task"}, {Key: "trade_date", Value: "2024-03-01"}}))
		trace, err = repo.FindByRunID(context.Background(), "legacy-task")
		if err != nil || trace.RunID != "legacy-task" {
			mt.Fatal("old trace unreadable", err)
		}
	})
}

func TestSummaryDeduplicatesAttemptsAfterProjectingReportBodies(t *testing.T) {
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	mt.Run("summary projection", func(mt *mtest.T) {
		repo := NewAnalysisRunRepository(mt.DB)
		mt.AddMockResponses(mtest.CreateCursorResponse(0, mt.DB.Name()+"."+collAnalysisRuns, mtest.FirstBatch, bson.D{{Key: "_id", Value: "execution-a"}, {Key: "logical_run_id", Value: "same-task"}, {Key: "trade_date", Value: "2024-03-01"}}))
		rng, err := shared.NewDateRange("2024-03-01", "2024-03-02")
		if err != nil {
			mt.Fatal(err)
		}
		rows, truncated, err := repo.ListSummaries(context.Background(), rng, 10)
		if err != nil || truncated || len(rows) != 1 || rows[0].RunID != "same-task" {
			mt.Fatal("summary identity changed", err)
		}
		stages, err := mt.GetStartedEvent().Command.Lookup("pipeline").Array().Values()
		if err != nil {
			mt.Fatal(err)
		}
		if stages[0].Document().Lookup("$match").Document().Lookup("research_json").Document().Lookup("$exists").Boolean() {
			mt.Fatal("research reports entered trading evaluation")
		}
		projected := false
		grouped := false
		for _, stage := range stages {
			doc := stage.Document()
			if doc.Lookup("$project").Type != 0 {
				projected = true
			}
			if doc.Lookup("$group").Type != 0 {
				if !projected {
					mt.Fatal("report bodies entered grouping/sort")
				}
				grouped = true
			}
		}
		if !grouped {
			mt.Fatal("attempts were not deduplicated")
		}
	})
}
