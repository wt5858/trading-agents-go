package repositories

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

const (
	// defaultQueryLimit 是 limit<=0 时的默认条数。本层没有定义「不限条数」的语义，
	// 而分析上下文一次最多也就看一两年日线，给一个有限默认值可以防止误传 0
	// 把整只票拉进内存。
	defaultQueryLimit = 500
	// maxQueryLimit 是硬上限。K 线接口被前端画图直连，必须有个天花板兜住 OOM。
	maxQueryLimit = 5000
)

// MarketDataRepository 读写行情/K线/财务/资讯/舆情（MongoDB）。
//
// 这些都是读模型值对象而不是实体，因此它们没有各自的聚合仓储，
// 统一由本仓储在读路径上直接返回 VO——这与「只有聚合根配仓储」并不冲突：
// 被禁止的是给子实体配仓储，而不是给读模型提供查询入口。
//
// 所有读方法都返回值对象切片而不是指针切片：这些是不可变读模型，
// 返回指针只会诱使调用方就地修改一份共享数据。
// 所有写方法都是批量的：这一层的数据永远成批到达（一次同步几百根 K 线），
// 不提供单条写入是刻意的——留了单条接口，调用方迟早会在 for 里逐条写。
type MarketDataRepository struct {
	db *mongo.Database
}

func NewMarketDataRepository(db *mongo.Database) *MarketDataRepository {
	return &MarketDataRepository{db: db}
}

func (repo *MarketDataRepository) GetDb() *mongo.Database { return repo.db }

// ---------------------------------------------------------------------------
// 行情快照
// ---------------------------------------------------------------------------

// SaveQuotes 幂等批量写入。
//
// 为什么必须是 upsert 而不是 insert：同步任务按「最近 N 个交易日」滚动拉取，
// 相邻两次执行的窗口大面积重叠；而且盘中快照一天内要被刷新很多次。
// 用 insert 的话同一个交易日会堆出成百上千条重复文档，
// 后续的 LatestQuote 取到哪一条完全看运气。
//
// 「按自然键定位再写」由 uk_quotes_symbol_date 唯一索引兜底，
// 不存在先查后插的窗口。
func (repo *MarketDataRepository) SaveQuotes(ctx context.Context, quotes []value_objects.Quote) error {
	models := make([]mongo.WriteModel, 0, len(quotes))
	now := time.Now()
	for i := range quotes {
		q := quotes[i]
		if !q.HasNaturalKey() {
			// 自然键不完整的记录会 upsert 出一条 symbol="" 的垃圾文档，直接丢弃。
			continue
		}
		q.UpdatedAt = now
		dto := dtos.FromDomainQuote(q)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"symbol": dto.Symbol, "trade_date": dto.TradeDate}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	return repo.bulkWrite(ctx, collQuotes, models, "行情快照")
}

// LatestQuote 取最新一条。(symbol asc, trade_date desc) 索引让这次查询等值定位后
// 直接读索引的第一条，不需要排序。
func (repo *MarketDataRepository) LatestQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	var dto dtos.QuoteDto
	err := repo.db.Collection(collQuotes).
		FindOne(ctx, bson.M{"symbol": code.Symbol},
			options.FindOne().SetSort(bson.D{{Key: "trade_date", Value: -1}})).
		Decode(&dto)
	if err != nil {
		return nil, translateMongo(err, "股票(%s) 最新行情", code.FullSymbol())
	}
	q := dto.ToDomain()
	return &q, nil
}

// LatestQuotes 一次取回一批标的的最新行情。
//
// 用聚合管道的 $sort + $group($first) 而不是在 Go 里对每个 symbol 发一次 FindOne：
// 后者是 N 次网络往返，一个 300 只股票的自选股看板就是 300 个 RTT。
// $sort 走 (symbol, trade_date desc) 索引，$group 只需顺序扫过一遍。
func (repo *MarketDataRepository) LatestQuotes(ctx context.Context, codes []shared_vo.StockCode) ([]value_objects.Quote, error) {
	symbols := make([]string, 0, len(codes))
	for _, c := range codes {
		if !c.IsZero() {
			symbols = append(symbols, c.Symbol)
		}
	}
	if len(symbols) == 0 {
		return []value_objects.Quote{}, nil
	}

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"symbol": bson.M{"$in": symbols}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "symbol", Value: 1}, {Key: "trade_date", Value: -1}}}},
		// $$ROOT 整份文档进 $first，出来的 latest 子文档可以直接被 $replaceRoot 摊平，
		// 省掉逐字段罗列——加一列行情字段时这里不用跟着改。
		bson.D{{Key: "$group", Value: bson.M{"_id": "$symbol", "latest": bson.M{"$first": "$$ROOT"}}}},
		bson.D{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$latest"}}},
	}

	cur, err := repo.db.Collection(collQuotes).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, translateMongo(err, "批量最新行情(%d 只)", len(symbols))
	}
	defer func() { _ = cur.Close(ctx) }()

	var rows []dtos.QuoteDto
	if err := cur.All(ctx, &rows); err != nil {
		return nil, translateMongo(err, "批量最新行情(%d 只)", len(symbols))
	}
	return dtos.ToDomainQuotes(rows), nil
}

// QuoteHistory 取区间历史。trade_date 存的是 YYYY-MM-DD 定长串，
// 字典序与时间序等价，所以可以直接用 $gte/$lte 做区间比较，省掉一次类型转换。
func (repo *MarketDataRepository) QuoteHistory(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]value_objects.Quote, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	filter := bson.M{"symbol": code.Symbol}
	if c := dateStringRange(rng); c != nil {
		filter["trade_date"] = c
	}

	var rows []dtos.QuoteDto
	if err := repo.find(ctx, collQuotes, filter, bson.D{{Key: "trade_date", Value: -1}}, limit, &rows); err != nil {
		return nil, translateMongo(err, "股票(%s) 历史行情", code.FullSymbol())
	}
	return dtos.ToDomainQuotes(rows), nil
}

// ---------------------------------------------------------------------------
// K 线
// ---------------------------------------------------------------------------

// SaveKlines 自然键 = (symbol, period, trade_date)。
// period 必须进键：同一天的日线和周线是两条合法文档，漏掉 period 会让它们互相覆盖。
// 复权与否（adjusted）刻意不进键——同一序列只保留一种复权口径，
// 切换口径时就是要用新数据整体覆盖旧数据。
func (repo *MarketDataRepository) SaveKlines(ctx context.Context, klines []value_objects.Kline) error {
	models := make([]mongo.WriteModel, 0, len(klines))
	for i := range klines {
		k := klines[i]
		if !k.HasNaturalKey() {
			continue
		}
		dto := dtos.FromDomainKline(k)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"symbol": dto.Symbol, "period": dto.Period, "trade_date": dto.TradeDate}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	return repo.bulkWrite(ctx, collKlines, models, "K 线")
}

func (repo *MarketDataRepository) Klines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, rng shared_vo.DateRange, limit int) ([]value_objects.Kline, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	// 上游偶尔漏传周期，退化成日线比返回空数组更符合调用者预期。
	period = period.OrDaily()
	filter := bson.M{"symbol": code.Symbol, "period": period.String()}
	if c := dateStringRange(rng); c != nil {
		filter["trade_date"] = c
	}

	var rows []dtos.KlineDto
	if err := repo.find(ctx, collKlines, filter, bson.D{{Key: "trade_date", Value: -1}}, limit, &rows); err != nil {
		return nil, translateMongo(err, "股票(%s) %s K 线", code.FullSymbol(), period)
	}
	return dtos.ToDomainKlines(rows), nil
}

// ---------------------------------------------------------------------------
// 财务
// ---------------------------------------------------------------------------

// SaveFinancials 自然键 = (symbol, report_date)。
// period_type（annual/quarter/ttm）不进键：报告期本身已经隐含了口径，
// 而且业绩快报转正式报时报告期不变、数字会变，正需要后写的覆盖先写的。
func (repo *MarketDataRepository) SaveFinancials(ctx context.Context, items []value_objects.Financial) error {
	models := make([]mongo.WriteModel, 0, len(items))
	now := time.Now()
	for i := range items {
		f := items[i]
		if !f.HasNaturalKey() {
			continue
		}
		f.UpdatedAt = now
		dto := dtos.FromDomainFinancial(f)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"symbol": dto.Symbol, "report_date": dto.ReportDate}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	return repo.bulkWrite(ctx, collFinancials, models, "财务数据")
}

func (repo *MarketDataRepository) Financials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	var rows []dtos.FinancialDto
	err := repo.find(ctx, collFinancials, bson.M{"symbol": code.Symbol},
		bson.D{{Key: "report_date", Value: -1}}, limit, &rows)
	if err != nil {
		return nil, translateMongo(err, "股票(%s) 财务数据", code.FullSymbol())
	}
	return dtos.ToDomainFinancials(rows), nil
}

// ---------------------------------------------------------------------------
// 资讯与舆情
// ---------------------------------------------------------------------------

// SaveNews 自然键 = (symbol, url)。
// 没有 URL 的条目由 HasNaturalKey 拦掉，否则会 upsert 出一条 url="" 的黑洞文档，
// 把所有无 URL 新闻都合并成一条。
func (repo *MarketDataRepository) SaveNews(ctx context.Context, items []value_objects.News) error {
	models := make([]mongo.WriteModel, 0, len(items))
	for i := range items {
		n := items[i]
		if !n.HasNaturalKey() {
			continue
		}
		dto := dtos.FromDomainNews(n)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"symbol": dto.Symbol, "url": dto.URL}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	return repo.bulkWrite(ctx, collNews, models, "资讯")
}

func (repo *MarketDataRepository) News(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]value_objects.News, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	filter := bson.M{"symbol": code.Symbol}
	if c := dateTimeRange(rng); c != nil {
		filter["published_at"] = c
	}

	var rows []dtos.NewsDto
	if err := repo.find(ctx, collNews, filter, bson.D{{Key: "published_at", Value: -1}}, limit, &rows); err != nil {
		return nil, translateMongo(err, "股票(%s) 资讯", code.FullSymbol())
	}
	return dtos.ToDomainNewsList(rows), nil
}

// SaveSocialPosts 自然键 = (symbol, platform, published_at)。
// 社交平台不给稳定的贴文 ID，只能用这三元组近似去重：
// 同平台、同标的、同一时刻的贴文当作同一条。代价是极小概率的误合并，
// 收益是重复抓取时不会把同一波讨论重复灌入、把情绪分算歪。
func (repo *MarketDataRepository) SaveSocialPosts(ctx context.Context, items []value_objects.SocialPost) error {
	models := make([]mongo.WriteModel, 0, len(items))
	for i := range items {
		p := items[i]
		if !p.HasNaturalKey() {
			continue
		}
		// FromDomainSocialPost 已经把 published_at 截断到毫秒（BSON 精度上限），
		// 这里用截断后的值做 filter，保证写入与查询用的是同一个时刻值。
		dto := dtos.FromDomainSocialPost(p)
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"symbol": dto.Symbol, "platform": dto.Platform, "published_at": dto.PublishedAt}).
			SetUpdate(bson.M{"$set": dto}).
			SetUpsert(true))
	}
	return repo.bulkWrite(ctx, collSocialPosts, models, "社交舆情")
}

func (repo *MarketDataRepository) SocialPosts(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]value_objects.SocialPost, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	filter := bson.M{"symbol": code.Symbol}
	if c := dateTimeRange(rng); c != nil {
		filter["published_at"] = c
	}

	var rows []dtos.SocialPostDto
	if err := repo.find(ctx, collSocialPosts, filter, bson.D{{Key: "published_at", Value: -1}}, limit, &rows); err != nil {
		return nil, translateMongo(err, "股票(%s) 社交舆情", code.FullSymbol())
	}
	return dtos.ToDomainSocialPosts(rows), nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// bulkWrite 统一执行批量 upsert。
// Ordered(false)：批内文档彼此独立，一条脏数据不应该让后面几百条全部丢弃，
// 同时无序模式允许 mongo 并行执行，吞吐显著高于有序模式。
func (repo *MarketDataRepository) bulkWrite(ctx context.Context, coll string, models []mongo.WriteModel, subject string) error {
	if len(models) == 0 {
		// 空批次是常态（非交易日、无新资讯），BulkWrite 对空数组会报错，这里直接短路。
		return nil
	}
	_, err := repo.db.Collection(coll).BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return translateMongo(err, "%s", subject)
	}
	return nil
}

// find 是带排序与条数收敛的通用查询。dst 必须是 *[]T。
func (repo *MarketDataRepository) find(ctx context.Context, coll string, filter bson.M, sort bson.D, limit int, dst any) error {
	opts := options.Find().SetSort(sort).SetLimit(int64(clampLimit(limit)))
	cur, err := repo.db.Collection(coll).Find(ctx, filter, opts)
	if err != nil {
		return err
	}
	// cur.All 内部会关闭游标，但出错路径下不会，补一个 defer 防止游标泄漏到服务端超时。
	defer func() { _ = cur.Close(ctx) }()
	return cur.All(ctx, dst)
}

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

// dateStringRange 把日期区间转成针对 YYYY-MM-DD 字符串列的比较条件。
// TradeDate 规范化之后是定长串，字典序即时间序，不需要转成 BSON date。
func dateStringRange(r shared_vo.DateRange) bson.M {
	cond := bson.M{}
	if !r.Start.IsZero() {
		cond["$gte"] = r.Start.String()
	}
	if !r.End.IsZero() {
		cond["$lte"] = r.End.String()
	}
	if len(cond) == 0 {
		return nil
	}
	return cond
}

// dateTimeRange 把日期区间转成针对 time.Time 列的比较条件。
// 结束日用「次日零点之前」的开区间：DateRange 语义上是闭区间，
// 而 2024-01-31 当天 15:30 发布的新闻必须被 End=2024-01-31 的查询命中。
func dateTimeRange(r shared_vo.DateRange) bson.M {
	cond := bson.M{}
	if t, ok := r.Start.Time(); ok {
		// TradeDate.Time() 解析出的是 UTC 零点，而发布时间是本地时刻，
		// 这里按本地时区重建当日零点，否则东八区会整体偏移 8 小时。
		cond["$gte"] = atLocalMidnight(t)
	}
	if t, ok := r.End.Time(); ok {
		cond["$lt"] = atLocalMidnight(t).AddDate(0, 0, 1)
	}
	if len(cond) == 0 {
		return nil
	}
	return cond
}

func atLocalMidnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}
