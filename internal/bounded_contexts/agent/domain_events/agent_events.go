// Package domain_events 声明 agent 上下文对外发布的领域事件。
//
// 事件只携带标识与结果，不携带实体：消费方（计费、审计、报告上下文）
// 拿到的必须是一份自洽的事实快照，而不是一个指向可变聚合的指针。
// 股票代码与交易日以字符串形式携带，同样是为了让事件可以被序列化后跨进程投递。
package domain_events

import (
	"encoding/json"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	OnAgentCompletedEventName = "agent.agent_completed"
	OnAgentFailedEventName    = "agent.agent_failed"
	OnDecisionMadeEventName   = "agent.decision_made"
)

// OnAgentCompleted 在一位成员产出报告后发布，是成本归因的原始凭据。
//
// CostUSD 以字符串承载：这条事件是计费的原始凭据，会被下游反序列化后累加，
// 任何一个消费者用 float64 接都会让账单在第几千次累加后开始漂。
type OnAgentCompleted struct {
	domain_event.BaseDomainEvent
	Symbol    string `json:"symbol"`
	TradeDate string `json:"tradeDate"`
	Agent     string `json:"agent"`
	Tokens    int    `json:"tokens"`
	CostUSD   string `json:"costUsd"`
}

func NewOnAgentCompleted(symbol, tradeDate, agent string, tokens int, costUSD decimal.Decimal) *OnAgentCompleted {
	return &OnAgentCompleted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Symbol:          symbol,
		TradeDate:       tradeDate,
		Agent:           agent,
		Tokens:          tokens,
		CostUSD:         decimalx.FormatMoney(costUSD),
	}
}

func (e *OnAgentCompleted) Name() string { return OnAgentCompletedEventName }

func (e *OnAgentCompleted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnAgentFailed 在一位成员失败后发布。
//
// 它不代表整次分析失败——容错阶段里单个成员挂掉是被允许的。
// 独立成一个事件是为了让运维能看出「最近三天情绪分析师失败率 40%」这种趋势，
// 而这种信息在「整次分析成功」的结果里是看不见的。
type OnAgentFailed struct {
	domain_event.BaseDomainEvent
	Symbol    string `json:"symbol"`
	TradeDate string `json:"tradeDate"`
	Agent     string `json:"agent"`
	Reason    string `json:"reason"`
}

func NewOnAgentFailed(symbol, tradeDate, agent, reason string) *OnAgentFailed {
	return &OnAgentFailed{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Symbol:          symbol,
		TradeDate:       tradeDate,
		Agent:           agent,
		Reason:          reason,
	}
}

func (e *OnAgentFailed) Name() string { return OnAgentFailedEventName }

func (e *OnAgentFailed) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnDecisionMade 在风控经理给出终局决策后发布。
type OnDecisionMade struct {
	domain_event.BaseDomainEvent
	Symbol     string `json:"symbol"`
	TradeDate  string `json:"tradeDate"`
	Action     string `json:"action"`
	Confidence string `json:"confidence"`
	RiskScore  string `json:"riskScore"`
}

func NewOnDecisionMade(symbol, tradeDate, action string, confidence, riskScore decimal.Decimal) *OnDecisionMade {
	return &OnDecisionMade{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Symbol:          symbol,
		TradeDate:       tradeDate,
		Action:          action,
		Confidence:      decimalx.FormatRatio(confidence),
		RiskScore:       decimalx.FormatPercent(riskScore),
	}
}

func (e *OnDecisionMade) Name() string { return OnDecisionMadeEventName }

func (e *OnDecisionMade) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnIndicatorsComputed 在技术指标被首次计算并落库后发布。
//
// 它存在的意义是让「指标是什么时候、用哪一批 K 线算出来的」成为一条可追溯的事实：
// 指标一经落库就不再重算，那么一旦有人质疑某个 MA20，
// 唯一能回答的就是这条事件加上库里的那条快照。
type OnIndicatorsComputed struct {
	domain_event.BaseDomainEvent
	Symbol    string `json:"symbol"`
	TradeDate string `json:"tradeDate"`
	Period    string `json:"period"`
	Samples   int    `json:"samples"`
}

func NewOnIndicatorsComputed(symbol, tradeDate, period string, samples int) *OnIndicatorsComputed {
	return &OnIndicatorsComputed{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		Symbol:          symbol,
		TradeDate:       tradeDate,
		Period:          period,
		Samples:         samples,
	}
}

func (e *OnIndicatorsComputed) Name() string { return "agent.indicators_computed" }

func (e *OnIndicatorsComputed) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
