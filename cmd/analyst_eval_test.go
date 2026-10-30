package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
)

func TestAnalystEvaluationCLIRecordsAllSixRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaluation.json")
	cmd := newAnalystEvalCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/synthetic-six-analysts.json", "--mock", "--compare", "--output", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result svc.AnalystEvaluation
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Passed != 6 || result.Complete != 6 || result.Failed != 0 || len(result.Samples) != 6 || result.UnknownCostCalls != 0 {
		t.Fatalf("unexpected evaluation: %+v", result)
	}
	for _, sample := range result.Samples {
		if sample.Baseline == nil || sample.Agent.AgentArtifact == "" {
			t.Fatal("pair artifact missing")
		}
	}
	cmd = newAnalystEvalCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/synthetic-six-analysts.json", "--mock", "--output", path})
	if err := cmd.Execute(); err == nil {
		t.Fatal("existing report overwritten")
	}
}
