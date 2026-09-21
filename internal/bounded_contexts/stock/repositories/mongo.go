package repositories

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MongoDB 承载量价/财务/资讯这类高频、大体量、schema 不稳定的时序数据，
// MySQL 只放主数据（stocks 表）与业务状态。
//
// 连接与索引管理放在本层而不是单独的基础设施包：集合名、索引键与 DTO 的 bson 标签
// 必须逐字对齐，分在两个包里改一处忘一处的代价是「写入悄悄退化成全集合扫描」。

// 集合名集中声明，避免各处硬编码写错了还查不出来
// （mongo 查不存在的集合不报错，只返回空）。
const (
	collQuotes      = "quotes"
	collKlines      = "klines"
	collFinancials  = "financials"
	collNews        = "news"
	collSocialPosts = "social_posts"
)

// Connect 建立连接并探活。
func Connect(ctx context.Context, cfg config.Mongo) (*mongo.Database, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	opts := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(timeout).
		// ServerSelectionTimeout 决定副本集主节点切换时请求要挂多久。
		// 同步任务宁可快速失败下次重试，也不要把 worker 堵在这里。
		SetServerSelectionTimeout(timeout).
		SetTimeout(timeout)

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, custom_errors.Internal("连接 MongoDB 失败").Wrap(err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 用 Primary 探活而不是默认的 nearest：写路径能不能用才是我们关心的。
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, custom_errors.Internal("MongoDB 探活失败").Wrap(err)
	}

	db := cfg.Database
	if db == "" {
		db = "trading_agents"
	}
	return client.Database(db), nil
}

// EnsureIndexes 幂等地建好全部索引，可在每次启动时调用。
//
// 索引设计的两条原则：
//  1. 每个集合的自然键都建唯一索引——所有写入都是「重刷重叠窗口」的 upsert，
//     唯一索引既是幂等性的兜底，也是 upsert 定位文档的手段
//     （没有它 upsert 会退化成全集合扫描）。
//  2. 唯一键统一写成 (标识字段 asc, 时间字段 desc) 的复合形式。
//     降序的时间字段不影响唯一性判定，却能让「某标的最近 N 条」这类查询
//     直接用同一个索引完成等值过滤 + 倒序排序，省掉一次 SORT 阶段。
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	type spec struct {
		coll  string
		model mongo.IndexModel
	}

	specs := []spec{
		// 行情：一只票一个交易日只能有一条快照。
		{collQuotes, mongo.IndexModel{
			Keys:    bson.D{{Key: "symbol", Value: 1}, {Key: "trade_date", Value: -1}},
			Options: options.Index().SetName("uk_quotes_symbol_date").SetUnique(true),
		}},
		// K 线：周期必须进唯一键，日线/周线/月线是同一 symbol 下并存的三套序列。
		{collKlines, mongo.IndexModel{
			Keys: bson.D{
				{Key: "symbol", Value: 1},
				{Key: "period", Value: 1},
				{Key: "trade_date", Value: -1},
			},
			Options: options.Index().SetName("uk_klines_symbol_period_date").SetUnique(true),
		}},
		// 财务：按报告期唯一。同一报告期的快报/正式报会被后来的覆盖，这正是想要的行为。
		{collFinancials, mongo.IndexModel{
			Keys:    bson.D{{Key: "symbol", Value: 1}, {Key: "report_date", Value: -1}},
			Options: options.Index().SetName("uk_financials_symbol_report_date").SetUnique(true),
		}},
		// 资讯：URL 是唯一可靠的去重依据（同一篇稿子会被多家源以不同标题转载，
		// published_at 也常有几秒到几分钟的漂移，都不能做键）。
		{collNews, mongo.IndexModel{
			Keys:    bson.D{{Key: "symbol", Value: 1}, {Key: "url", Value: 1}},
			Options: options.Index().SetName("uk_news_symbol_url").SetUnique(true),
		}},
		// 查询路径是「某标的某时间窗内的新闻」，URL 索引帮不上忙，单独建时间索引。
		{collNews, mongo.IndexModel{
			Keys:    bson.D{{Key: "symbol", Value: 1}, {Key: "published_at", Value: -1}},
			Options: options.Index().SetName("idx_news_symbol_published"),
		}},
		// 社交舆情没有 URL，退而用 (平台, 发布时间) 做自然键：
		// 同一平台同一时刻的同标的贴文视作同一条，接受极小概率的误合并，
		// 换取重复抓取时不会把同一条讨论灌进去几十遍。
		{collSocialPosts, mongo.IndexModel{
			Keys: bson.D{
				{Key: "symbol", Value: 1},
				{Key: "platform", Value: 1},
				{Key: "published_at", Value: -1},
			},
			Options: options.Index().SetName("uk_social_symbol_platform_published").SetUnique(true),
		}},
	}

	for _, s := range specs {
		if _, err := db.Collection(s.coll).Indexes().CreateOne(ctx, s.model); err != nil {
			return custom_errors.Internal("创建 MongoDB 索引失败: %s", s.coll).Wrap(err)
		}
	}
	return nil
}
