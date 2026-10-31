package entities

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

var ErrResearchBudget = errors.New("research request/token budget exhausted")

// ResearchBudget reserves a conservative input/output allowance before a request.
// Unknown usage retains its reservation rather than creating free capacity.
type ResearchBudget struct {
	mu                    sync.Mutex
	limits                vo.ResearchLimits
	calls, used, reserved int
}

func NewResearchBudget(l vo.ResearchLimits) *ResearchBudget { return &ResearchBudget{limits: l} }
func (b *ResearchBudget) Reserve(tokens int) (func(*vo.Usage), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tokens < 1 || b.calls >= b.limits.MaxRequests || b.used+b.reserved+tokens > b.limits.MaxTokens {
		return nil, ErrResearchBudget
	}
	b.calls++
	b.reserved += tokens
	return func(u *vo.Usage) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.reserved -= tokens
		if u == nil {
			b.calls--
			return
		}
		if u.TotalTokens > 0 {
			b.used += u.TotalTokens
		} else {
			b.used += tokens
		}
	}, nil
}

func (c *AnalysisContext) StartResearch(question string, l vo.ResearchLimits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	question = strings.TrimSpace(question)
	if err := vo.ValidateResearchQuestion(question); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.research != nil {
		return fmt.Errorf("research already started")
	}
	c.research = &vo.ResearchTrace{Symbol: c.code.FullSymbol(), AsOf: c.tradeDate.String(), Version: vo.ResearchVersion, Question: question, Limits: l, Status: "running"}
	return nil
}
func (c *AnalysisContext) PlanResearch(p vo.ResearchPlan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.research == nil || c.research.Status != "running" || len(c.research.Plan.Questions) > 0 {
		return fmt.Errorf("research cannot be planned")
	}
	r := c.research.Clone()
	r.Plan = p
	r = r.Clone()
	c.research = &r
	return nil
}
func (c *AnalysisContext) ReviewResearch(review vo.ResearchReview) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.research == nil || c.research.Status != "running" || c.research.Review != nil {
		return fmt.Errorf("research requires exactly one concentrated review")
	}
	var gaps []vo.ResearchGap
	for _, g := range review.Gaps {
		if !c.research.QuestionSkipped(g.QuestionID) {
			gaps = append(gaps, g)
		}
	}
	review.Gaps = gaps
	if err := review.Validate(c.research.Plan); err != nil {
		return err
	}
	r := c.research.Clone()
	r.Review = &review
	r = r.Clone()
	c.research = &r
	return nil
}
func (c *AnalysisContext) RecordResearchAttempt(a vo.ResearchAttempt, report *vo.AnalystReport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.research == nil || c.research.Status != "running" {
		return fmt.Errorf("research is not running")
	}
	known := false
	for _, q := range c.research.Plan.Questions {
		known = known || q.ID == a.QuestionID
	}
	if !known || a.Round < 0 || a.Round > c.research.Limits.SupplementRounds {
		return fmt.Errorf("invalid research attempt")
	}
	for _, old := range c.research.Attempts {
		if old.QuestionID == a.QuestionID && old.Round == a.Round {
			return fmt.Errorf("duplicate research attempt")
		}
	}
	if report != nil {
		a.Status = report.Status
		a.Missing = append([]string(nil), report.Missing...)
		a.Risks = append([]string(nil), report.Risks...)
	}
	c.research.Attempts = append(c.research.Attempts, a)
	if a.Skipped {
		c.research.Attempts[len(c.research.Attempts)-1].Status = "skipped"
		return nil
	}
	if a.Error == "" && report != nil && report.SupportVerified {
		for i, f := range report.Clone().Findings {
			c.research.Findings = append(c.research.Findings, vo.ResearchFinding{ID: fmt.Sprintf("%s-r%d-f%d", a.QuestionID, a.Round, i+1), QuestionID: a.QuestionID, Round: a.Round, Finding: f})
		}
	}
	return nil
}
func (c *AnalysisContext) Research() *vo.ResearchTrace {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.research == nil {
		return nil
	}
	r := c.research.Clone()
	return &r
}

// FinishResearch keeps review gaps open after supplementation: new quotations alone
// cannot prove that a contradiction or a substantive research question is resolved.
func (c *AnalysisContext) FinishResearch(selection vo.ResearchSelection, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.research == nil || c.research.Status != "running" {
		return fmt.Errorf("research is not running")
	}
	r := c.research
	allSkipped := len(r.Plan.Questions) > 0
	for _, q := range r.Plan.Questions {
		if !r.QuestionSkipped(q.ID) {
			allSkipped = false
		}
	}
	if allSkipped {
		r.Status = "skipped"
		r.StopReason = reason
		r.Report = ""
		r.Unresolved = nil
		return nil
	}
	known := map[string]vo.ResearchFinding{}
	for _, f := range r.Findings {
		known[f.ID] = f
	}
	seen, coverage := map[string]bool{}, map[string]bool{}
	for _, id := range selection.FindingIDs {
		f, ok := known[id]
		if !ok || seen[id] {
			return fmt.Errorf("invalid research selection: %s", id)
		}
		seen[id] = true
		if f.Finding.Kind != "hypothesis" {
			coverage[f.QuestionID] = true
		}
	}
	r.Selected = append([]string(nil), selection.FindingIDs...)
	for _, q := range r.Plan.Questions {
		if r.QuestionSkipped(q.ID) {
			continue
		}
		if !coverage[q.ID] {
			r.Unresolved = append(r.Unresolved, q.Question+"：缺少已引用的事实或推断")
		}
	}
	latest := map[string]vo.ResearchAttempt{}
	for _, a := range r.Attempts {
		latest[a.QuestionID] = a
	}
	for _, q := range r.Plan.Questions {
		if r.QuestionSkipped(q.ID) {
			continue
		}
		a, ok := latest[q.ID]
		if !ok {
			r.Unresolved = append(r.Unresolved, q.ID+"：研究任务未执行")
			continue
		}
		if a.Error != "" {
			r.Unresolved = append(r.Unresolved, fmt.Sprintf("%s 第%d轮失败：%s", a.QuestionID, a.Round, a.Error))
		}
		if a.Status != "complete" || a.Truncated {
			r.Unresolved = append(r.Unresolved, q.ID+"：分析尚未完整完成")
		}
		for _, missing := range a.Missing {
			r.Unresolved = append(r.Unresolved, q.ID+"："+missing)
		}
	}
	if r.Review == nil {
		r.Unresolved = append(r.Unresolved, "集中审查未完成")
	} else {
		for _, g := range r.Review.Gaps {
			r.Unresolved = append(r.Unresolved, g.QuestionID+"："+g.Reason+"（补证结果待复核）")
		}
	}
	if reason != "completed" {
		r.Unresolved = append(r.Unresolved, "运行停止："+reason)
	}
	r.Status = "complete"
	if len(r.Unresolved) > 0 {
		r.Status = "partial"
	}
	if len(r.Selected) == 0 {
		r.Status = "insufficient"
	}
	r.StopReason = reason
	r.Report = r.Markdown()
	return nil
}
