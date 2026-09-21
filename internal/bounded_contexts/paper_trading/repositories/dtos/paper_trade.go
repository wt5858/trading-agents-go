package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// PaperTradeDto 是 paper_trades 表的持久化对象。这张表只追加，没有 UPDATE 路径。
type PaperTradeDto struct {
	ID        string `gorm:"column:id;type:varchar(48);primaryKey"`
	AccountID string `gorm:"column:account_id;type:varchar(48);not null;index:idx_paper_trades_account,priority:1"`
	// (account_id, traded_at) 覆盖唯一的读路径：某账户的成交历史按时间倒序分页，
	// 排序直接吃索引而不是 filesort。
	TradedAt time.Time `gorm:"column:traded_at;type:datetime(3);not null;index:idx_paper_trades_account,priority:2"`

	Symbol string `gorm:"column:symbol;type:varchar(16);not null"`
	Market string `gorm:"column:market;type:varchar(8);not null;default:''"`
	Raw    string `gorm:"column:symbol_raw;type:varchar(24);not null;default:''"`
	Side   string `gorm:"column:side;type:varchar(8);not null"`

	Quantity decimal.Decimal `gorm:"column:quantity;type:decimal(20,8);not null"`
	Price    decimal.Decimal `gorm:"column:price;type:decimal(20,4);not null"`
	// Amount 是成交那一刻算好的乘法派生量，也是真正从现金里划走/划入的数。
	// 它必须独立成列：把它当成「可以用 quantity × price 随时算出来的冗余」
	// 而省掉，就等于让展示值和账户余额的变动失去唯一事实来源。
	Amount      decimal.Decimal `gorm:"column:amount;type:decimal(20,4);not null"`
	Fee         decimal.Decimal `gorm:"column:fee;type:decimal(20,4);not null"`
	RealizedPnl decimal.Decimal `gorm:"column:realized_pnl;type:decimal(20,4);not null"`
	CashAfter   decimal.Decimal `gorm:"column:cash_after;type:decimal(20,4);not null"`
}

func (PaperTradeDto) TableName() string { return "paper_trades" }

// ToDomainRecord 把成交行转成**读模型值对象**，而不是实体。
//
// 成交历史没有生命周期、没有不变式要守，让它绕道聚合只会白白付出
// 「为了看一页历史而加载整个账户」的代价。读路径上 DTO → VO 是允许的。
//
// 注意 Amount 是原样搬运的存量值，这里没有 Quantity.Mul(Price)。
func (dto PaperTradeDto) ToDomainRecord() value_objects.TradeRecord {
	return value_objects.RehydrateTradeRecord(
		dto.ID,
		dto.AccountID,
		shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.Raw},
		value_objects.RehydrateOrderSide(dto.Side),
		dto.Quantity,
		dto.Price,
		dto.Amount,
		dto.Fee,
		dto.RealizedPnl,
		dto.CashAfter,
		dto.TradedAt,
	)
}

func FromDomainTrade(t *entities.Trade) *PaperTradeDto {
	return &PaperTradeDto{
		ID:          t.ID,
		AccountID:   t.AccountID,
		TradedAt:    t.TradedAt,
		Symbol:      t.Code.Symbol,
		Market:      t.Code.Market.String(),
		Raw:         t.Code.Raw,
		Side:        t.Side.String(),
		Quantity:    t.Quantity,
		Price:       t.Price,
		Amount:      t.Amount,
		Fee:         t.Fee,
		RealizedPnl: t.RealizedPnL,
		CashAfter:   t.CashAfter,
	}
}

func FromDomainTrades(trades []*entities.Trade) []*PaperTradeDto {
	out := make([]*PaperTradeDto, 0, len(trades))
	for _, t := range trades {
		out = append(out, FromDomainTrade(t))
	}
	return out
}

func ToDomainTradeRecords(rows []*PaperTradeDto) []value_objects.TradeRecord {
	out := make([]value_objects.TradeRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomainRecord())
	}
	return out
}
