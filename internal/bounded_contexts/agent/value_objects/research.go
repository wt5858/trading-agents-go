package value_objects

import (
	"encoding/json"
	"fmt"
	"strings"
)

const ResearchVersion = "research-v1"

func ValidateResearchQuestion(question string) error {
	if strings.TrimSpace(question) == "" || len([]rune(question)) > 2000 {
		return fmt.Errorf("research question must contain 1..2000 characters")
	}
	return nil
}

// ResearchLimits bounds the entire workflow, including planning and review.
type ResearchLimits struct {
	MaxRequests      int `json:"max_requests"`
	MaxTokens        int `json:"max_tokens"`
	SupplementRounds int `json:"supplement_rounds"`
	TimeoutSeconds   int `json:"timeout_seconds"`
}

func DefaultResearchLimits() ResearchLimits { return ResearchLimits{24, 50000, 2, 300} }
func (l ResearchLimits) Validate() error {
	if l.MaxRequests < 3 || l.MaxRequests > 64 || l.MaxTokens < 1000 || l.MaxTokens > 200000 || l.SupplementRounds < 0 || l.SupplementRounds > 2 || l.TimeoutSeconds < 1 || l.TimeoutSeconds > 900 {
		return fmt.Errorf("invalid research limits: requests 3..64, tokens 1000..200000, supplements 0..2, timeout 1..900 seconds")
	}
	return nil
}

type ResearchQuestion struct {
	ID         string    `json:"id"`
	Analyst    AgentKind `json:"analyst"`
	Question   string    `json:"question"`
	Acceptance []string  `json:"acceptance"`
}
type ResearchPlan struct {
	Questions []ResearchQuestion `json:"questions"`
}

func (p ResearchPlan) Validate() error {
	if len(p.Questions) < 1 || len(p.Questions) > 4 {
		return fmt.Errorf("research requires 1..4 questions")
	}
	ids, roles := map[string]bool{}, map[AgentKind]bool{}
	for _, q := range p.Questions {
		if q.ID == "" || len(q.ID) > 40 || ids[q.ID] || !q.Analyst.IsAnalyst() || roles[q.Analyst] || strings.TrimSpace(q.Question) == "" || len([]rune(q.Question)) > 1000 || len(q.Acceptance) < 1 || len(q.Acceptance) > 5 {
			return fmt.Errorf("invalid or duplicate research question/analyst: %s", q.ID)
		}
		for _, s := range q.Acceptance {
			if strings.TrimSpace(s) == "" || len([]rune(s)) > 500 {
				return fmt.Errorf("invalid acceptance criterion")
			}
		}
		ids[q.ID], roles[q.Analyst] = true, true
	}
	return nil
}

// ResearchTask is a question-specific, read-only instruction for an existing analyst.
type ResearchTask struct {
	Question ResearchQuestion `json:"question"`
	Round    int              `json:"round"`
	Focus    string           `json:"focus,omitempty"`
	Prior    string           `json:"prior,omitempty"`
}
type ResearchGap struct {
	QuestionID  string `json:"question_id"`
	Reason      string `json:"reason"`
	Recoverable bool   `json:"recoverable"`
	Query       string `json:"query,omitempty"`
}
type ResearchReview struct {
	Gaps []ResearchGap `json:"gaps"`
}

func (r ResearchReview) Validate(p ResearchPlan) error {
	if len(r.Gaps) > 8 {
		return fmt.Errorf("too many research gaps")
	}
	ids := map[string]bool{}
	for _, q := range p.Questions {
		ids[q.ID] = true
	}
	for _, g := range r.Gaps {
		if !ids[g.QuestionID] || strings.TrimSpace(g.Reason) == "" || len([]rune(g.Reason)) > 1000 || len([]rune(g.Query)) > 500 || (g.Recoverable && strings.TrimSpace(g.Query) == "") {
			return fmt.Errorf("invalid research gap for %s", g.QuestionID)
		}
	}
	return nil
}

type ResearchAttempt struct {
	Skipped      bool     `json:"skipped,omitempty"`
	SkipReason   string   `json:"skip_reason,omitempty"`
	Status       string   `json:"status"`
	Missing      []string `json:"missing,omitempty"`
	Risks        []string `json:"risks,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
	QuestionID   string   `json:"question_id"`
	Round        int      `json:"round"`
	TurnSeq      int      `json:"turn_seq"`
	NewDocuments int      `json:"new_documents"`
	Error        string   `json:"error,omitempty"`
}
type ResearchFinding struct {
	ID         string         `json:"id"`
	QuestionID string         `json:"question_id"`
	Round      int            `json:"round"`
	Finding    AnalystFinding `json:"finding"`
}
type ResearchSelection struct {
	FindingIDs []string `json:"finding_ids"`
}

// ResearchTrace is owned by AnalysisContext and stored in its run document.
type ResearchTrace struct {
	Symbol     string            `json:"symbol"`
	AsOf       string            `json:"as_of"`
	Version    string            `json:"version"`
	Question   string            `json:"question"`
	Limits     ResearchLimits    `json:"limits"`
	Plan       ResearchPlan      `json:"plan"`
	Review     *ResearchReview   `json:"review,omitempty"`
	Attempts   []ResearchAttempt `json:"attempts"`
	Findings   []ResearchFinding `json:"findings"`
	Selected   []string          `json:"selected"`
	Status     string            `json:"status"`
	StopReason string            `json:"stop_reason,omitempty"`
	Unresolved []string          `json:"unresolved"`
	Report     string            `json:"report"`
}

func (r ResearchTrace) Clone() ResearchTrace {
	b, _ := json.Marshal(r)
	var out ResearchTrace
	_ = json.Unmarshal(b, &out)
	return out
}

func (r ResearchTrace) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 研究报告\n\n%s（截至 %s）\n\n%s\n\n状态：%s；停止原因：%s\n", r.Symbol, r.AsOf, r.Question, r.Status, r.StopReason)
	selected := map[string]bool{}
	for _, id := range r.Selected {
		selected[id] = true
	}
	for _, q := range r.Plan.Questions {
		if r.QuestionSkipped(q.ID) {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n", q.Question)
		fmt.Fprintf(&b, "验收条件：%s\n", strings.Join(q.Acceptance, "；"))
		for _, f := range r.Findings {
			if f.QuestionID == q.ID && selected[f.ID] {
				fmt.Fprintf(&b, "- %s（%s）[%s]\n", f.Finding.Claim, f.Finding.Kind, strings.Join(f.Finding.EvidenceIDs, ", "))
			}
		}
		var latest *ResearchAttempt
		for i := range r.Attempts {
			if r.Attempts[i].QuestionID == q.ID {
				latest = &r.Attempts[i]
			}
		}
		if latest != nil && len(latest.Risks) > 0 {
			fmt.Fprintf(&b, "风险与失效条件：%s\n", strings.Join(latest.Risks, "；"))
		}
	}
	if len(r.Unresolved) > 0 {
		fmt.Fprintf(&b, "\n## 未解决问题\n- %s\n", strings.Join(r.Unresolved, "\n- "))
	}
	b.WriteString("\n引用已校验到原文；推断的正确性和问题覆盖程度仍需人工判断。\n")
	return b.String()
}

func (r ResearchTrace) QuestionSkipped(id string) bool {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].QuestionID == id {
			return r.Attempts[i].Skipped
		}
	}
	return false
}
