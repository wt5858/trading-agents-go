package repositories

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// collAnalysisRuns 是分析运行轨迹集合。集中声明避免各处硬编码：
// mongo 查一个不存在的集合不报错，只返回空，拼错了根本查不出来。
const collAnalysisRuns = "agent_runs"

// defaultSummaryLimit 是区间查询的兜底上限。
//
// 一个没有上限的区间查询迟早会被一个手滑写成 2000-01-01 的起始日期
// 变成全集合扫描，而调用方（回测命令）拿到几十万条之后只会 OOM。
// 1000 条覆盖得住一次正常回测的样本量，不够时由调用方显式加大。
const defaultSummaryLimit = 1000

// AnalysisRunRepository 读写一次分析的运行轨迹，是 AnalysisContext 聚合根的仓储。
//
// # 它为什么存在
//
// indicator_repository.go 里曾写着「AnalysisContext 是一次运行期的共享状态，
// 随进程结束而消失，不配仓储」。这个判断在只需要交付结论的时候是对的，
// 但排查一次分析为什么慢、为什么贵、为什么这次和上次结论相反，需要的全是
// 那份共享状态里的过程信息——而结论里一个字都没有。
// 轨迹必须活过进程，于是这个聚合有了仓储。
//
// # 原子性
//
// 一次运行的全部发言内嵌在同一份文档的 turns 数组里，一次 ReplaceOne 写完。
// 事务只存在于本层，且这里连显式事务都不需要：Mongo 的单文档写入本身就是原子的，
// 「根写进去了但少了两条发言」这种半截状态在存储层面就不可能出现。
type AnalysisRunRepository struct {
	db *mongo.Database
}

func NewAnalysisRunRepository(db *mongo.Database) *AnalysisRunRepository {
	return &AnalysisRunRepository{db: db}
}

// EnsureIndexes 幂等建索引，可在每次启动时调用。
//
// 主键是任务 ID（_id），点查不需要额外索引。这里建两条，各自对应一种查法：
//
//  1. (symbol, trade_date desc)：看板按「某只票跑过哪些分析」检索。
//     trade_date 降序与 agent_indicators、stock 的 klines 保持同一形态，
//     跨集合聚合才拼得起来。
//
//  2. (failed, trade_date asc)：回测的 ListSummaries 用。它不带 symbol，
//     因此用不上第一条索引的前缀——只建第一条的话，一次回测就是一次
//     全集合扫描加一次阻塞式排序，样本一多直接撞上 Mongo 32MB 的排序内存上限报错。
//     字段顺序是「等值在前、范围在后」：failed 是等值过滤，trade_date 是区间，
//     反过来排的话区间之后的等值条件用不上索引。升序与查询的排序方向一致，
//     这样连排序阶段都省掉。
func (repo *AnalysisRunRepository) EnsureIndexes(ctx context.Context) error {
	if repo.db == nil {
		return nil
	}
	models := []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "symbol", Value: 1},
				{Key: "trade_date", Value: -1},
			},
			Options: options.Index().SetName("idx_agent_runs_symbol_date"),
		},
		{
			Keys: bson.D{
				{Key: "failed", Value: 1},
				{Key: "trade_date", Value: 1},
			},
			Options: options.Index().SetName("idx_agent_runs_failed_date"),
		},
	}
	if _, err := repo.db.Collection(collAnalysisRuns).Indexes().CreateMany(ctx, models); err != nil {
		return custom_errors.Internal("创建分析轨迹索引失败").Wrap(err)
	}
	return nil
}

// Save 落库一次运行的完整轨迹。
//
// 整个聚合一次性交出去，调用方不碰单条发言：turns 是根的一部分，
// 没有任何独立于这次运行的写入路径。
//
// 按 _id 覆盖写而不是插入：任务派发是至少一次投递，同一个任务重跑时
// 应该得到最新一份轨迹，而不是两份无从分辨的记录。覆盖写天然幂等，
// 重放多少次结果都一样。
//
// failReason 为空表示这次运行正常收尾。
func (repo *AnalysisRunRepository) Save(
	ctx context.Context,
	ac *entities.AnalysisContext,
	finishedAt time.Time,
	failReason string,
) error {
	if repo.db == nil {
		return nil
	}
	if ac == nil || ac.RunID() == "" {
		// 没有任务 ID 的运行（脚本直调）没有可用的主键。
		// 编一个出来只会在集合里堆出一批谁也查不到的孤儿文档。
		return nil
	}

	dto := dtos.FromDomainAnalysisRun(ac, finishedAt, failReason)
	_, err := repo.db.Collection(collAnalysisRuns).ReplaceOne(
		ctx,
		bson.M{"_id": dto.RunID},
		dto,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return custom_errors.Internal("保存分析轨迹失败").Wrap(err)
	}
	return nil
}

// ListSummaries 列出某段交易日区间内的运行骨架，按交易日升序。
// 第二个返回值表示区间里还有更多运行没取回来。
//
// 投影掉 turns：回测要扫的是几百上千次运行，正文一个字都用不上，
// 读回来只会把几十 MB 的报告搬进内存再丢掉。
// 走的是 EnsureIndexes 建的 (failed, trade_date) 索引。
//
// # 为什么要回一个截断标志
//
// 因为调用方是回测，而它的全部价值建立在「样本量是诚实的」之上。
// 静默丢掉超出上限的那部分，统计里的「扫到运行 N」看起来就是全量，
// 而实际上是被砍过的——这正是回测本身极力避免的那种自欺。
// 多取一条来判断有没有超，代价是一行，换的是调用方能如实汇报。
func (repo *AnalysisRunRepository) ListSummaries(
	ctx context.Context,
	rng shared_vo.DateRange,
	limit int,
) ([]value_objects.RunSummary, bool, error) {
	if repo.db == nil {
		return nil, false, custom_errors.Unavailable("未启用 MongoDB，分析轨迹不可用")
	}
	if limit <= 0 {
		limit = defaultSummaryLimit
	}

	filter := bson.M{
		// trade_date 是定长串，字典序等价于时间序，因此区间查询可以直接比字符串。
		"trade_date": bson.M{"$gte": rng.Start.String(), "$lte": rng.End.String()},
		// 跑挂的运行没有可评分的结论，在数据库这一侧就滤掉，
		// 不要读回来再在内存里丢——它们在失败率高的时段能占到样本的一大半。
		"failed": false,
	}
	opts := options.Find().
		SetProjection(bson.M{"turns": 0}).
		SetSort(bson.D{{Key: "trade_date", Value: 1}}).
		// 多取一条：取回来 limit+1 条就说明区间里还有更多，
		// 这比再发一次 CountDocuments 便宜得多。
		SetLimit(int64(limit) + 1)

	cursor, err := repo.db.Collection(collAnalysisRuns).Find(ctx, filter, opts)
	if err != nil {
		return nil, false, custom_errors.Internal("查询分析轨迹失败").Wrap(err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []dtos.AnalysisRunSummaryDto
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, false, custom_errors.Internal("解析分析轨迹失败").Wrap(err)
	}

	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return dtos.ToDomainAnalysisRunSummaries(rows), truncated, nil
}

// ExistingRunIDs 从给定的一批 ID 里挑出已经落库且未失败的那些。
//
// 供批量回填做断点续跑：一次查询判掉整批，而不是每格跑之前查一次——
// 几百格就是几百次往返，而这批查询恰好发生在决定要不要花钱之前，
// 让它慢等于让「估算成本」这一步比跑分析本身还久。
//
// 只认未失败的运行：一次跑挂的记录同样占着那个 _id，
// 把它当成「已完成」会让所有失败的格子永远不会被重试，
// 而它们恰恰是最需要重跑的那些。
func (repo *AnalysisRunRepository) ExistingRunIDs(ctx context.Context, ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if repo.db == nil || len(ids) == 0 {
		return out, nil
	}
	cursor, err := repo.db.Collection(collAnalysisRuns).Find(ctx,
		bson.M{"_id": bson.M{"$in": ids}, "failed": false},
		options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, custom_errors.Internal("查询已有分析轨迹失败").Wrap(err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []struct {
		ID string `bson:"_id"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, custom_errors.Internal("解析已有分析轨迹失败").Wrap(err)
	}
	for _, r := range rows {
		out[r.ID] = struct{}{}
	}
	return out, nil
}

// RunCostStats 是历史运行的成本统计，用于估算一批回填要花多少钱。
type RunCostStats struct {
	// Samples 是统计所基于的运行条数。它必须和均值一起交给调用方：
	// 基于 3 条样本算出的「平均成本」不该被当成预算依据，
	// 而只报一个均值就等于把这个判断从用户手里拿走了。
	Samples int
	AvgUSD  decimal.Decimal
	MaxUSD  decimal.Decimal
}

// RunCostStatsOf 按深度统计历史运行的单次成本。
//
// 按深度分组是必要的：depth 1 与 depth 3 之间差着辩论与风控两个阶段，
// 成本相差数倍，混在一起算出的均值对哪一档都不适用。
//
// 排除失败运行与缓存命中为零成本的极端值都不做——失败的运行也是真花了钱的，
// 而回填过程中同样会有失败。要估的是「跑一批下来实际扣多少」，不是理想值。
func (repo *AnalysisRunRepository) RunCostStatsOf(ctx context.Context, depth int) (RunCostStats, error) {
	if repo.db == nil {
		return RunCostStats{}, custom_errors.Unavailable("未启用 MongoDB，分析轨迹不可用")
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"depth": depth}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id": nil,
			"n":   bson.M{"$sum": 1},
			"avg": bson.M{"$avg": "$cost_usd"},
			"max": bson.M{"$max": "$cost_usd"},
		}}},
	}
	cursor, err := repo.db.Collection(collAnalysisRuns).Aggregate(ctx, pipeline)
	if err != nil {
		return RunCostStats{}, custom_errors.Internal("统计历史成本失败").Wrap(err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []struct {
		N   int             `bson:"n"`
		Avg decimal.Decimal `bson:"avg"`
		Max decimal.Decimal `bson:"max"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return RunCostStats{}, custom_errors.Internal("解析历史成本失败").Wrap(err)
	}
	if len(rows) == 0 {
		return RunCostStats{}, nil
	}
	return RunCostStats{Samples: rows[0].N, AvgUSD: rows[0].Avg, MaxUSD: rows[0].Max}, nil
}

// FindByRunID 按任务 ID 取一次运行的轨迹。
//
// 回值对象而不是聚合：这是一条纯读路径，读者要的是「当时发生了什么」，
// 没有任何状态需要再往前推进。理由在 dtos.AnalysisRunDto.ToDomain 上写全了。
func (repo *AnalysisRunRepository) FindByRunID(ctx context.Context, runID string) (value_objects.RunTrace, error) {
	if repo.db == nil {
		return value_objects.RunTrace{}, custom_errors.Unavailable("未启用 MongoDB，分析轨迹不可用")
	}

	var dto dtos.AnalysisRunDto
	err := repo.db.Collection(collAnalysisRuns).
		FindOne(ctx, bson.M{"_id": runID}).
		Decode(&dto)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return value_objects.RunTrace{}, custom_errors.NotFound("分析轨迹(%s)不存在", runID)
		}
		return value_objects.RunTrace{}, custom_errors.Internal("读取分析轨迹失败").Wrap(err)
	}
	return dto.ToDomain(), nil
}
