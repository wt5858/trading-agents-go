package repositories

import (
	"context"

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

// 本仓储没有 EnsureIndexes。
//
// 现有的两个方法都按 _id 点查或点写，走的是主键索引，不需要任何额外索引。
// 之前这里建过一条 (created_at desc)，理由是「将来要按时间列出历次回测」——
// 但那个查询并不存在。一条没有查询在用的索引不是零成本：每次写入都要维护它，
// 而且它会让下一个读代码的人以为存在一条列表查询路径。真要加列表功能时，
// 连同它的索引一起加。

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
