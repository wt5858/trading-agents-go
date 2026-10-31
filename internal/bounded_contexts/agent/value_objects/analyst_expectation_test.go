package value_objects

import "testing"

func TestAnnotationsDoNotConfuseContractAndQuality(t *testing.T) {
	var unlabelled *AnalystExpectation
	if unlabelled.Score(&AnalystReport{Status: "complete"}, "").Labelled {
		t.Fatal("invented ground truth")
	}
	e := &AnalystExpectation{Status: "insufficient", Direction: "neutral"}
	if e.Score(&AnalystReport{Status: "complete", Direction: "bullish"}, "").Passed {
		t.Fatal("contract compliance counted as quality")
	}
	if !e.Score(&AnalystReport{Status: "insufficient", Direction: "neutral"}, "").Passed {
		t.Fatal("correct abstention penalized")
	}
	if e.Score(nil, "cancelled").Passed {
		t.Fatal("failed execution passed")
	}
}
