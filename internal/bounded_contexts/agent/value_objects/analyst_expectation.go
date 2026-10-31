package value_objects

import (
	"fmt"
	"strings"
)

type AnalystExpectation struct {
	Status          string   `json:"status,omitempty"`
	Direction       string   `json:"direction,omitempty"`
	RequiredClaims  []string `json:"required_claims,omitempty"`
	ForbiddenClaims []string `json:"forbidden_claims,omitempty"`
}
type AnalystScore struct {
	Labelled bool     `json:"labelled"`
	Passed   bool     `json:"passed"`
	Reasons  []string `json:"reasons,omitempty"`
}

func (e *AnalystExpectation) Score(report *AnalystReport, executionError string) AnalystScore {
	score := AnalystScore{Labelled: e != nil}
	if e == nil {
		return score
	}
	if report == nil || executionError != "" {
		score.Reasons = []string{"execution or evidence validation failed"}
		return score
	}
	if e.Status != "" && report.Status != e.Status {
		score.Reasons = append(score.Reasons, "unexpected status")
	}
	if e.Direction != "" && report.Direction != e.Direction {
		score.Reasons = append(score.Reasons, "unexpected direction")
	}
	var claims strings.Builder
	for _, f := range report.Findings {
		claims.WriteString(f.Claim)
		claims.WriteByte('\n')
	}
	text := strings.ToLower(claims.String())
	for _, v := range e.RequiredClaims {
		if !strings.Contains(text, strings.ToLower(v)) {
			score.Reasons = append(score.Reasons, "required annotated claim missing: "+v)
		}
	}
	for _, v := range e.ForbiddenClaims {
		if strings.Contains(text, strings.ToLower(v)) {
			score.Reasons = append(score.Reasons, "forbidden annotated claim present: "+v)
		}
	}
	score.Passed = len(score.Reasons) == 0
	return score
}

func (e *AnalystExpectation) Validate() error {
	if e == nil {
		return nil
	}
	if e.Status == "" && e.Direction == "" && len(e.RequiredClaims) == 0 && len(e.ForbiddenClaims) == 0 {
		return fmt.Errorf("expectation must contain a scoring criterion")
	}
	if e.Status != "" && e.Status != "complete" && e.Status != "partial" && e.Status != "insufficient" && e.Status != "skipped" {
		return fmt.Errorf("invalid expected status")
	}
	if e.Direction != "" && e.Direction != "neutral" && e.Direction != "bullish" && e.Direction != "bearish" {
		return fmt.Errorf("invalid expected direction")
	}
	for _, v := range append(append([]string(nil), e.RequiredClaims...), e.ForbiddenClaims...) {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("empty annotated claim")
		}
	}
	return nil
}

func (e *AnalystExpectation) ScoreSkipped(executionError string) AnalystScore {
	score := AnalystScore{Labelled: e != nil}
	if e == nil {
		return score
	}
	if executionError != "" {
		score.Reasons = append(score.Reasons, "execution failed")
	}
	if e.Status != "" && e.Status != "skipped" {
		score.Reasons = append(score.Reasons, "unexpected status")
	}
	if e.Direction != "" {
		score.Reasons = append(score.Reasons, "skipped analysis has no direction")
	}
	if len(e.RequiredClaims) > 0 {
		score.Reasons = append(score.Reasons, "skipped analysis has no claims")
	}
	score.Passed = len(score.Reasons) == 0
	return score
}
