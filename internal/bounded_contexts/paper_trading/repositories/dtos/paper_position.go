package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// PaperPositionDto 是 paper_positions 表的持久化对象。
//
// # 它是子实体的持久化形状，不代表 Position 有自己的仓储
//
// 这张表只被 PaperAccountRepository 读写，而且只在 Save 的那个事务里被写。
// 「每个聚合根一个仓储」约束的是**对外暴露的入口**，不是表的数量：
// 子实体当然要有自己的表，但外界拿不到直接操作它的方法。
//
// 主键是 (account_id, symbol) 复合键而不是自增 ID：
// 一个账户里一只票只能有一行持仓，这是聚合的不变式，让主键去执行它，
// 比在应用层写一段查重代码可靠得多，也让 Save 的 upsert 有天然的冲突目标。
type PaperPositionDto struct {
	AccountID string `gorm:"column:account_id;type:varchar(48);primaryKey"`
	Symbol    string `gorm:"column:symbol;type:varchar(16);primaryKey"`
	Market    string `gorm:"column:market;type:varchar(8);not null;default:''"`
	// Raw 保留用户当初输入的原始写法，重建 StockCode 时还原，便于排查。
	Raw string `gorm:"column:symbol_raw;type:varchar(24);not null;default:''"`

	Quantity decimal.Decimal `gorm:"column:quantity;type:decimal(20,8);not null"`
	// AvgCost 与 CostBasis 都是买入时算好的派生量。两个都存不是冗余：
	// AvgCost 按金额精度取整后反乘回去未必等于当初真正划走的现金，
	// 而 CostBasis 才是那个事实。详见 entities/position.go。
	AvgCost   decimal.Decimal `gorm:"column:avg_cost;type:decimal(20,4);not null"`
	CostBasis decimal.Decimal `gorm:"column:cost_basis;type:decimal(20,4);not null"`

	OpenedAt  time.Time `gorm:"column:opened_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (PaperPositionDto) TableName() string { return "paper_positions" }

// ToDomain 重建持仓子实体。
//
// 走 RehydratePosition 而不是直接构造字面量，是为了把「读回来不重算平均成本」
// 这条纪律固定在一个函数里：那里有完整的理由说明。
func (dto PaperPositionDto) ToDomain() *entities.Position {
	return entities.RehydratePosition(
		shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.Raw},
		dto.Quantity,
		dto.AvgCost,
		dto.CostBasis,
		dto.OpenedAt,
		dto.UpdatedAt,
	)
}

func FromDomainPosition(accountID string, p *entities.Position) *PaperPositionDto {
	return &PaperPositionDto{
		AccountID: accountID,
		Symbol:    p.Code.Symbol,
		Market:    p.Code.Market.String(),
		Raw:       p.Code.Raw,
		Quantity:  p.Quantity,
		AvgCost:   p.AvgCost,
		CostBasis: p.CostBasis,
		OpenedAt:  p.OpenedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func FromDomainPositions(accountID string, positions []*entities.Position) []*PaperPositionDto {
	out := make([]*PaperPositionDto, 0, len(positions))
	for _, p := range positions {
		out = append(out, FromDomainPosition(accountID, p))
	}
	return out
}
