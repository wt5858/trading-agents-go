package repositories

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// PaperAccountRepository 持久化 PaperAccount 聚合。
//
// # 它的写入入口只有「整个聚合」这一个粒度
//
// 调用方交出来的永远是根：Save(ctx, account)。持仓怎么 diff、成交怎么追加，
// 全是本仓储的内部细节。刻意不提供 SavePosition / AppendTrade 这类方法——
// 一旦提供，「买入扣的现金」和「建仓增加的成本」就可能分两次提交，
// 中间崩一次账本就永久对不平了。子实体没有仓储，是这条保证的前提。
type PaperAccountRepository struct {
	db *gorm.DB
}

func NewPaperAccountRepository(db *gorm.DB) *PaperAccountRepository {
	return &PaperAccountRepository{db: db}
}

func (repo *PaperAccountRepository) GetDb() *gorm.DB { return repo.db }

// Create 开户落库。
//
// 新账户没有持仓也没有成交，所以这里是一条语句就够了，不需要事务：
// 事务的意义是把多次写入捆成一个原子操作，单条语句本身就是原子的。
func (repo *PaperAccountRepository) Create(ctx context.Context, a *entities.PaperAccount) error {
	if err := repo.db.WithContext(ctx).Create(dtos.FromDomainAccount(a)).Error; err != nil {
		return translatef(err, "模拟账户(id=%s)", a.ID)
	}
	return nil
}

// FindByID 加载单个聚合：账户一次查询、持仓一次查询，固定两条语句。
func (repo *PaperAccountRepository) FindByID(ctx context.Context, id string) (*entities.PaperAccount, error) {
	var row dtos.PaperAccountDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return nil, translatef(err, "模拟账户(id=%s)", id)
	}

	positions, err := repo.loadPositions(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	accounts := dtos.ToDomainAccountsWithPositions([]*dtos.PaperAccountDto{&row}, positions)
	return accounts[0], nil
}

// FindByUser 加载某用户的全部模拟账户。
//
// # 为什么是两条语句而不是「一个账户一次持仓查询」
//
// 在循环里逐个账户查持仓就是 N+1：三个账户三次往返，看着无害，
// 但这个写法一旦被复制到「按持仓给一批账户估值」的路径上就是几十次往返。
// 这里先一次取回全部账户，再用一条 IN 查询取回它们的全部持仓，
// 最后在内存里按 account_id 分组装配——语句数与账户数无关。
func (repo *PaperAccountRepository) FindByUser(ctx context.Context, userID uint64) ([]*entities.PaperAccount, error) {
	var rows []*dtos.PaperAccountDto
	err := repo.db.WithContext(ctx).Model(&dtos.PaperAccountDto{}).
		Where("user_id = ?", userID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "用户(id=%d) 模拟账户列表", userID)
	}
	if len(rows) == 0 {
		return []*entities.PaperAccount{}, nil
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	positions, err := repo.loadPositions(ctx, ids)
	if err != nil {
		return nil, err
	}
	return dtos.ToDomainAccountsWithPositions(rows, positions), nil
}

// Save 把整个聚合的变更写回，全部在一个事务里。
//
// # 事务里发生的四件事，缺一不可
//
//  1. 带乐观锁地更新账户（现金、已实现盈亏、版本号）；
//  2. 删除已经被清仓、不再属于聚合的持仓行；
//  3. upsert 当前持仓；
//  4. 追加本次工作单元产生的成交记录。
//
// 它们必须同生共死。假设只提交了 1 和 4 而 3 失败：现金扣了、成交记录也写了，
// 但持仓没建上——用户付了钱却没拿到股票，而且没有任何补偿路径能推断出
// 到底该补多少。所以这里是一个事务，而不是四次调用加一段重试逻辑。
//
// # 乐观锁
//
// WHERE id = ? AND version = ? 把「我读到的版本仍然是当前版本」这个前提
// 写进了 UPDATE 本身，检查与写入因此是一个原子操作。没有它的话，
// 同一账户上两笔并发买入会各自读到同一个现金余额、各自减去自己的金额、
// 后写的覆盖先写的——账户凭空多出一笔钱，而两条成交记录都在，账本永久对不平。
// 命中 0 行说明有人抢先提交过，返回 Conflict，由领域服务重新加载后重试。
//
// # 成交记录何时从聚合上摘掉
//
// 只有事务提交成功之后才调用 TakeNewTrades。在事务内部就排空的话，
// 一次冲突回滚会把这批成交从内存里也弄丢，重试时就只剩一个改了现金
// 却没有凭证的聚合。
func (repo *PaperAccountRepository) Save(ctx context.Context, a *entities.PaperAccount) error {
	if a == nil {
		return custom_errors.Invalid("待保存的模拟账户为空")
	}
	accountDto := dtos.FromDomainAccount(a)
	// 不排空，只读引用；提交成功后再排空。
	newTrades := a.NewTrades

	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// --- 1. 乐观锁更新账户 ---
		res := tx.Model(&dtos.PaperAccountDto{}).
			Where("id = ? AND version = ?", accountDto.ID, accountDto.Version).
			Updates(map[string]any{
				"name":         accountDto.Name,
				"cash":         accountDto.Cash,
				"realized_pnl": accountDto.RealizedPnl,
				"total_fee":    accountDto.TotalFee,
				"version":      accountDto.Version + 1,
				"updated_at":   accountDto.UpdatedAt,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 0 行有两种可能，在同一个事务里读一次把它们区分开，
			// 调用方才知道是该重试（并发冲突）还是该放弃（账户不存在）。
			var n int64
			if err := tx.Model(&dtos.PaperAccountDto{}).
				Where("id = ?", accountDto.ID).Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				return custom_errors.NotFound("模拟账户(id=%s) 不存在", accountDto.ID)
			}
			return custom_errors.Conflict("模拟账户(id=%s) 已被并发修改，请重试", accountDto.ID)
		}

		// --- 2. 删除不再属于聚合的持仓 ---
		//
		// 用一条 NOT IN 而不是「先查出库里有哪些、再逐个比对删除」：
		// 后者既多一次往返，又要在应用层维护一份差集，而差集算错就会留下幽灵持仓。
		// 聚合内存里的持仓集合就是权威，库里多出来的一律删掉。
		symbols := make([]string, 0, len(a.Positions))
		for _, p := range a.Positions {
			symbols = append(symbols, p.Symbol())
		}
		del := tx.Where("account_id = ?", accountDto.ID)
		if len(symbols) > 0 {
			del = del.Where("symbol NOT IN ?", symbols)
		}
		if err := del.Delete(&dtos.PaperPositionDto{}).Error; err != nil {
			return err
		}

		// --- 3. 批量 upsert 持仓 ---
		//
		// 一条多值 INSERT ... ON DUPLICATE KEY UPDATE，不在 for 里逐条写。
		// 冲突目标是复合主键 (account_id, symbol)，也就是「一个账户一只票一行」
		// 这条不变式的执行者。
		if len(a.Positions) > 0 {
			rows := dtos.FromDomainPositions(accountDto.ID, a.Positions)
			err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "account_id"}, {Name: "symbol"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"market", "symbol_raw", "quantity", "avg_cost", "cost_basis", "updated_at",
				}),
			}).Create(&rows).Error
			if err != nil {
				return err
			}
		}

		// --- 4. 追加成交记录 ---
		//
		// ON CONFLICT DO NOTHING：成交 ID 由聚合生成且稳定，整笔操作被重试时
		// 重复写入应当是无害的 no-op，而不是一个需要调用方特判的唯一键冲突。
		if len(newTrades) > 0 {
			rows := dtos.FromDomainTrades(newTrades)
			err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoNothing: true,
			}).Create(&rows).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return translatef(err, "模拟账户(id=%s)", a.ID)
	}

	// 提交成功，把聚合推进到与库里一致的状态，调用方可以继续复用它。
	a.Version = accountDto.Version + 1
	a.TakeNewTrades()
	return nil
}

// ListTrades 分页返回成交历史，返回的是**值对象**而不是实体。
//
// 成交历史只追加、无上限增长、没有不变式要守，它是读模型。
// 让它以实体形式经由聚合返回，意味着看一页历史要先把整个账户加载进内存，
// 而且加载成本随历史长度线性增长。读路径直接 DTO → VO 是这条规则明确允许的。
//
// 分页是强制的，不提供「全部拉回」的入口：那个入口一旦存在，
// 迟早有人在一个跑了一年的账户上调用它。
func (repo *PaperAccountRepository) ListTrades(
	ctx context.Context,
	accountID string,
	page shared_vo.Page,
) ([]value_objects.TradeRecord, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.PaperTradeDto{}).
		Where("account_id = ?", accountID).Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "模拟账户(id=%s) 成交历史", accountID)
	}
	if total == 0 {
		return []value_objects.TradeRecord{}, 0, nil
	}

	var rows []*dtos.PaperTradeDto
	// (account_id, traded_at) 索引让这次排序直接走索引。id 作为次级排序键
	// 消解同毫秒成交的顺序抖动，否则翻页时同一条记录可能出现在两页上。
	err := q.Order("traded_at DESC, id DESC").
		Offset(page.Offset()).Limit(page.Limit()).
		Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "模拟账户(id=%s) 成交历史", accountID)
	}
	return dtos.ToDomainTradeRecords(rows), total, nil
}

// CountByUser 统计用户已开的模拟账户数，供领域服务做开户数量限制。
func (repo *PaperAccountRepository) CountByUser(ctx context.Context, userID uint64) (int64, error) {
	var n int64
	err := repo.db.WithContext(ctx).Model(&dtos.PaperAccountDto{}).
		Where("user_id = ?", userID).Count(&n).Error
	if err != nil {
		return 0, translatef(err, "用户(id=%d) 模拟账户数量", userID)
	}
	return n, nil
}

// loadPositions 一次取回一批账户的全部持仓。
// 入参是账户 ID 切片而不是单个 ID，正是为了让调用点没有「在循环里调它」的动机。
func (repo *PaperAccountRepository) loadPositions(ctx context.Context, accountIDs []string) ([]*dtos.PaperPositionDto, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	var rows []*dtos.PaperPositionDto
	err := repo.db.WithContext(ctx).Model(&dtos.PaperPositionDto{}).
		Where("account_id IN ?", accountIDs).
		Order("account_id ASC, symbol ASC").
		Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "模拟账户持仓(%d 个账户)", len(accountIDs))
	}
	return rows, nil
}

// IsConflict 让领域服务不必去 errors.As 一个具体类型就能判断「要不要重试」。
func IsConflict(err error) bool {
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de.Code == custom_errors.CodeConflict
	}
	return false
}
