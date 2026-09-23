package repositories

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories/dtos"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// collIndicators 是技术指标快照集合。集中声明避免各处硬编码：
// mongo 查一个不存在的集合不报错，只返回空，拼错了根本查不出来。
const collIndicators = "agent_indicators"

// IndicatorRepository 读写技术指标快照，是 IndicatorSnapshot 聚合根的仓储。
//
// # 它为什么存在
//
// 技术指标整体是一组用乘除从 K 线推导出来的派生量。派生量必须落库、读回，
// 不允许在读路径上重算——理由写在 value_objects/indicators.go 与
// entities/indicator_snapshot.go 里。这个仓储就是那条规则的执行机构：
// 计算只发生在数据准备阶段并立刻 Save，之后全系统所有读路径
// （工具 get_technical_indicators、提示词渲染、失败重跑）都走 Find。
//
// 仓储只配给聚合根：本上下文只有 IndicatorSnapshot 一个需要持久化的聚合，
// AnalysisContext 是一次运行期的共享状态，随进程结束而消失，不配仓储。
type IndicatorRepository struct {
	db *mongo.Database
}

func NewIndicatorRepository(db *mongo.Database) *IndicatorRepository {
	return &IndicatorRepository{db: db}
}

// EnsureIndexes 幂等建索引，可在每次启动时调用。
//
// 唯一索引建在自然键 (symbol, period, trade_date) 上，它承担两个职责：
//  1. 幂等性兜底——所有写入都是 upsert，没有唯一索引时并发的两次写会插出两条文档，
//     之后 Find 取到哪一条全看运气；
//  2. upsert 的定位手段——没有它，每次 upsert 都要全集合扫描。
//
// trade_date 降序与 stock 上下文的 klines 索引保持同一形态，
// 让「某只票最近 N 天的指标」这类查询能等值过滤 + 倒序读索引，省掉排序阶段。
func (repo *IndicatorRepository) EnsureIndexes(ctx context.Context) error {
	model := mongo.IndexModel{
		Keys: bson.D{
			{Key: "symbol", Value: 1},
			{Key: "period", Value: 1},
			{Key: "trade_date", Value: -1},
		},
		Options: options.Index().SetName("uk_agent_indicators_symbol_period_date").SetUnique(true),
	}
	if _, err := repo.db.Collection(collIndicators).Indexes().CreateOne(ctx, model); err != nil {
		return custom_errors.Internal("创建技术指标索引失败").Wrap(err)
	}
	return nil
}

// Save 幂等批量写入指标快照。
//
// # 为什么是 upsert 而不是 insert
//
// 同一只票同一交易日会被反复分析（用户重试、批量任务重叠、盘中多次刷新）。
// 用 insert 的话自然键上会堆出成百上千条重复文档，而唯一索引会让第二次写直接报错，
// 于是一次正常的重试会变成一次失败的分析。
//
// # 为什么这里没有「先查有没有再决定写不写」
//
// 那是典型的 TOCTOU：两个并发的分析都查到「没有」，都去算，都去写，
// 后写的覆盖先写的——如果两次用的 K 线窗口恰好不同，库里的值会在两个数之间跳。
// 这里的做法是「无条件按自然键 upsert」，配合唯一索引，让检查与写入成为同一个原子操作。
// 内容层面的一致性由计算的确定性保证：同一批 K 线必然算出同一组指标，
// 因此覆盖是幂等的、无害的。
//
// # 为什么只有批量接口
//
// 留了单条写入接口，调用方迟早会在 for 里逐条写。指标数据永远是成批产生的
// （一次分析一批、批量任务一大批），批量是唯一合理的粒度。
func (repo *IndicatorRepository) Save(ctx context.Context, snapshots []*entities.IndicatorSnapshot) error {
	models := make([]mongo.WriteModel, 0, len(snapshots))
	for _, s := range snapshots {
		if s == nil || !s.HasNaturalKey() {
			// 自然键不完整的记录会 upsert 出一条 symbol="" 的黑洞文档，
			// 把所有脏数据合并成一条，直接丢弃。
			continue
		}
		dto := dtos.FromDomainIndicatorSnapshot(s)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{
				"symbol":     dto.Symbol,
				"period":     dto.Period,
				"trade_date": dto.TradeDate,
			}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	if len(models) == 0 {
		// 空批次是常态（非交易日、K 线缺失），BulkWrite 对空数组会报错，直接短路。
		return nil
	}

	// Ordered(false)：批内文档彼此独立，一条坏数据不该让后面几百条全部丢弃；
	// 无序模式还允许 mongo 并行执行，吞吐显著更高。
	_, err := repo.db.Collection(collIndicators).
		BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return translateMongo(err, "技术指标快照")
	}
	return nil
}

// Find 按自然键精确读回一条快照。这是全系统读技术指标的唯一入口。
//
// 找不到返回 NotFound 而不是零值：零值的 MA20 是 0，而 0 会被下游当成
// 「均线在零轴」这个有意义的数字用下去，比一个明确的「没有」危险得多。
func (repo *IndicatorRepository) Find(
	ctx context.Context,
	code shared_vo.StockCode,
	period stock_vo.Period,
	tradeDate shared_vo.TradeDate,
) (*entities.IndicatorSnapshot, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	if tradeDate.IsZero() {
		return nil, custom_errors.Invalid("交易日不能为空")
	}

	var dto dtos.IndicatorSnapshotDto
	err := repo.db.Collection(collIndicators).FindOne(ctx, bson.M{
		"symbol":     code.Symbol,
		"period":     period.OrDaily().String(),
		"trade_date": tradeDate.String(),
	}).Decode(&dto)
	if err != nil {
		return nil, translateMongo(err, "股票(%s) %s 技术指标(%s)",
			code.FullSymbol(), tradeDate.String(), period.OrDaily())
	}
	return dto.ToDomain(), nil
}

// LatestNotAfter 取不晚于给定交易日的最近一条快照。
//
// 它服务的是「周末或节假日发起分析」这种常见场景：请求里的交易日是自然日，
// 而那天根本没有行情。精确查会落空，退到最近一个已算过的交易日才是用户想要的。
// 同样是纯读路径，不会触发任何重算。
func (repo *IndicatorRepository) LatestNotAfter(
	ctx context.Context,
	code shared_vo.StockCode,
	period stock_vo.Period,
	tradeDate shared_vo.TradeDate,
) (*entities.IndicatorSnapshot, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}

	filter := bson.M{"symbol": code.Symbol, "period": period.OrDaily().String()}
	if !tradeDate.IsZero() {
		filter["trade_date"] = bson.M{"$lte": tradeDate.String()}
	}

	var dto dtos.IndicatorSnapshotDto
	err := repo.db.Collection(collIndicators).FindOne(ctx, filter,
		options.FindOne().SetSort(bson.D{{Key: "trade_date", Value: -1}})).Decode(&dto)
	if err != nil {
		return nil, translateMongo(err, "股票(%s) 技术指标(%s)", code.FullSymbol(), period.OrDaily())
	}
	return dto.ToDomain(), nil
}

// History 取区间内的指标序列，按交易日倒序。供报告与前端画指标线使用。
func (repo *IndicatorRepository) History(
	ctx context.Context,
	code shared_vo.StockCode,
	period stock_vo.Period,
	rng shared_vo.DateRange,
	limit int,
) ([]*entities.IndicatorSnapshot, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}

	filter := bson.M{"symbol": code.Symbol, "period": period.OrDaily().String()}
	cond := bson.M{}
	if !rng.Start.IsZero() {
		cond["$gte"] = rng.Start.String()
	}
	if !rng.End.IsZero() {
		cond["$lte"] = rng.End.String()
	}
	if len(cond) > 0 {
		filter["trade_date"] = cond
	}

	cur, err := repo.db.Collection(collIndicators).Find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "trade_date", Value: -1}}).
			SetLimit(int64(clampLimit(limit))))
	if err != nil {
		return nil, translateMongo(err, "股票(%s) 技术指标序列", code.FullSymbol())
	}
	// cur.All 正常路径会自己关闭游标，出错路径不会，补一个 defer 防止游标泄漏到服务端超时。
	defer func() { _ = cur.Close(ctx) }()

	var rows []dtos.IndicatorSnapshotDto
	if err := cur.All(ctx, &rows); err != nil {
		return nil, translateMongo(err, "股票(%s) 技术指标序列", code.FullSymbol())
	}
	return dtos.ToDomainIndicatorSnapshots(rows), nil
}

const (
	// defaultQueryLimit 是 limit<=0 时的默认条数：一次分析最多看一两年日线。
	defaultQueryLimit = 250
	// maxQueryLimit 是硬上限，兜住误传一个巨大 limit 导致的 OOM。
	maxQueryLimit = 2000
)

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultQueryLimit
	case limit > maxQueryLimit:
		return maxQueryLimit
	default:
		return limit
	}
}
