package repositories

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// EnsureScreeningIndexes 幂等地补上选股筛选特有的 MongoDB 索引，可在每次启动时调用。
//
// # 为什么股票上下文已有的索引不够用
//
// stock/repositories/mongo.go 建的索引都以 symbol 为前缀
// （uk_quotes_symbol_date、uk_financials_symbol_report_date），
// 它们服务的是「某一只票的时间序列」这条访问路径——那是分析上下文的形状。
//
// 选股是**相反**的形状：给定一个交易日，横着扫过全部股票。
// 它的查询谓词是 {trade_date: '2026-09-16', pe: {...}}，里面压根没有 symbol，
// 于是那个以 symbol 打头的复合索引一条都用不上，查询会退化成全集合扫描
// （5000 只票 × 数百个交易日）。这正是「把过滤下推到数据库」会失效的方式——
// 语句写对了，但没有索引，数据库照样把全量文档读了一遍。
//
// 所以本函数补两个以时间维度打头的索引，让横截面扫描只碰当天那 5000 条文档。
//
// # 为什么建在这里而不是 stock 上下文
//
// 索引服务于访问路径，而这两条访问路径只有选股会走。把它们放在这里，
// 「为什么存在这个索引」这个问题在代码里有答案；混进股票上下文那份清单里，
// 下一个做索引瘦身的人会看不出它还有用户。
//
// 组装根（internal/di）需要在启动时调用一次。本次改动不触碰 internal/di，
// 因此接线要单独做——在那之前筛选功能依然正确，只是慢。
func EnsureScreeningIndexes(ctx context.Context, db *mongo.Database) error {
	if db == nil {
		return nil
	}
	type spec struct {
		coll  string
		model mongo.IndexModel
	}
	specs := []spec{
		// 行情横截面：trade_date 打头做等值定位，symbol 跟在后面让
		// 「候选代码集 $in」也能吃到同一个索引，并顺带覆盖了只投影 symbol 的查询。
		{collQuotes, mongo.IndexModel{
			Keys:    bson.D{{Key: "trade_date", Value: -1}, {Key: "symbol", Value: 1}},
			Options: options.Index().SetName("idx_quotes_date_symbol"),
		}},
		// 财报回溯窗口：report_date 区间扫描 + 按 symbol 有序输出，
		// 让 latestFinancialPipeline 的 $sort 阶段直接吃索引，不做内存排序。
		{collFinancials, mongo.IndexModel{
			Keys:    bson.D{{Key: "report_date", Value: -1}, {Key: "symbol", Value: 1}},
			Options: options.Index().SetName("idx_financials_report_date_symbol"),
		}},
	}
	for _, s := range specs {
		if _, err := db.Collection(s.coll).Indexes().CreateOne(ctx, s.model); err != nil {
			return custom_errors.Internal("创建选股索引失败: %s", s.coll).Wrap(err)
		}
	}
	return nil
}
