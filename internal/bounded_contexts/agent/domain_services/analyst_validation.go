package domain_services

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

func validateAnalystOutput(turn entities.Turn, reg frozenRegistry, raw string, pages []frozenEvidence) (vo.AnalystReport, error) {
	available := map[string]bool{}
	data := map[string]string{}
	primary := map[string]bool{}
	for _, e := range pages {
		if e.Missing {
			continue
		}
		available[e.ID] = true
		data[e.ID] = e.Data
		for _, d := range e.Documents {
			available[d.ID] = true
			data[d.ID] = d.Content
		}
		for _, t := range reg.tools {
			if !vo.IsPrimaryAnalystTool(turn.Contract.Kind, t.spec.Name) {
				continue
			}
			for _, own := range t.documents {
				for _, shown := range e.Documents {
					if own.ID == shown.ID {
						primary[shown.ID] = true
						primary[e.ID] = true
					}
				}
			}
		}
	}
	report, err := vo.ParseAnalystReport(raw, available)
	if err != nil {
		return report, err
	}
	report.SupportVerified = false
	if report.Status != "insufficient" {
		if err := report.RequirePrimaryEvidence(primary); err != nil {
			return report, err
		}
	}
	if err := report.VerifySupport(data); err != nil {
		return report, err
	}
	report.SupportVerified = true
	return report, nil
}
func allFrozenPages(reg frozenRegistry) []frozenEvidence {
	var out []frozenEvidence
	for _, t := range reg.tools {
		if len(t.documents) == 0 {
			out = append(out, t.evidence)
			continue
		}
		for i := range t.documents {
			p, _ := t.page(i, 1, "")
			out = append(out, p)
		}
	}
	return out
}
func makeAnalystArtifact(kind vo.AgentKind, id, raw string, pages []frozenEvidence, valid bool) string {
	b, _ := json.Marshal(struct {
		Kind       vo.AgentKind     `json:"kind"`
		Version    string           `json:"version"`
		SnapshotID string           `json:"snapshot_id"`
		Evidence   []frozenEvidence `json:"evidence"`
		Output     string           `json:"output"`
		Valid      bool             `json:"valid"`
	}{kind, vo.MarketAgentVersion, id, pages, raw, valid})
	return string(b)
}
