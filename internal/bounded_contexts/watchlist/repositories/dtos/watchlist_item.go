package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// WatchlistItemDto 对应 watchlist_items 表，是**子实体**的持久化形态。
//
// 注意它没有导出的 ToDomain，理由见本包的包注释。
type WatchlistItemDto struct {
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// (group_id, market, symbol) 唯一索引是「组内不重复」这条不变式的数据库兜底。
	// 内存里 AddItem 已经判过一次，这里再钉一道，是因为内存判断只在
	// 「同一个聚合实例」内成立：同一个用户开两个标签页同时点「加自选」，
	// 会加载出两个各自认为自己没重复的聚合实例。唯一索引是唯一挡得住那一幕的东西。
	GroupID uint64 `gorm:"column:group_id;not null;uniqueIndex:uk_watchlist_items_group_code,priority:1"`
	Market  string `gorm:"column:market;type:varchar(8);not null;uniqueIndex:uk_watchlist_items_group_code,priority:2"`
	Symbol  string `gorm:"column:symbol;type:varchar(16);not null;uniqueIndex:uk_watchlist_items_group_code,priority:3"`
	// Raw 保留用户输入的原始写法（600519.SH / sh600519），仅供排查，不参与索引与判重。
	Raw  string `gorm:"column:raw_code;type:varchar(24);not null;default:''"`
	Note string `gorm:"column:note;type:varchar(300);not null;default:''"`
	// 参考价用定点数而不是 float：它是加入自选那一刻的观测事实，要能逐分复现。
	// float 列在跨库对账时会出现末位漂移，而这一列正是「自选以来涨跌幅」的分母，
	// 分母漂一点，展示出来的百分比就对不上。
	RefPrice     decimal.Decimal `gorm:"column:ref_price;type:decimal(20,4);not null;default:0"`
	RefTradeDate string          `gorm:"column:ref_trade_date;type:varchar(10);not null;default:''"`
	RefPriceAt   *time.Time      `gorm:"column:ref_price_at;type:datetime(3)"`
	// SortOrder 上**没有**唯一约束，这是刻意的：重排的中间态必然出现两行同号
	// （交换两只票的位置时），加了唯一约束就得引入临时负数之类的把戏。
	// 稠密连续由聚合根保证，数据库只负责按它排序。
	SortOrder int       `gorm:"column:sort_order;not null;default:0;index:idx_watchlist_items_group_sort,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (WatchlistItemDto) TableName() string { return "watchlist_items" }

// attachTo 把一行自选项挂回它的聚合根。**不导出**，因为包外不该有任何办法
// 从一行数据造出一个游离的自选项——它在领域里不是一个合法的东西。
//
// 最终落到 g.RehydrateItem 上：无论是领域路径（AddItem）还是重建路径，
// 子实体的产生都必须经过根，这一点在读写两条路上是一致的。
//
// 这里直接拼装 StockCode 而不走 shared_vo.NewStockCode：库里的代码在写入时
// 已经规范化过，读路径再校验一次，只会让「数据源改过格式」的历史行整条查不出来。
func (dto WatchlistItemDto) attachTo(g *entities.WatchlistGroup) {
	g.RehydrateItem(
		dto.ID,
		shared_vo.StockCode{
			Symbol: dto.Symbol,
			Market: shared_vo.Market(dto.Market),
			Raw:    dto.Raw,
		},
		value_objects.RehydrateItemNote(dto.Note),
		value_objects.RehydrateReferencePrice(
			dto.RefPrice,
			shared_vo.MustTradeDate(dto.RefTradeDate),
			dto.RefPriceAt,
		),
		dto.SortOrder,
		dto.CreatedAt,
		dto.UpdatedAt,
	)
}

// FromDomainItem 把子实体投影成 DTO。
//
// groupID 由调用方显式传入，而不是读 it.GroupID：新建分组时子实体身上的
// group_id 还是 0（自增主键要等根 INSERT 之后才知道）。由仓储在拿到主键之后
// 统一传入，比让每个调用点自己记得先回填要可靠。
//
// StockCode 拆成三列而不是整体序列化：market / symbol 要进唯一索引和 WHERE 条件，
// 存成 JSON 就查不了了。
func FromDomainItem(groupID uint64, it *entities.WatchlistItem) *WatchlistItemDto {
	dto := &WatchlistItemDto{
		ID:           it.ID,
		GroupID:      groupID,
		Market:       it.Code.Market.String(),
		Symbol:       it.Code.Symbol,
		Raw:          it.Code.Raw,
		Note:         it.Note.String(),
		RefPrice:     it.RefPrice.Price,
		RefTradeDate: it.RefPrice.TradeDate.String(),
		SortOrder:    it.SortOrder,
		CreatedAt:    it.CreatedAt,
		UpdatedAt:    it.UpdatedAt,
	}
	if !it.RefPrice.At.IsZero() {
		at := it.RefPrice.At
		dto.RefPriceAt = &at
	}
	return dto
}

// SameAs 判断一行子实体是否与库里那一行完全一致，供仓储的子实体 diff 使用。
//
// 只比**可变列**：id / group_id / market / symbol / created_at 一旦写入就不再变化，
// 把它们纳入比较不会多发现任何差异，只会让「改了备注」这类修改因为
// created_at 的时区往返误差而被误判成有变化，白白多发一条 UPDATE。
//
// ref_price 三列也在比较范围内：移动自选股会带着参考价一起搬家，
// 落到目标分组时它们是新值。
func (dto WatchlistItemDto) SameAs(other WatchlistItemDto) bool {
	if dto.Note != other.Note ||
		dto.SortOrder != other.SortOrder ||
		dto.RefPrice != other.RefPrice ||
		dto.RefTradeDate != other.RefTradeDate {
		return false
	}
	switch {
	case dto.RefPriceAt == nil && other.RefPriceAt == nil:
		return true
	case dto.RefPriceAt == nil || other.RefPriceAt == nil:
		return false
	default:
		return dto.RefPriceAt.Equal(*other.RefPriceAt)
	}
}

// FromDomainItems 批量投影，供仓储一次性构造批量写入的入参。
func FromDomainItems(groupID uint64, items []*entities.WatchlistItem) []*WatchlistItemDto {
	out := make([]*WatchlistItemDto, 0, len(items))
	for _, it := range items {
		out = append(out, FromDomainItem(groupID, it))
	}
	return out
}
