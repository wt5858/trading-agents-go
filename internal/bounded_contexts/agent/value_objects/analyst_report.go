package value_objects

import (
	"encoding/json"
	"fmt"
	"strings"
)

const MarketAgentVersion = "analyst-agent-v4"

type AnalystFinding struct {
	Kind           string            `json:"kind,omitempty"`
	EvidenceQuotes map[string]string `json:"evidence_quotes,omitempty"`
	Claim          string            `json:"claim"`
	EvidenceIDs    []string          `json:"evidence_ids"`
}
type AnalystReport struct {
	SupportVerified bool             `json:"support_verified,omitempty"`
	Status          string           `json:"status"`
	Direction       string           `json:"direction"`
	Findings        []AnalystFinding `json:"findings"`
	Risks           []string         `json:"risks"`
	Missing         []string         `json:"missing"`
}

// ParseAnalystReport checks the delivery contract against evidence actually returned by tools.
// It checks attribution, not the semantic truth of the model's claims.
func ParseAnalystReport(raw string, evidence map[string]bool) (AnalystReport, error) {
	var r AnalystReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, validationError("invalid_json", -1, "analyst output JSON: "+err.Error(), err)
	}
	if r.Status != "complete" && r.Status != "partial" && r.Status != "insufficient" {
		return r, validationError("invalid_contract", -1, "invalid analyst status", nil)
	}
	if r.Direction != "bullish" && r.Direction != "bearish" && r.Direction != "neutral" {
		return r, validationError("invalid_contract", -1, "invalid analyst direction", nil)
	}
	if r.Status == "insufficient" && (r.Direction != "neutral" || len(r.Missing) == 0 || len(r.Findings) > 0) {
		return r, validationError("invalid_contract", -1, "insufficient output must be neutral with missing data and no findings", nil)
	}
	if r.Status != "insufficient" && len(r.Findings) == 0 {
		return r, validationError("invalid_contract", -1, "findings required", nil)
	}
	if r.Status == "partial" && len(r.Missing) == 0 {
		return r, validationError("invalid_contract", -1, "partial output must declare missing data", nil)
	}
	for i, f := range r.Findings {
		if strings.TrimSpace(f.Claim) == "" || len(f.EvidenceIDs) == 0 {
			return r, validationError("invalid_contract", i, "finding requires claim and evidence", nil)
		}
		for _, id := range f.EvidenceIDs {
			if !evidence[id] {
				return r, validationError("unavailable_evidence", i, fmt.Sprintf("unavailable evidence: %s", id), nil)
			}
		}
	}
	return r, nil
}
func (r AnalystReport) Markdown() string {
	var s strings.Builder
	fmt.Fprintf(&s, "### 分析结论\n%s（%s）\n", map[string]string{"bullish": "看多", "bearish": "看空", "neutral": "中性"}[r.Direction], map[string]string{"complete": "完整", "partial": "部分完成", "insufficient": "证据不足"}[r.Status])
	for _, f := range r.Findings {
		fmt.Fprintf(&s, "- %s [%s]\n", f.Claim, strings.Join(f.EvidenceIDs, ", "))
	}
	if len(r.Risks) > 0 {
		fmt.Fprintf(&s, "\n### 风险\n%s\n", strings.Join(r.Risks, "；"))
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(&s, "\n### 缺失数据\n%s\n", strings.Join(r.Missing, "；"))
	}
	return s.String()
}

// Clone keeps structured reports immutable across concurrent snapshots.
func (r AnalystReport) Clone() AnalystReport {
	out := r
	out.Risks = append([]string(nil), r.Risks...)
	out.Missing = append([]string(nil), r.Missing...)
	out.Findings = append([]AnalystFinding(nil), r.Findings...)
	for i, f := range out.Findings {
		out.Findings[i].EvidenceIDs = append([]string(nil), f.EvidenceIDs...)
		if f.EvidenceQuotes != nil {
			out.Findings[i].EvidenceQuotes = map[string]string{}
			for k, v := range f.EvidenceQuotes {
				out.Findings[i].EvidenceQuotes[k] = v
			}
		}
	}
	return out
}

// VerifySupport checks exact source quotations, not the truth of arbitrary inference.
func (r AnalystReport) VerifySupport(data map[string]string) error {
	for i, f := range r.Findings {
		if f.Kind != "fact" && f.Kind != "inference" && f.Kind != "hypothesis" {
			return validationError("invalid_contract", i, "invalid finding kind", nil)
		}
		factMatch := false
		for _, id := range f.EvidenceIDs {
			quote := strings.TrimSpace(f.EvidenceQuotes[id])
			text, ok := data[id]
			if strings.Contains(quote, strings.TrimSpace(f.Claim)) {
				factMatch = true
			}
			if !ok || quote == "" || !strings.Contains(text, quote) {
				return validationError("quote_mismatch", i, fmt.Sprintf("finding lacks a matching source quote for %s", id), nil)
			}
		}
		if f.Kind == "fact" && !factMatch {
			return validationError("fact_not_extract", i, "fact claim must be an exact extract; classify reasoning as inference", nil)
		}
	}
	return nil
}

func (r AnalystReport) DecisionEligible() bool {
	if r.Status != "complete" || !r.SupportVerified {
		return false
	}
	for _, f := range r.Findings {
		if f.Kind == "fact" || f.Kind == "inference" {
			return true
		}
	}
	return false
}
