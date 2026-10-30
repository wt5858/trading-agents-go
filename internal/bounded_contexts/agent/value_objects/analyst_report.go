package value_objects

import (
	"encoding/json"
	"fmt"
	"strings"
)

const MarketAgentVersion = "analyst-agent-v2"

type AnalystFinding struct {
	Claim       string   `json:"claim"`
	EvidenceIDs []string `json:"evidence_ids"`
}
type AnalystReport struct {
	Status    string           `json:"status"`
	Direction string           `json:"direction"`
	Findings  []AnalystFinding `json:"findings"`
	Risks     []string         `json:"risks"`
	Missing   []string         `json:"missing"`
}

// ParseAnalystReport checks the delivery contract against evidence actually returned by tools.
// It checks attribution, not the semantic truth of the model's claims.
func ParseAnalystReport(raw string, evidence map[string]bool) (AnalystReport, error) {
	var r AnalystReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, fmt.Errorf("analyst output JSON: %w", err)
	}
	if r.Status != "complete" && r.Status != "partial" && r.Status != "insufficient" {
		return r, fmt.Errorf("invalid analyst status")
	}
	if r.Direction != "bullish" && r.Direction != "bearish" && r.Direction != "neutral" {
		return r, fmt.Errorf("invalid analyst direction")
	}
	if r.Status == "insufficient" && (r.Direction != "neutral" || len(r.Missing) == 0 || len(r.Findings) > 0) {
		return r, fmt.Errorf("insufficient output must be neutral with missing data and no findings")
	}
	if r.Status != "insufficient" && len(r.Findings) == 0 {
		return r, fmt.Errorf("findings required")
	}
	if r.Status == "partial" && len(r.Missing) == 0 {
		return r, fmt.Errorf("partial output must declare missing data")
	}
	for _, f := range r.Findings {
		if strings.TrimSpace(f.Claim) == "" || len(f.EvidenceIDs) == 0 {
			return r, fmt.Errorf("finding requires claim and evidence")
		}
		for _, id := range f.EvidenceIDs {
			if !evidence[id] {
				return r, fmt.Errorf("unavailable evidence: %s", id)
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
