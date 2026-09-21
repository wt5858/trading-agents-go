// Package domain_events 定义报告上下文对外广播的既成事实。
//
// 事件只带标量快照，不带 *Report 指针、也不带整份章节内容：
// 事件是「已经发生的事实」，带指针会让消费方读到聚合的后续变更；
// 带上十几段 markdown 则会让一个投递到进程外的事件大到无法接受。
// 需要正文的消费方拿 ReportID 回来查。
package domain_events

import (
	"encoding/json"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	OnReportGeneratedEventName = "report.report_generated"
)

// OnReportGenerated 在一份报告成功生成时发出。
//
// 带上 TaskID 是为了让下游（通知、审计）能把报告与触发它的那次分析对上；
// 带上 Action / Confidence 是为了让推送类消费方不必为了拼一句
// 「您关注的 600519 分析完成，建议买入」而回查一次报告正文。
type OnReportGenerated struct {
	domain_event.BaseDomainEvent
	ReportID   string `json:"reportId"`
	TaskID     string `json:"taskId"`
	UserID     uint64 `json:"userId"`
	Symbol     string `json:"symbol"`
	Action     string `json:"action"`
	Confidence string `json:"confidence"`
}

func NewOnReportGenerated(reportID, taskID string, userID uint64, symbol, action string, confidence decimal.Decimal) *OnReportGenerated {
	return &OnReportGenerated{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		ReportID:        reportID,
		TaskID:          taskID,
		UserID:          userID,
		Symbol:          symbol,
		Action:          action,
		Confidence:      decimalx.FormatRatio(confidence),
	}
}

func (e *OnReportGenerated) Name() string { return OnReportGeneratedEventName }

func (e *OnReportGenerated) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
