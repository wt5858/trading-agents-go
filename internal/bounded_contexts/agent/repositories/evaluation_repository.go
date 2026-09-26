package repositories

import (
	"context"

	"github.com/shopspring/decimal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// collEvaluations 是回测评估集合。
const collEvaluations = "agent_evaluations"

// EvaluationRepository 读写回测评估，是 Evaluation 聚合根的仓储。
//
// 整个聚合（评估本身 + 全部样本 + 固化统计）内嵌在一份文档里，一次写完。
// 事务只存在于本层，而这里连显式事务都不需要：Mongo 的单文档写入本身就是原子的，
// 「统计写进去了但样本少了一半」这种半截状态在存储层面不可能出现——
// 而那恰恰是最危险的一种脏数据：它看起来完全正常。
type EvaluationRepository struct {
	db *mongo.Database
}

func NewEvaluationRepository(db *mongo.Database) *EvaluationRepository {
	return &EvaluationRepository{db: db}
}

// EnsureIndexes 幂等建索引，可在每次启动时调用。
//
// 这里只有一条 (created_at desc)，服务于 ListRecent——列表页要的就是
// 「最近几次回测」，而它是本集合唯一一条不按 _id 走的查询。
//
// 这条索引曾经被删掉过，理由写得很明白：当时没有任何查询在用它，
// 而一条没人用的索引不是零成本（每次写入都要维护，还会误导读代码的人
// 以为存在一条列表路径）。那条注释同时留了话——「真要加列表功能时，
// 连同它的索引一起加」。ListRecent 就是那个功能，这是那句话的兑现。
//
// # 名字必须是 idx_agent_evaluations_created，不能改
//
// 当初那次「删索引」删的只是代码——**已经建好的索引还留在数据库里**。
// 而 Mongo 用键去重、用名字标识：同一组键配一个不同的名字，
// 它报的是 IndexOptionsConflict 而不是「已存在，跳过」。
// 于是换个名字重新加回来，所有老库都会在启动建索引这一步直接起不来，
// 而新库一切正常——一个只在别人机器上复现的启动失败。
//
// 改索引名在 Mongo 里是破坏性变更，要改就得同时给一条 dropIndex 的迁移。
func (repo *EvaluationRepository) EnsureIndexes(ctx context.Context) error {
	if repo.db == nil {
		return nil
	}
	model := mongo.IndexModel{
		Keys: bson.D{{Key: "created_at", Value: -1}},
		// idx_ 前缀是本仓库普通索引的惯例，uk_ 留给唯一索引。
		Options: options.Index().SetName("idx_agent_evaluations_created"),
	}
	if _, err := repo.db.Collection(collEvaluations).Indexes().CreateOne(ctx, model); err != nil {
		return custom_errors.Internal("创建回测评估索引失败").Wrap(err)
	}
	return nil
}

// ListRecent 按创建时间倒序列出最近的评估。
//
// 只投影出统计与元信息，**不带样本**：一次评估内嵌着几百上千条样本，
// 而列表页一条都用不到。不投影的话，一次「看看最近几次回测」
// 会把几十 MB 的样本读进内存——这和 AnalysisRunRepository.ListSummaries
// 排除 turns 是同一个理由。
func (repo *EvaluationRepository) ListRecent(ctx context.Context, limit int) ([]*entities.Evaluation, error) {
	if repo.db == nil {
		return nil, custom_errors.Unavailable("未启用 MongoDB，回测评估不可用")
	}
	if limit <= 0 || limit > maxEvaluationListLimit {
		limit = defaultEvaluationListLimit
	}
	cursor, err := repo.db.Collection(collEvaluations).Find(ctx, bson.M{},
		options.Find().
			SetProjection(bson.M{"samples": 0}).
			SetSort(bson.D{{Key: "created_at", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, custom_errors.Internal("查询回测评估失败").Wrap(err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []dtos.EvaluationDto
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, custom_errors.Internal("解析回测评估失败").Wrap(err)
	}
	out := make([]*entities.Evaluation, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out, nil
}

const (
	defaultEvaluationListLimit = 20
	maxEvaluationListLimit     = 100
	// maxTrackRecordSamples 是战绩查询回溯的样本上限。
	// 它既是内存上限，也是一条语义边界：更久远的建议对「这套系统最近判得准不准」
	// 没有参考价值，而把三年前的样本混进来只会稀释近期的信号。
	maxTrackRecordSamples = 200
)

// TrackRecordSample 是战绩里的一条：当时建议了什么，后来涨跌如何。
type TrackRecordSample struct {
	TradeDate string
	Action    string
	ReturnPct decimal.Decimal
	Hit       bool
}

// TrackRecord 是系统对某只标的的历史战绩。
//
// # 为什么读已落库的评分而不是现场重算
//
// 命中与否是乘除派生量（由收益率与横盘带宽判出来），本仓库的规矩是
// 算一次落库、读路径不得重算。更实际的理由是：判定口径会变
// （横盘带宽调过一次，一致率就跟着变），现算出来的战绩和当初那份评测报告
// 会对不上——而两个数字都以「命中率」的名义出现在用户面前。
type TrackRecord struct {
	Symbol  string
	Scored  int
	Hits    int
	Samples []TrackRecordSample
}

// TrackRecordOf 取系统对某只标的的历史战绩。
//
// 只统计**已评分**的样本：跳过的那些（没给方向、缺行情）既没有对错可言，
// 把它们算进分母会让战绩凭空变差，算进分子则是送分。
func (repo *EvaluationRepository) TrackRecordOf(ctx context.Context, symbol string) (TrackRecord, error) {
	out := TrackRecord{Symbol: symbol}
	if repo.db == nil || symbol == "" {
		return out, nil
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$unwind", Value: "$samples"}},
		bson.D{{Key: "$match", Value: bson.M{
			"samples.symbol": symbol,
			// skip_reason 带 omitempty，已评分的样本上这个字段根本不存在。
			// 用 $exists:false 而不是等于空串——后者匹配不到不存在的字段。
			"samples.skip_reason": bson.M{"$exists": false},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "samples.trade_date", Value: -1}}}},
		bson.D{{Key: "$limit", Value: maxTrackRecordSamples}},
		bson.D{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$samples"}}},
	}
	cursor, err := repo.db.Collection(collEvaluations).Aggregate(ctx, pipeline)
	if err != nil {
		return out, custom_errors.Internal("查询历史战绩失败").Wrap(err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var rows []struct {
		TradeDate string          `bson:"trade_date"`
		Action    string          `bson:"action"`
		ReturnPct decimal.Decimal `bson:"return_pct"`
		Hit       bool            `bson:"hit"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return out, custom_errors.Internal("解析历史战绩失败").Wrap(err)
	}

	// 同一条运行可能被多次评测覆盖（重跑 backtest 会产出新的评估文档），
	// 按交易日去重只留最新那份，否则一次重跑就会让战绩样本数翻倍。
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.TradeDate]; dup {
			continue
		}
		seen[r.TradeDate] = struct{}{}
		out.Scored++
		if r.Hit {
			out.Hits++
		}
		out.Samples = append(out.Samples, TrackRecordSample{
			TradeDate: r.TradeDate, Action: r.Action, ReturnPct: r.ReturnPct, Hit: r.Hit,
		})
	}
	return out, nil
}

// Save 落库一次评估。整个聚合交出去，调用方不碰单条样本。
// 按 _id 覆盖写：同一个评估 ID 重跑得到最新一份，天然幂等。
func (repo *EvaluationRepository) Save(ctx context.Context, e *entities.Evaluation) error {
	if repo.db == nil {
		return custom_errors.Unavailable("未启用 MongoDB，回测评估不可用")
	}
	if e == nil || e.ID == "" {
		return custom_errors.Invalid("评估 ID 不能为空")
	}

	dto := dtos.FromDomainEvaluation(e)
	_, err := repo.db.Collection(collEvaluations).ReplaceOne(
		ctx,
		bson.M{"_id": dto.ID},
		dto,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return custom_errors.Internal("保存回测评估失败").Wrap(err)
	}
	return nil
}

// FindByID 取一次评估。
func (repo *EvaluationRepository) FindByID(ctx context.Context, id string) (*entities.Evaluation, error) {
	if repo.db == nil {
		return nil, custom_errors.Unavailable("未启用 MongoDB，回测评估不可用")
	}

	var dto dtos.EvaluationDto
	err := repo.db.Collection(collEvaluations).FindOne(ctx, bson.M{"_id": id}).Decode(&dto)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, custom_errors.NotFound("回测评估(%s)不存在", id)
		}
		return nil, custom_errors.Internal("读取回测评估失败").Wrap(err)
	}
	return dto.ToDomain(), nil
}
