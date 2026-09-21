// Package dtos 是模拟交易上下文的持久化形状：表结构，以及与聚合的双向映射。
//
// 映射写在 DTO 文件里（DTO 上挂 ToDomain()，包级函数 FromDomainXxx()），
// 不另开 mapper 包：映射和它服务的表结构必须同生共死，拆开只会让改一次列
// 要动两个目录，还容易漏。
//
// 铁律：DTO 绝不越过 repositories/ 这一层。上层拿到的永远是聚合或值对象。
//
// # 金额列为什么是 decimal.Decimal 而不是 float64
//
// shopspring/decimal 自带 driver.Valuer 与 sql.Scanner，可以和 MySQL 的
// decimal(20,4) 无损往返。换成 float64 的话，读写各经历一次二进制浮点转换，
// 一个 0.1 存进去再读出来就可能变成 0.09999999999999999——
// 模拟账本会在肉眼看不见的地方慢慢对不平。
package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
)

// 编译期断言：DTO 必须自带表名，漏写会让 GORM 按结构体名推导出错误的表。
var (
	_ interface{ TableName() string } = PaperAccountDto{}
	_ interface{ TableName() string } = PaperPositionDto{}
	_ interface{ TableName() string } = PaperTradeDto{}
)

// PaperAccountDto 是 paper_accounts 表的持久化对象（聚合根）。
type PaperAccountDto struct {
	// ID 由领域服务生成：账户要先有稳定标识，持仓与成交才能引用它。
	ID string `gorm:"column:id;type:varchar(48);primaryKey"`
	// 用户的账户列表是最热的查询，单列索引即可（单用户账户数是个位数）。
	UserID uint64 `gorm:"column:user_id;not null;index:idx_paper_accounts_user"`
	Name   string `gorm:"column:name;type:varchar(64);not null;default:''"`

	InitialCash decimal.Decimal `gorm:"column:initial_cash;type:decimal(20,4);not null"`
	Cash        decimal.Decimal `gorm:"column:cash;type:decimal(20,4);not null"`
	// 已实现盈亏与累计手续费都是逐笔累加的落库值，读路径直接取，不去成交表 SUM。
	RealizedPnl decimal.Decimal `gorm:"column:realized_pnl;type:decimal(20,4);not null"`
	TotalFee    decimal.Decimal `gorm:"column:total_fee;type:decimal(20,4);not null"`

	// Version 是乐观锁版本号，见 PaperAccountRepository.Save。
	Version int64 `gorm:"column:version;not null;default:0"`

	// 时间戳由聚合维护，关掉 GORM 的自动写入，否则导入存量数据会被静默改写。
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (PaperAccountDto) TableName() string { return "paper_accounts" }

// ToDomain 重建聚合根**但不含持仓**。
//
// 持仓由 ToDomainAccountsWithPositions 单独一次查询后挂上去：这是一个刻意的
// 两段式设计，为的是让「取 N 个账户 + 取它们全部持仓」永远是两条语句，
// 而不是 1 + N 条。让 ToDomain 自己去查持仓会强迫它持有一个 DB 句柄，
// 那才是 N+1 真正的来源。
//
// 这里不做任何校验：已经落库的行是既成事实，把它们再过一遍构造校验，
// 一行历史脏数据就能打挂整个账户列表接口。校验属于写路径。
func (dto PaperAccountDto) ToDomain() *entities.PaperAccount {
	return &entities.PaperAccount{
		ID:          dto.ID,
		UserID:      dto.UserID,
		Name:        dto.Name,
		InitialCash: dto.InitialCash,
		Cash:        dto.Cash,
		RealizedPnL: dto.RealizedPnl,
		TotalFee:    dto.TotalFee,
		Positions:   []*entities.Position{},
		CreatedAt:   dto.CreatedAt,
		UpdatedAt:   dto.UpdatedAt,
		Version:     dto.Version,
	}
}

func FromDomainAccount(a *entities.PaperAccount) *PaperAccountDto {
	return &PaperAccountDto{
		ID:          a.ID,
		UserID:      a.UserID,
		Name:        a.Name,
		InitialCash: a.InitialCash,
		Cash:        a.Cash,
		RealizedPnl: a.RealizedPnL,
		TotalFee:    a.TotalFee,
		Version:     a.Version,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
	}
}

// ToDomainAccountsWithPositions 把两次查询的结果装配成完整的聚合。
//
// 持仓先按 account_id 装进 map 再分发，整个装配是 O(账户数 + 持仓数)，
// 而不是对每个账户遍历一遍持仓切片。
func ToDomainAccountsWithPositions(
	accountRows []*PaperAccountDto,
	positionRows []*PaperPositionDto,
) []*entities.PaperAccount {
	grouped := make(map[string][]*entities.Position, len(accountRows))
	for _, p := range positionRows {
		grouped[p.AccountID] = append(grouped[p.AccountID], p.ToDomain())
	}

	out := make([]*entities.PaperAccount, 0, len(accountRows))
	for _, row := range accountRows {
		acc := row.ToDomain()
		if ps, ok := grouped[acc.ID]; ok {
			acc.Positions = ps
		}
		out = append(out, acc)
	}
	return out
}
