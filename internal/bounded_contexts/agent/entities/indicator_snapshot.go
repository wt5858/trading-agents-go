package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// IndicatorSnapshot 是技术指标快照聚合根，自然键 = (symbol, period, trade_date)。
//
// # 为什么它是一个聚合根而不是 MarketBrief 里的一个字段
//
// 因为它需要被持久化、被按键查回、被跨次分析复用——而只有聚合根配仓储。
// 指标必须落库的理由见 value_objects/indicators.go：它整体是一组用乘除
// 从 K 线推导出来的派生量，读路径重算会让同一只票同一天的指标
// 随取数窗口漂移，报告里的数字和图表上的数字对不上，且重跑不可复现。
//
// 于是这里的规则是：ComputeIndicatorSnapshot 只在数据准备阶段被调用一次，
// 结果立刻落库；工具 get_technical_indicators、提示词渲染、失败重跑，
// 全部走仓储读回，没有任何一条读路径会再次调用 ComputeIndicators。
type IndicatorSnapshot struct {
	domain_event.EventRecorder

	Code       shared_vo.StockCode
	TradeDate  shared_vo.TradeDate
	Period     stock_vo.Period
	Indicators value_objects.Indicators
	// Source 记录这批 K 线来自哪个数据源，用于跨源对账：
	// 同一天的指标算出两个值时，第一件要查的事就是两次用的是不是同一个源。
	Source string
	// ComputedAt 是计算时刻。它与 TradeDate 不同：
	// 盘中算出来的「今日指标」和收盘后算出来的并不一样，
	// 没有这个字段就无法解释「为什么昨天看到的今日 MA5 和今天不同」。
	ComputedAt time.Time
}

// ComputeIndicatorSnapshot 计算并构造快照。这是写路径，全系统只有数据准备阶段调用它。
//
// # 只用截至 tradeDate 的 K 线
//
// 入参里的 K 线可能包含 tradeDate 之后的数据（仓储按区间取数、区间右端默认取今天）。
// 直接全量参与计算就是经典的未来函数：回测 2024-03-01 的决策时，
// MA20 里混进了 3 月 5 日的收盘价，得出的结论准得可疑却毫无意义。
// 因此这里先按交易日截断——这是本聚合最重要的一条不变式，
// 放在实体里而不是调用方，是因为任何一个忘记截断的调用方都会静默地产生错误结论。
func ComputeIndicatorSnapshot(
	code shared_vo.StockCode,
	tradeDate shared_vo.TradeDate,
	period stock_vo.Period,
	klines []stock_vo.Kline,
	source string,
) (*IndicatorSnapshot, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("技术指标快照缺少股票代码")
	}
	if tradeDate.IsZero() {
		return nil, custom_errors.Invalid("技术指标快照缺少交易日")
	}
	period = period.OrDaily()

	usable := make([]stock_vo.Kline, 0, len(klines))
	for _, k := range klines {
		// 丢掉无交易日的脏数据，以及晚于目标交易日的那部分。
		// TradeDate 规范化后字典序即时间序，Before 就是完整的判定。
		if k.TradeDate.IsZero() || tradeDate.Before(k.TradeDate) {
			continue
		}
		usable = append(usable, k)
	}
	if len(usable) == 0 {
		return nil, custom_errors.Unavailable("股票(%s) 截至 %s 没有可用 K 线，无法计算技术指标",
			code.FullSymbol(), tradeDate.String())
	}

	ind, err := value_objects.ComputeIndicators(usable)
	if err != nil {
		return nil, err
	}

	s := &IndicatorSnapshot{
		Code:       code,
		TradeDate:  tradeDate,
		Period:     period,
		Indicators: ind,
		Source:     source,
		ComputedAt: time.Now(),
	}
	s.AddDomainEvent(domain_events.NewOnIndicatorsComputed(
		code.FullSymbol(), tradeDate.String(), period.String(), ind.Samples))
	return s, nil
}

// RehydrateIndicatorSnapshot 从持久化数据重建，不做校验也不发事件。
// 库里的行是既成事实；重新跑一遍写入期的校验，只会让一条历史脏数据
// 把整个读路径打挂。
func RehydrateIndicatorSnapshot(
	code shared_vo.StockCode,
	tradeDate shared_vo.TradeDate,
	period stock_vo.Period,
	ind value_objects.Indicators,
	source string,
	computedAt time.Time,
) *IndicatorSnapshot {
	return &IndicatorSnapshot{
		Code:       code,
		TradeDate:  tradeDate,
		Period:     period,
		Indicators: ind,
		Source:     source,
		ComputedAt: computedAt,
	}
}

// HasNaturalKey 判定自然键是否完整。仓储用它挡掉会 upsert 出垃圾文档的记录。
func (s *IndicatorSnapshot) HasNaturalKey() bool {
	return !s.Code.IsZero() && !s.TradeDate.IsZero() && s.Period.Valid()
}

func (s *IndicatorSnapshot) Symbol() string { return s.Code.Symbol }

// Stale 判定快照是否已过期。
//
// 盘中算出来的指标当天之内会不断变化，因此「今天的快照」有保鲜期；
// 而历史交易日的指标一经收盘就是定值，永不过期——
// 这条区分让重跑历史任务完全不必重算，也让盘中分析不会用上几小时前的均线。
func (s *IndicatorSnapshot) Stale(now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	if s.TradeDate.String() != shared_vo.TradeDateOf(now).String() {
		return false
	}
	return now.Sub(s.ComputedAt) > ttl
}
