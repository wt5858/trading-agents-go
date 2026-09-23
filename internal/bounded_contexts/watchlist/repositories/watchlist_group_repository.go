package repositories

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// WatchlistGroupRepository 持久化 WatchlistGroup 聚合——**根和它的全部子实体**。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
//
// 它碰两张表（watchlist_groups 和 watchlist_items），这与「一个仓储只碰一张表」
// 的常见说法并不冲突：真正的规则是**一个仓储只服务一个聚合**。
// 这两张表属于同一个聚合，它们之间的一致性正是这个聚合存在的理由，
// 因此它们必须在同一个事务里写——反过来，本仓储也绝不会去碰任何别的聚合的表。
type WatchlistGroupRepository struct {
	db *gorm.DB
}

func NewWatchlistGroupRepository(db *gorm.DB) *WatchlistGroupRepository {
	return &WatchlistGroupRepository{db: db}
}

// ---------------------------------------------------------------------------
// 写入：整聚合保存
// ---------------------------------------------------------------------------

// Save 保存整个聚合：根 + 全部子实体，一个事务。
//
// ===========================================================================
// 本方法是「聚合根 ↔ 子实体」规则的后半场，值得完整解释
// ===========================================================================
//
// # 契约：调用方只交出根
//
// 签名里只有 *entities.WatchlistGroup。没有 SaveItem，没有 AddItem(ctx, groupID, ...)，
// 没有 DeleteItem——子实体级别的持久化 API 在本层压根不存在。
// 调用方永远是「把整个聚合交出去」，至于其中哪一条要 INSERT、哪一条要 UPDATE、
// 哪一条已经被移除要 DELETE，是本方法的**实现细节**，对上层完全不可见。
//
// 这条契约是有代价的（下面那一大段 diff），但它买到的是：上层永远不可能写出
// 「只更新了一条自选项，却没同步分组的 updated_at」或者「删了自选项忘了重排序号」
// 这类破坏聚合一致性的代码——因为上层根本没有那样的 API 可用。
//
// # 子实体 diff 怎么做
//
// 事务里先把库中现存的子实体行捞出来（带 FOR UPDATE 行锁），与内存中的
// g.Items 按主键对齐，分成三组：
//
//	新增：内存里 ID == 0 的（只可能由 AddItem 产生）
//	更新：ID 能对上、但可变列不同的（备注改了、排序变了、参考价搬家了）
//	删除：库里有、内存里没有的（RemoveItem 移走的）
//
// 三组各发**一条**语句（批量 DELETE / 批量 INSERT / 批量 upsert），
// 加上开头的那次 SELECT 和根自身的一条语句，整个 Save 是**常数条 SQL**，
// 与自选股数量无关。绝不逐条 UPDATE——重排一次 200 只票的分组就是 200 条语句。
//
// # 为什么先写根、后写子实体
//
// 两个原因，都不是随便排的：
//   - 新建分组时，子实体的 group_id 要等根的自增主键回填之后才知道；
//   - 并发时，两个 Save 都先在根那一行上排队（INSERT 或 UPDATE 都会锁住它），
//     于是它们取子实体行锁的顺序天然一致，不存在交叉加锁的死锁。
//
// # 为什么没有乐观锁版本号
//
// 因为 diff 本身就是一种冲突检测，而且是语义更好的那一种：并发地往同一个分组里
// 各加一只票，两次 Save 会各自 INSERT 一行，结果是两只票都在——这正是用户期望的合并。
// 用版本号的话，后到的那次会被判失败，用户会看到「请重试」而他其实什么都没做错。
// 真正互相冲突的情况（一边删、另一边改同一条）会在 diff 里被识别成
// 「内存里有一条 id 在库里已经不见了」，那时才抛 Conflict。
func (repo *WatchlistGroupRepository) Save(ctx context.Context, g *entities.WatchlistGroup) error {
	if g == nil {
		return custom_errors.Invalid("待保存的自选分组为空")
	}
	// 聚合自检放在最前面：它能挡住「绕过根塞进来的子实体」和排序断裂这类
	// 已经在内存里破损的状态。一旦写进库，破损就变成了后续每次加载都会接受的既成事实。
	if err := g.Validate(); err != nil {
		return err
	}

	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		isNew := g.ID == 0
		if err := repo.saveRoot(tx, g, isNew); err != nil {
			return err
		}
		return repo.saveChildren(tx, g, isNew)
	})
	if err != nil {
		return repo.translateSave(err, g)
	}
	return nil
}

// saveRoot 写根那一行。
func (repo *WatchlistGroupRepository) saveRoot(tx *gorm.DB, g *entities.WatchlistGroup, isNew bool) error {
	dto := dtos.FromDomainGroup(g)
	if isNew {
		if err := tx.Create(dto).Error; err != nil {
			return err
		}
		// 回填必须经由根：它要同时刷到根和全部子实体的 group_id 上。
		g.AssignPersistedID(dto.ID)
		return nil
	}

	// user_id 进 WHERE 而不是只用 id：这样「改别人的分组」在数据层就是 0 行，
	// 上层拿到的是 NotFound 而不是一次成功的越权写入。归属判定在领域服务里
	// 已经做过一次，这里是纵深防御——真正的并发保证只有写在语句里才成立。
	res := tx.Model(&dtos.WatchlistGroupDto{}).
		Where("id = ? AND user_id = ?", dto.ID, dto.UserID).
		Updates(map[string]any{
			"name":       dto.Name,
			"updated_at": dto.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// 0 行有歧义：行可能不存在（或不属于这个用户），也可能是值压根没变
	// （只改了子实体的备注，而那次修改把 updated_at 刷成了同一毫秒）。
	// 只在这一条分支上多查一次，正常路径仍然只有一条语句。
	return repo.assertGroupExists(tx, dto.ID, dto.UserID)
}

// saveChildren 是子实体的三路 diff，也就是「整聚合保存」的全部实现细节。
func (repo *WatchlistGroupRepository) saveChildren(tx *gorm.DB, g *entities.WatchlistGroup, isNew bool) error {
	var existing []dtos.WatchlistItemDto
	if !isNew {
		// FOR UPDATE 锁住本分组现存的子实体行。
		//
		// 「先 SELECT 出来，再据此决定删哪几行」如果不加锁就是 TOCTOU：
		// 两个并发的 Save 会看到同一份现状，各自算出一份 diff，后提交的那份
		// 会把对方刚插入的行当成「库里多出来的」而删掉。行锁让第二个事务
		// 必须等第一个提交后再读，从而读到的是真正的当前状态。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("group_id = ?", g.ID).
			Find(&existing).Error
		if err != nil {
			return err
		}
	}

	existingByID := make(map[uint64]dtos.WatchlistItemDto, len(existing))
	for _, row := range existing {
		existingByID[row.ID] = row
	}

	// 已落库的那部分：对齐主键，只挑出可变列真的变了的行。
	desired := dtos.FromDomainItems(g.ID, g.Items)
	keep := make(map[uint64]struct{}, len(desired))
	toUpdate := make([]*dtos.WatchlistItemDto, 0, len(desired))
	for _, d := range desired {
		if d.ID == 0 {
			continue // 新增的走下面 PendingItems 那条路
		}
		current, ok := existingByID[d.ID]
		if !ok {
			// 内存里有一条自认为已落库的子实体，库里却没有。
			// 只可能是别人在我们持有这份聚合期间把它删掉了（或者把整组删了）。
			// 继续写下去会凭空复活一条用户已经删掉的记录，必须让调用方重来。
			return custom_errors.Conflict(
				"自选分组(id=%d) 已被并发修改（%s 已不在组内），请重新加载后重试",
				g.ID, d.Symbol)
		}
		keep[d.ID] = struct{}{}
		if !d.SameAs(current) {
			toUpdate = append(toUpdate, d)
		}
	}

	// 删除：库里有、内存里没有的。遍历 existing 切片而不是 map，
	// 是为了让生成的 IN 列表顺序稳定，慢查询日志与线上复现才对得上。
	toDelete := make([]uint64, 0, len(existing))
	for _, row := range existing {
		if _, ok := keep[row.ID]; !ok {
			toDelete = append(toDelete, row.ID)
		}
	}

	// 顺序要紧：DELETE 必须先于 INSERT。
	// 「把某只票移出去、又在同一次操作里加回来」会产生一条待删的旧行和一条待插的新行，
	// 它们的 (group_id, market, symbol) 相同，先插就会撞上唯一索引。
	if len(toDelete) > 0 {
		err := tx.Where("group_id = ? AND id IN ?", g.ID, toDelete).
			Delete(&dtos.WatchlistItemDto{}).Error
		if err != nil {
			return err
		}
	}

	if pending := g.PendingItems(); len(pending) > 0 {
		rows := dtos.FromDomainItems(g.ID, pending)
		// 一条多值 INSERT，不是 len(pending) 条。
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		ids := make([]uint64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		// 主键回填同样经由根：子实体的任何字段都不由本层直接写。
		if err := g.AssignPersistedItemIDs(ids); err != nil {
			return err
		}
	}

	if len(toUpdate) > 0 {
		// 一条 INSERT ... ON DUPLICATE KEY UPDATE 覆盖全部改动行。
		//
		// 为什么不是 for 里逐行 UPDATE：每行的 sort_order 和 note 都不同，
		// 没法合并成一条带统一 SET 的语句；而重排一次 200 只票的分组
		// 恰恰会让每一行的 sort_order 都变化——那就是 200 条往返。
		// upsert 的批量形式是在保留「逐行不同的值」的前提下把它压成一条语句的办法。
		//
		// 这些行的主键都已存在（上面刚核对过），因此必然走 UPDATE 分支，
		// 不会凭空插入。冲突列只认主键，业务唯一键的冲突仍然要如实报错。
		err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"note", "ref_price", "ref_trade_date", "ref_price_at", "sort_order", "updated_at",
			}),
		}).Create(&toUpdate).Error
		if err != nil {
			return err
		}
	}
	return nil
}

// Delete 删除整个聚合：分组连同它的全部自选项，一个事务。
//
// 「删分组顺带删自选项」不是级联，而是这个聚合的定义——子实体的生命周期
// 完全从属于根，根没了它们就不该存在。所以它必须和根的删除在同一个事务里，
// 否则会留下一堆 group_id 指向空气的孤儿行，而且它们还占着唯一索引的位置。
//
// # 为什么不用外键的 ON DELETE CASCADE
//
// 数据库级联确实能做这件事，但它是**看不见**的：读这段代码的人不会知道
// 还有一张表被清空了，排查数据丢失时也不会想到去翻建表语句。
// 既然本层已经持有事务，把这一步写成一条显式语句成本为零，收益是它出现在代码里。
// 顺带的好处是插入自选项时不必付外键检查的开销。
//
// userID 进 WHERE：越权删除在数据层就是 0 行，上层因此拿到 NotFound 而不是成功。
func (repo *WatchlistGroupRepository) Delete(ctx context.Context, id, userID uint64) error {
	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先删根：它顺便充当了存在性与归属的判定。判定不成立时整个事务回滚，
		// 子实体那条 DELETE 压根不会执行。
		res := tx.Where("id = ? AND user_id = ?", id, userID).
			Delete(&dtos.WatchlistGroupDto{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 不存在与不属于你，对外是同一个答案：返回 Forbidden 等于确认
			// 「这个 id 确实存在」，会让分组 ID 空间变成可探测的信道。
			return custom_errors.NotFound("自选分组(id=%d) 不存在", id)
		}
		return tx.Where("group_id = ?", id).Delete(&dtos.WatchlistItemDto{}).Error
	})
	if err != nil {
		return translatef(err, "自选分组(id=%d)", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读取：加载整聚合
// ---------------------------------------------------------------------------

// FindByID 加载完整聚合：根 + 它的全部子实体。
//
// **两条查询**，与自选股数量无关：一条取分组，一条取它的自选项。
// 绝不是「先取分组，再对每只票查一次」——那是 N+1，一个 200 只票的分组
// 就是 201 次往返。
//
// 没有「只加载根、不加载子实体」的重载，这是刻意的：那样的半个聚合一旦流到
// Save 手上，会被如实理解为「用户把这一组清空了」，一次读取变成一次删除。
// 这个聚合的边界（MaxItemsPerGroup = 200）本来就是为了让「总是全量加载」
// 成为一个常数级操作而存在的。
//
// 两条查询不包在事务里：这是只读路径，最坏情况是渲染出一份晚了一个操作的列表，
// 而为一次列表查询付出事务与快照的代价并不划算。真正需要一致视图的是写路径，
// Save 会在它自己的事务里带锁重新读一次子实体。
func (repo *WatchlistGroupRepository) FindByID(ctx context.Context, id uint64) (*entities.WatchlistGroup, error) {
	var groupRow dtos.WatchlistGroupDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&groupRow).Error; err != nil {
		return nil, translatef(err, "自选分组(id=%d)", id)
	}

	itemRows, err := repo.loadItems(ctx, []uint64{id})
	if err != nil {
		return nil, err
	}
	return groupRow.ToDomain(itemRows), nil
}

// ListByUser 列出用户的全部分组，每个都带齐子实体。
//
// # 它为什么不是 N+1
//
// 天真的写法是「查出分组列表，然后对每个分组调一次 FindByID」，那是
// 1 + 分组数 次查询。这里改成：一条查询取全部分组，拿到它们的 id 之后
// 用一条 WHERE group_id IN (...) 取回**所有**分组的全部自选项，
// 最后在内存里按 group_id 缝回去。总共两条查询，与分组数无关。
//
// # 为什么不分页
//
// 一个用户的分组是个位数到几十个的量级（它是盯盘用的 tab，不是数据表），
// 而每个分组的自选项有 MaxItemsPerGroup 兜底。总量有界，分页只会让
// 「取回全部分组的自选项」这一步的缝合逻辑凭空复杂一倍。
// 真正需要翻页的是分组**内部**的自选项列表，那属于另一条读路径。
func (repo *WatchlistGroupRepository) ListByUser(ctx context.Context, userID uint64) ([]*entities.WatchlistGroup, error) {
	var groupRows []dtos.WatchlistGroupDto
	err := repo.db.WithContext(ctx).
		Where("user_id = ?", userID).
		// 走 uk_watchlist_groups_user_name 的 user_id 前缀定位；
		// 按创建时间排序让分组 tab 的顺序稳定，补一个 id 做 tie-break，
		// 否则同一毫秒建出的两个分组每次刷新的先后可能不同。
		Order("created_at ASC, id ASC").
		Find(&groupRows).Error
	if err != nil {
		return nil, translatef(err, "用户(id=%d) 的自选分组列表", userID)
	}
	if len(groupRows) == 0 {
		return []*entities.WatchlistGroup{}, nil
	}

	ids := make([]uint64, 0, len(groupRows))
	for _, row := range groupRows {
		ids = append(ids, row.ID)
	}
	itemRows, err := repo.loadItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	return dtos.ToDomainGroups(groupRows, itemRows), nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// loadItems 一次取回若干分组的全部自选项，是「不出现 N+1」的那条查询。
//
// 排序交给数据库而不是内存：idx_watchlist_items_group_sort 正是
// (group_id, sort_order)，这个 ORDER BY 直接吃索引，不需要额外的排序开销。
// 补 id 做 tie-break 是为了兜住历史上可能存在的重号数据，让顺序至少是确定的。
func (repo *WatchlistGroupRepository) loadItems(ctx context.Context, groupIDs []uint64) ([]dtos.WatchlistItemDto, error) {
	var rows []dtos.WatchlistItemDto
	err := repo.db.WithContext(ctx).
		Where("group_id IN ?", groupIDs).
		Order("group_id ASC, sort_order ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "自选分组(共 %d 个) 的自选股", len(groupIDs))
	}
	return rows, nil
}

// assertGroupExists 把一次条件 UPDATE 的「0 行」收敛成一个确定的答案。
func (repo *WatchlistGroupRepository) assertGroupExists(tx *gorm.DB, id, userID uint64) error {
	var n int64
	err := tx.Model(&dtos.WatchlistGroupDto{}).
		Where("id = ? AND user_id = ?", id, userID).Count(&n).Error
	if err != nil {
		return err
	}
	if n == 0 {
		return custom_errors.NotFound("自选分组(id=%d) 不存在", id)
	}
	// 行在、值没变：当成成功。重复提交同一份表单不该是一个错误。
	return nil
}

// translateSave 把事务里冒出来的技术错误翻译成对用户有意义的话。
//
// 唯一键冲突在这里分两种，它们对应两条完全不同的不变式，也对应两句
// 完全不同的提示。分辨的依据是索引名（见 violatedIndex）。
//
// 注意这里**没有**「先查有没有重名再插」：那是 TOCTOU。两个并发的创建请求
// 会双双查到「不重名」，然后双双插入。唯一索引才是这条不变式的真正保证，
// 本方法只负责把它的失败翻译成人话。
func (repo *WatchlistGroupRepository) translateSave(err error, g *entities.WatchlistGroup) error {
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de
	}
	if isDuplicateKey(err) {
		switch violatedIndex(err) {
		case idxGroupUserName:
			return custom_errors.AlreadyExists("已存在同名的自选分组「%s」", g.Name.String()).Wrap(err)
		case idxItemGroupCode:
			return custom_errors.AlreadyExists("分组「%s」中已存在该股票", g.Name.String()).Wrap(err)
		}
	}
	return translatef(err, "自选分组「%s」", g.Name.String())
}
