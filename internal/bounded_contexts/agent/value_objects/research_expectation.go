package value_objects

import (
	"fmt"
	"strings"
)

// ResearchExpectation is an explicit, literal regression oracle, not an LLM judge.
type ResearchExpectation struct {
	Status          string   `json:"status,omitempty"`
	RequiredClaims  []string `json:"required_claims,omitempty"`
	ForbiddenClaims []string `json:"forbidden_claims,omitempty"`
}

func (e *ResearchExpectation) Validate() error {
	if e == nil {
		return nil
	}
	if e.Status != "" && e.Status != "complete" && e.Status != "partial" && e.Status != "insufficient" && e.Status != "skipped" {
		return fmt.Errorf("invalid expected research status")
	}
	if len(e.RequiredClaims)+len(e.ForbiddenClaims) > 40 {
		return fmt.Errorf("too many research expectations")
	}
	for _, list := range [][]string{e.RequiredClaims, e.ForbiddenClaims} {
		for _, v := range list {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("empty research expectation")
			}
		}
	}
	return nil
}
func (e *ResearchExpectation) Check(r *ResearchTrace) []string {
	if e == nil {
		return nil
	}
	if r == nil {
		return []string{"research result missing"}
	}
	var failures []string
	if e.Status != "" && e.Status != r.Status {
		failures = append(failures, "status differs from annotation")
	}
	// Claims are checked only in selected supported findings, not in the question or gaps.
	selected := map[string]bool{}
	for _, id := range r.Selected {
		selected[id] = true
	}
	var claims strings.Builder
	for _, f := range r.Findings {
		if selected[f.ID] {
			claims.WriteString(f.Finding.Claim)
			claims.WriteByte('\n')
		}
	}
	for _, v := range e.RequiredClaims {
		if !strings.Contains(claims.String(), v) {
			failures = append(failures, "required claim missing: "+v)
		}
	}
	for _, v := range e.ForbiddenClaims {
		if strings.Contains(claims.String(), v) {
			failures = append(failures, "forbidden claim present: "+v)
		}
	}
	return failures
}
