// Package dtos 承载 agent 上下文的持久化对象与映射。
//
// 映射写在 DTO 文件里，不单独开 mapper 包：DTO 与它的 ToDomain/FromDomain
// 是同一件事的两面，分到两个文件只会让「加了一列忘了改映射」变成常态。
//
// DTO 永远不离开 repositories 包：上层拿到的是实体与值对象。
package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// codeColumns 是 StockCode 在 Mongo 文档里的扁平形态。
//
// 为什么不给 shared_vo.StockCode 直接打 bson 标签：它是嵌套结构，落成子文档后
// symbol 会变成 code.symbol，自然键索引直接失效；而 shared_vo.TradeDate 的
// 内部字段不导出，bson 编解码器看不见它，序列化出来是一个空对象 {}。
// 字段名与 stock 上下文的 market_data 集合逐字一致，跨集合聚合才拼得起来。
type codeColumns struct {
	Symbol string `bson:"symbol"`
	Market string `bson:"market"`
	Raw    string `bson:"raw_code,omitempty"`
}

func codeColumnsOf(c shared_vo.StockCode) codeColumns {
	return codeColumns{Symbol: c.Symbol, Market: string(c.Market), Raw: c.Raw}
}

// toDomain 直接拼装而不走 NewStockCode：库里的行是既成事实，
// 读路径再跑一次写入期的校验，只会让历史脏数据把查询打挂。
func (c codeColumns) toDomain() shared_vo.StockCode {
	return shared_vo.StockCode{Symbol: c.Symbol, Market: shared_vo.Market(c.Market), Raw: c.Raw}
}

// indicatorColumns 是 Indicators 值对象的落库形态。
//
// 每一个指标各占一列，一个不漏地全部存下来——这正是「派生量必须持久化」
// 这条规则的落点：MA/RSI/MACD/BOLL/ATR 以及乖离率都是从 K 线用乘除推出来的，
// 存下来读回去，读路径永远不会再算第二遍。
// 少存一列的后果不是「那一项没有」，而是「那一项会在读路径上被重算」，
// 于是它和同一份快照里的其他指标就不再是同一批 K 线的产物了。
type indicatorColumns struct {
	Close decimal.Decimal `bson:"close"`

	MA5  decimal.Decimal `bson:"ma5"`
	MA10 decimal.Decimal `bson:"ma10"`
	MA20 decimal.Decimal `bson:"ma20"`
	MA60 decimal.Decimal `bson:"ma60"`

	EMA12 decimal.Decimal `bson:"ema12"`
	EMA26 decimal.Decimal `bson:"ema26"`

	MACDDIF  decimal.Decimal `bson:"macd_dif"`
	MACDDEA  decimal.Decimal `bson:"macd_dea"`
	MACDHist decimal.Decimal `bson:"macd_hist"`

	RSI6  decimal.Decimal `bson:"rsi6"`
	RSI14 decimal.Decimal `bson:"rsi14"`

	BollUpper decimal.Decimal `bson:"boll_upper"`
	BollMid   decimal.Decimal `bson:"boll_mid"`
	BollLower decimal.Decimal `bson:"boll_lower"`

	ATR14 decimal.Decimal `bson:"atr14"`

	VolMA5  decimal.Decimal `bson:"vol_ma5"`
	VolMA20 decimal.Decimal `bson:"vol_ma20"`

	DeviationMA20Pct decimal.Decimal `bson:"deviation_ma20_pct"`

	Samples int `bson:"samples"`
}

func indicatorColumnsOf(in value_objects.Indicators) indicatorColumns {
	return indicatorColumns{
		Close:            in.Close,
		MA5:              in.MA5,
		MA10:             in.MA10,
		MA20:             in.MA20,
		MA60:             in.MA60,
		EMA12:            in.EMA12,
		EMA26:            in.EMA26,
		MACDDIF:          in.MACDDIF,
		MACDDEA:          in.MACDDEA,
		MACDHist:         in.MACDHist,
		RSI6:             in.RSI6,
		RSI14:            in.RSI14,
		BollUpper:        in.BollUpper,
		BollMid:          in.BollMid,
		BollLower:        in.BollLower,
		ATR14:            in.ATR14,
		VolMA5:           in.VolMA5,
		VolMA20:          in.VolMA20,
		DeviationMA20Pct: in.DeviationMA20Pct,
		Samples:          in.Samples,
	}
}

func (c indicatorColumns) toDomain() value_objects.Indicators {
	return value_objects.Indicators{
		Close:            c.Close,
		MA5:              c.MA5,
		MA10:             c.MA10,
		MA20:             c.MA20,
		MA60:             c.MA60,
		EMA12:            c.EMA12,
		EMA26:            c.EMA26,
		MACDDIF:          c.MACDDIF,
		MACDDEA:          c.MACDDEA,
		MACDHist:         c.MACDHist,
		RSI6:             c.RSI6,
		RSI14:            c.RSI14,
		BollUpper:        c.BollUpper,
		BollMid:          c.BollMid,
		BollLower:        c.BollLower,
		ATR14:            c.ATR14,
		VolMA5:           c.VolMA5,
		VolMA20:          c.VolMA20,
		DeviationMA20Pct: c.DeviationMA20Pct,
		Samples:          c.Samples,
	}
}

// IndicatorSnapshotDto 对应 agent_indicators 集合。
// 自然键 = (symbol, period, trade_date)，与 stock 上下文的 klines 集合同构——
// 指标是从 K 线推导出来的，键不同构就没法把两者对起来查。
type IndicatorSnapshotDto struct {
	codeColumns `bson:",inline"`
	Period      string `bson:"period"`
	// TradeDate 存 YYYY-MM-DD 定长串而不是 BSON date：
	// 定长串的字典序等价于时间序，区间查询可以直接 $lte，且省掉时区换算。
	TradeDate  string           `bson:"trade_date"`
	Indicators indicatorColumns `bson:"indicators"`
	Source     string           `bson:"source"`
	ComputedAt time.Time        `bson:"computed_at"`
}

func (dto IndicatorSnapshotDto) ToDomain() *entities.IndicatorSnapshot {
	return entities.RehydrateIndicatorSnapshot(
		dto.toDomain(),
		shared_vo.MustTradeDate(dto.TradeDate),
		stock_vo.Period(dto.Period),
		dto.Indicators.toDomain(),
		dto.Source,
		dto.ComputedAt,
	)
}

func FromDomainIndicatorSnapshot(s *entities.IndicatorSnapshot) *IndicatorSnapshotDto {
	return &IndicatorSnapshotDto{
		codeColumns: codeColumnsOf(s.Code),
		Period:      s.Period.String(),
		TradeDate:   s.TradeDate.String(),
		Indicators:  indicatorColumnsOf(s.Indicators),
		Source:      s.Source,
		// BSON 的 datetime 精度到毫秒，先截断：
		// 写进去的时间和读回来的不相等会让「快照是否过期」的判断莫名其妙地抖动。
		ComputedAt: s.ComputedAt.Truncate(time.Millisecond),
	}
}

func ToDomainIndicatorSnapshots(rows []IndicatorSnapshotDto) []*entities.IndicatorSnapshot {
	out := make([]*entities.IndicatorSnapshot, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
