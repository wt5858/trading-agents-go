package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	svc "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

func TestResearchEvaluationCLISeparatesContractAndLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "research.json")
	cmd := newResearchCommand(true)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/research-v1.json", "--mock", "--output", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result svc.ResearchEvaluation
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Passed != 3 || result.Failed != 0 || result.Labelled != 3 || result.QualityPassed != 3 || result.UnknownCostCalls != 0 {
		t.Fatalf("bad research evaluation: %+v", result)
	}
	for _, sample := range result.Samples {
		if sample.Trace.Research == nil || len(sample.Trace.Turns) != 4 || sample.Trace.Research.Report == "" {
			t.Fatal("lost research/cost trace")
		}
	}
	cmd = newResearchCommand(true)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/research-v1.json", "--mock", "--output", path})
	if cmd.Execute() == nil {
		t.Fatal("overwrote existing results")
	}
}
func TestResearchRunCLIPreservesBudgetInterruptedResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.json")
	cmd := newResearchCommand(false)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/research-single.json", "--mock", "--max-requests", "3", "--output", path})
	if cmd.Execute() == nil {
		t.Fatal("request exhaustion reported as success")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace vo.RunTrace
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Research == nil || trace.Research.StopReason != "budget_exhausted" || len(trace.Research.Findings) == 0 || trace.Research.Report == "" || !trace.Failed {
		t.Fatalf("lost budget partial: %+v", trace)
	}
}
func TestResearchCLIDryRunAndAnnotatedFailure(t *testing.T) {
	cmd := newResearchCommand(false)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--input", "../docs/agent/fixtures/research-single.json", "--mock", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../docs/agent/fixtures/research-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []svc.ResearchCase
	_ = json.Unmarshal(raw, &cases)
	cases = cases[:1]
	cases[0].Expected = &vo.ResearchExpectation{Status: "insufficient"}
	raw, _ = json.Marshal(cases)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "output.json")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd = newResearchCommand(true)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--input", input, "--mock", "--output", output})
	if cmd.Execute() == nil {
		t.Fatal("annotation mismatch exited successfully")
	}
	raw, _ = os.ReadFile(output)
	var result svc.ResearchEvaluation
	_ = json.Unmarshal(raw, &result)
	if result.Passed != 1 || result.QualityPassed != 0 || result.Labelled != 1 {
		t.Fatalf("conflated metrics: %+v", result)
	}
}
func TestResearchInputRejectsTrailingJSONAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{"unexpected":true}`} {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		var cases []svc.ResearchCase
		if readResearchInput(path, false, &cases) == nil {
			t.Fatal("invalid research input accepted")
		}
	}
}
