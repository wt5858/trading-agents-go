// Package dtos 承载自选股上下文的持久化对象与领域映射。
//
// 映射函数（ToDomain / FromDomainXxx）与 DTO 定义在同一文件，本项目没有 mapper 包：
// 表结构与它的映射规则是同一件事的两面，拆开只会让改一列要开两个文件。
//
// 本包的类型绝不允许越过 repositories/ 向上泄漏。带 gorm 标签的结构一旦出现在
// domain_services 或 handler 的签名里，换存储就要改业务代码。
//
// # 本包如何体现「子实体只能经由根产生」
//
// WatchlistItemDto **没有** 导出的 ToDomain：一个脱离分组的自选项在领域里
// 根本不是一个合法的东西，给它一个能单独造出实体的函数，就等于在持久化层
// 开了一扇绕过聚合根的后门。它只有一个不导出的 attachTo，由
// WatchlistGroupDto.ToDomain 调用，最终落到根的 RehydrateItem 上。
package dtos

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
)

// WatchlistGroupDto 对应 watchlist_groups 表。
//
// 通用约定与其它上下文一致：时间统一 datetime(3)、关掉 GORM 的自动时间戳
// （时间由聚合根维护，否则「导入存量自选股」会被静默改写成导入时刻）。
type WatchlistGroupDto struct {
	ID     uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	UserID uint64 `gorm:"column:user_id;not null;uniqueIndex:uk_watchlist_groups_user_name,priority:1"`
	// (user_id, name) 唯一索引是「同一用户下组名唯一」这条不变式的**唯一**保证。
	// 聚合里没有对应的内存判断，因为那条规则跨聚合实例，内存看不见兄弟分组；
	// 而「先查有没有重名再插」是 TOCTOU，两个并发请求会双双查到「没有」然后都插进去。
	Name      string    `gorm:"column:name;type:varchar(64);not null;uniqueIndex:uk_watchlist_groups_user_name,priority:2"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (WatchlistGroupDto) TableName() string { return "watchlist_groups" }

// ToDomain 把一行分组连同它的子实体行重建成完整聚合。
//
// 签名里带着 items 是刻意的：这个聚合**没有**「只有根、没有子实体」的合法形态。
// 允许单独重建一个空壳根，调用方迟早会拿着它去 Save，而 Save 会把
// 「子实体为空」如实理解为「用户把这一组清空了」，一次读取就变成了一次删除。
// 要重建就得给全，这个约束由签名本身表达。
func (dto WatchlistGroupDto) ToDomain(items []WatchlistItemDto) *entities.WatchlistGroup {
	g := entities.RehydrateWatchlistGroup(
		dto.ID,
		dto.UserID,
		// 不走 NewGroupName：落库的行是既成事实，读路径再校验一次，
		// 一条规则收紧前存下的历史数据就能把整个列表接口打挂。
		value_objects.RehydrateGroupName(dto.Name),
		dto.CreatedAt,
		dto.UpdatedAt,
		len(items),
	)
	for i := range items {
		items[i].attachTo(g)
	}
	return g
}

func FromDomainGroup(g *entities.WatchlistGroup) *WatchlistGroupDto {
	return &WatchlistGroupDto{
		ID:        g.ID,
		UserID:    g.UserID,
		Name:      g.Name.String(),
		CreatedAt: g.CreatedAt,
		UpdatedAt: g.UpdatedAt,
	}
}

// ToDomainGroups 把「一批分组行 + 它们全部的子实体行」缝成若干个完整聚合。
//
// 缝合放在本包而不是仓储里，是为了让仓储的 ListByUser 读起来就是
// 「两条查询 + 一次缝合」这三行。
//
// 实现上先按 group_id 归拢成一个 map 再分发，整体 O(分组数 + 自选项数)。
// 绝不能写成「对每个分组遍历一遍全部自选项」——那是 O(分组数 × 自选项数)，
// 也绝不能写成「对每个分组查一次库」——那是 N+1，正是本方法存在的理由。
//
// itemRows 必须已经按 (group_id, sort_order) 排好序：排序是数据库的活，
// 在这里再排一次是重复劳动，而且会让「排序口径」出现第二个定义点。
func ToDomainGroups(groupRows []WatchlistGroupDto, itemRows []WatchlistItemDto) []*entities.WatchlistGroup {
	byGroup := make(map[uint64][]WatchlistItemDto, len(groupRows))
	for _, row := range itemRows {
		byGroup[row.GroupID] = append(byGroup[row.GroupID], row)
	}
	out := make([]*entities.WatchlistGroup, 0, len(groupRows))
	for _, g := range groupRows {
		out = append(out, g.ToDomain(byGroup[g.ID]))
	}
	return out
}
