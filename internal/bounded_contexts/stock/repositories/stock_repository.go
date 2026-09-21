package repositories

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// stockUpsertBatchSize 是批量 upsert 的分片大小。
// 单条 A 股记录约 200 字节，500 条一批的 SQL 文本在 100KB 量级，
// 远低于 max_allowed_packet 默认值，同时把全市场 5000 只股票压到 10 个往返内。
const stockUpsertBatchSize = 500

// StockRepository 持久化 Stock 聚合（MySQL）。
//
// Stock 是本上下文唯一的聚合根，所以也是唯一有仓储的实体；
// Quote / Kline / Financial / News / SocialPost 是读模型值对象，
// 由 MarketDataRepository 统一读写，不各自配仓储。
type StockRepository struct {
	db *gorm.DB
}

func NewStockRepository(db *gorm.DB) *StockRepository {
	return &StockRepository{db: db}
}

func (repo *StockRepository) GetDb() *gorm.DB { return repo.db }

// stockUpsertColumns 是冲突时**无条件**覆盖的列。
// 刻意不含 id 和 (market, symbol)：前者是代理键，后者是业务主键，
// 覆盖它们等于把「更新」变成「改身份」。
//
// 只剩下三列：raw_code 是纯诊断字段；source 必须永远反映最后写入者，
// 否则跨源对账时看不出今天这行是谁给的；updated_at 同理。
// 其余各列的合并策略见 stockOnConflict。
var stockUpsertColumns = []string{"raw_code", "source", "updated_at"}

// stockOnConflict 是 stocks 表的列级合并策略。
//
// ===========================================================================
// 为什么不能再无条件覆盖
// ===========================================================================
//
// 同一只票会被多个源写，而各源给的列并不相同：东财的列表有名称与市值、
// 没有行业/地区/上市日期；Tushare 的 stock_basic 正好相反。
// 无条件覆盖等于「谁最后跑谁说了算」——东财跑完一轮，Tushare 填好的 industry
// 全变成空串，前端的行业筛选第二天就是空的。而且全程没有任何报错：
// 同步会如实报告「成功 5900 条」。
//
// entities.Stock.UpdateProfile 早就写明了规则「只覆盖非空字段」，但批量写入走的是
// DTO → SQL，根本不经过聚合。所以规则必须在这里再表达一次——这不是重复，
// 是把同一条规则补到聚合管不到的那条路径上。
//
// 为什么放在 SQL 里而不是先查后合并：一次全量近 6000 行，读-改-写是 6000 次额外查询；
// 更要命的是两个源并发同步时各自读到的都是过期快照，合并结果取决于谁后写——
// 标准的 TOCTOU。ON DUPLICATE KEY UPDATE 的表达式由 MySQL 在持有行锁时求值，
// 没有这个窗口。
//
// 子句里 VALUES(col) 指「本来要插入的那个值」，裸 col 指「表里已有的值」。
// MySQL 8.0.20 起 VALUES() 被标记为 deprecated，官方替代写法是
// INSERT ... AS new ... ON DUPLICATE KEY UPDATE col = new.col，
// 但 gorm.io/driver/mysql 目前仍然只生成 VALUES() 形式，两种写法不能混用，
// 所以这里跟着 driver 走；等 driver 换掉再统一改这一处。
func stockOnConflict() clause.OnConflict {
	set := clause.AssignmentColumns(stockUpsertColumns)
	set = append(set,
		// 空串的含义是「本次这个源没提供这一列」，不是「这一列真的为空」。
		keepWhenIncomingEmpty("name"),
		keepWhenIncomingEmpty("industry"),
		keepWhenIncomingEmpty("area"),

		// list_date 是可空 DATE，缺失的形态是 SQL NULL 而不是空串，
		// 所以用 COALESCE 而不是上面那套 IF(...='')。
		clause.Assignment{
			Column: clause.Column{Name: "list_date"},
			Value:  gorm.Expr("COALESCE(VALUES(`list_date`), `list_date`)"),
		},

		// 市值：0 表示「本源不提供」，不是「市值为零」。
		//
		// 两列必须由同一个源整体写入，所以共用 total_mv 这一个闸门。
		// 分开判断会拼出「A 源的 total_mv + B 源的 circ_mv」这种行，
		// 两个源口径不同时直接违反聚合守着的 circ_mv <= total_mv——
		// 这行从此再也过不了 entities.List 的校验，可读回来却是「合法」的。
		clause.Assignment{
			Column: clause.Column{Name: "total_mv"},
			Value:  gorm.Expr("IF(VALUES(`total_mv`) = 0, `total_mv`, VALUES(`total_mv`))"),
		},
		clause.Assignment{
			Column: clause.Column{Name: "circ_mv"},
			Value:  gorm.Expr("IF(VALUES(`total_mv`) = 0, `circ_mv`, VALUES(`circ_mv`))"),
		},

		// delisted 只允许单向翻转：同步能把票标成退市，但永远不能撤销退市。
		//
		// 有的源（东财列表）只返回在市股票，它给的 delisted 恒为 false，
		// 而 false 正好是零值——「没报告这只票」和「确认它在市」完全无法区分。
		// 无条件覆盖意味着别的源标过退市的票，它一跑就悄悄复活，
		// 绕过 Relist()；而那条路径存在的全部理由就是「撤销退市是一次需要留痕、
		// 需要发领域事件的决策」，见 entities.Stock.Relist。
		clause.Assignment{
			Column: clause.Column{Name: "delisted"},
			Value:  gorm.Expr("IF(VALUES(`delisted`) = 1, 1, `delisted`)"),
		},
	)
	return clause.OnConflict{
		Columns:   []clause.Column{{Name: "market"}, {Name: "symbol"}},
		DoUpdates: set,
	}
}

// keepWhenIncomingEmpty 生成「传入值为空串则保留原值」的赋值。
// col 只来自本文件的字面量，不经过任何外部输入，因此这里的拼接不是注入面。
func keepWhenIncomingEmpty(col string) clause.Assignment {
	q := "`" + col + "`"
	return clause.Assignment{
		Column: clause.Column{Name: col},
		Value:  gorm.Expr("IF(VALUES(" + q + ") = '', " + q + ", VALUES(" + q + "))"),
	}
}

// Upsert 落一只股票。行情同步任务每天会把全市场重刷一遍，所以写入必须幂等：
// 靠 uk_stocks_market_symbol + ON DUPLICATE KEY UPDATE 实现，
// 而不是「先查有没有再决定 insert 还是 update」——后者在并发同步下必然写重。
func (repo *StockRepository) Upsert(ctx context.Context, s *entities.Stock) error {
	dto := dtos.FromDomainStock(s)
	if err := repo.db.WithContext(ctx).Clauses(stockOnConflict()).Create(dto).Error; err != nil {
		return translateSQL(err, "股票(%s)", s.FullSymbol())
	}
	// MySQL 在 ON DUPLICATE KEY UPDATE 分支不会回填可靠的自增 ID
	// （LAST_INSERT_ID 只在插入时有意义），单条路径上补一次主键查询让调用方拿到完整聚合；
	// 批量路径不做，见 BulkUpsert。
	if dto.ID != 0 {
		s.ID = dto.ID
		return nil
	}
	var id uint64
	err := repo.db.WithContext(ctx).Model(&dtos.StockDto{}).
		Select("id").Where("market = ? AND symbol = ?", dto.Market, dto.Symbol).
		Limit(1).Scan(&id).Error
	if err != nil {
		return translateSQL(err, "股票(%s)", s.FullSymbol())
	}
	s.ID = id
	return nil
}

// BulkUpsert 全量同步的主力路径。
// 这里不回填 ID：为几千条记录各补一次主键查询会把一次同步从秒级拖到分钟级，
// 而同步任务本身并不关心自增 ID。
func (repo *StockRepository) BulkUpsert(ctx context.Context, list []*entities.Stock) error {
	if len(list) == 0 {
		// GORM 对空切片会返回 ErrEmptySlice，空输入在同步场景是常态（当天无新增），不该报错。
		return nil
	}
	rows := make([]*dtos.StockDto, 0, len(list))
	for _, s := range list {
		if s == nil {
			continue
		}
		rows = append(rows, dtos.FromDomainStock(s))
	}
	if len(rows) == 0 {
		return nil
	}
	err := repo.db.WithContext(ctx).Clauses(stockOnConflict()).
		CreateInBatches(rows, stockUpsertBatchSize).Error
	if err != nil {
		return translateSQL(err, "股票批量写入(%d 条)", len(rows))
	}
	return nil
}

func (repo *StockRepository) FindByCode(ctx context.Context, code shared_vo.StockCode) (*entities.Stock, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	var dto dtos.StockDto
	err := repo.db.WithContext(ctx).
		Where("market = ? AND symbol = ?", string(code.Market), code.Symbol).
		First(&dto).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("股票不存在: %s", code.FullSymbol())
		}
		return nil, translateSQL(err, "股票(%s)", code.FullSymbol())
	}
	return dto.ToDomain(), nil
}

// FindByCodes 一次查回一批标的。
//
// 存在的唯一理由是让上层处理一篮子标的时不必在 for 里逐个 FindByCode——
// 那是 N 次往返，而这里是一次。
//
// 用 (market, symbol) 的 IN 元组而不是把 codes 拆成 OR 链：元组 IN 能命中
// uk_stocks_market_symbol，OR 链在跨市场时会退化成全表扫。
// 查不到的代码不报错，由调用方比对入参自行决定怎么补齐（通常是 read-through）。
func (repo *StockRepository) FindByCodes(ctx context.Context, codes []shared_vo.StockCode) ([]*entities.Stock, error) {
	pairs := make([][]any, 0, len(codes))
	for _, c := range codes {
		if c.IsZero() {
			continue
		}
		pairs = append(pairs, []any{string(c.Market), c.Symbol})
	}
	if len(pairs) == 0 {
		return []*entities.Stock{}, nil
	}
	var rows []dtos.StockDto
	if err := repo.db.WithContext(ctx).Where("(market, symbol) IN ?", pairs).Find(&rows).Error; err != nil {
		return nil, translateSQL(err, "股票批量查询(%d 个代码)", len(pairs))
	}
	return dtos.ToDomainStocks(rows), nil
}

// Search 按代码前缀 / 名称 / 行业模糊匹配。
// symbol 用前缀匹配（"6005%"）是有意为之：代码是左对齐的定长串，前缀查询能走 uk 索引；
// 名称与行业才用双侧通配，因为用户输入的是「茅台」这样的中缀词。
func (repo *StockRepository) Search(ctx context.Context, keyword string, market shared_vo.Market, page shared_vo.Page) ([]*entities.Stock, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.StockDto{})
	if market.Valid() {
		q = q.Where("market = ?", string(market))
	}
	if kw := strings.TrimSpace(keyword); kw != "" {
		escaped := escapeLike(kw)
		q = q.Where("symbol LIKE ? OR name LIKE ? OR industry LIKE ?",
			escaped+"%", "%"+escaped+"%", "%"+escaped+"%")
	}
	return repo.pageQuery(q, page)
}

func (repo *StockRepository) ListByMarket(ctx context.Context, market shared_vo.Market, page shared_vo.Page) ([]*entities.Stock, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.StockDto{})
	if market.Valid() {
		q = q.Where("market = ?", string(market))
	}
	return repo.pageQuery(q, page)
}

// Industries 返回行业字典，供前端筛选框使用。
// DISTINCT 直接走 idx_stocks_industry 的松散索引扫描，不需要额外维护一张行业表。
func (repo *StockRepository) Industries(ctx context.Context, market shared_vo.Market) ([]string, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.StockDto{}).
		Distinct("industry").Where("industry <> ''")
	if market.Valid() {
		q = q.Where("market = ?", string(market))
	}
	var out []string
	if err := q.Order("industry ASC").Pluck("industry", &out).Error; err != nil {
		return nil, translateSQL(err, "行业列表")
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// CountByMarket 统计某市场已入库的标的数量，上层用它判断是否需要触发首次全量同步。
func (repo *StockRepository) CountByMarket(ctx context.Context, market shared_vo.Market) (int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.StockDto{})
	if market.Valid() {
		q = q.Where("market = ?", string(market))
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, translateSQL(err, "股票数量统计")
	}
	return n, nil
}

// DelistAndCount 在一个事务里落库退市并统计该市场剩余在市数量。
//
// `delisted = false` 是 WHERE 谓词而不是先查后写：两个请求同时退市同一只票时，
// 内存里的状态判断都会通过，只有带这个条件的 UPDATE 能分出胜负，
// 后到的那条影响 0 行，于是被翻译成 Conflict 而不是静默重写 updated_at。
// 检查与动作合并成同一条语句，这正是 TOCTOU 的唯一正解。
//
// 「退市后该市场是否已空」也必须在同一事务内统计才有意义，
// 因此做成一个方法而不是让上层拼两步——上层不允许持有事务。
func (repo *StockRepository) DelistAndCount(ctx context.Context, s *entities.Stock) (remainingListed int64, err error) {
	dto := dtos.FromDomainStock(s)
	var remaining int64
	err = repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&dtos.StockDto{}).
			Where("market = ? AND symbol = ? AND delisted = ?", dto.Market, dto.Symbol, false).
			Updates(map[string]any{"delisted": true, "updated_at": dto.UpdatedAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return resolveTransitionFailure(tx, dto.Market, dto.Symbol, s.FullSymbol(), false)
		}
		return tx.Model(&dtos.StockDto{}).
			Where("market = ? AND delisted = ?", dto.Market, false).
			Count(&remaining).Error
	})
	if err != nil {
		// 事务内构造的领域错误要原样透出，不能被再包一层 Internal，
		// 否则 NotFound 会在 HTTP 层变成 500。
		var de *custom_errors.Error
		if errors.As(err, &de) {
			return 0, de
		}
		return 0, translateSQL(err, "股票退市(%s)", s.FullSymbol())
	}
	return remaining, nil
}

// Relist 撤销退市。
//
// 与 DelistAndCount 同构：`delisted = true` 进 WHERE，把「当前确实处于退市状态」
// 这个前置条件和写入合并成一条语句。之前的实现是 FindByCode → 聚合内判断 → Upsert，
// 两个并发的撤销请求会双双通过判断并各写一次，是标准的 TOCTOU。
func (repo *StockRepository) Relist(ctx context.Context, s *entities.Stock) error {
	dto := dtos.FromDomainStock(s)
	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&dtos.StockDto{}).
			Where("market = ? AND symbol = ? AND delisted = ?", dto.Market, dto.Symbol, true).
			Updates(map[string]any{"delisted": false, "updated_at": dto.UpdatedAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return resolveTransitionFailure(tx, dto.Market, dto.Symbol, s.FullSymbol(), true)
		}
		return nil
	})
	if err != nil {
		var de *custom_errors.Error
		if errors.As(err, &de) {
			return de
		}
		return translateSQL(err, "股票恢复上市(%s)", s.FullSymbol())
	}
	return nil
}

// resolveTransitionFailure 解释「影响 0 行」到底是哪个前置条件没满足。
//
// 影响 0 行本身是二义的：行不存在，或者状态已经是目标态。多花一次事务内的读
// 换一个可执行的错误消息——这条路径只在失败时走到，正常路径仍是单条语句。
// expectedDelisted 是 UPDATE 谓词要求的原状态。
func resolveTransitionFailure(tx *gorm.DB, market, symbol, fullSymbol string, expectedDelisted bool) error {
	var current dtos.StockDto
	if err := tx.Where("market = ? AND symbol = ?", market, symbol).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return custom_errors.NotFound("股票不存在: %s", fullSymbol)
		}
		return err
	}
	if expectedDelisted {
		return custom_errors.Conflict("股票 %s 未处于退市状态", fullSymbol)
	}
	return custom_errors.Conflict("股票 %s 已处于退市状态", fullSymbol)
}

// pageQuery 抽掉 Search / ListByMarket 重复的「先 count 再取页」逻辑。
// 排序固定按 (market, symbol)，与唯一索引同序，翻页时不需要额外 filesort。
func (repo *StockRepository) pageQuery(q *gorm.DB, page shared_vo.Page) ([]*entities.Stock, int64, error) {
	// Session 把链式条件固化成可复用的基准查询。GORM 的 *gorm.DB 在执行过 Count
	// 这类终结方法后再复用会串条件，这是官方给的正确复用姿势。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translateSQL(err, "股票列表")
	}
	if total == 0 {
		return []*entities.Stock{}, 0, nil
	}
	var rows []dtos.StockDto
	err := q.Order("market ASC, symbol ASC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translateSQL(err, "股票列表")
	}
	return dtos.ToDomainStocks(rows), total, nil
}
