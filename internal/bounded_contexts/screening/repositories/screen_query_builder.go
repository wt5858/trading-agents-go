package repositories

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 本文件是「筛选条件 → 查询语句」的翻译层，也是注入防线落地的地方。
//
// 两条铁律，每个函数都遵守：
//
//	标识符（列名 / BSON 键）只能来自 FieldName.Column()——那是 value_objects
//	里写死的常量表。用户给的字符串只用来查这张表，查不到就报错。
//	值只能走占位符（? / bson 值），任何情况下都不做字符串拼接。
//
// 于是本文件里唯一被拼进语句文本的东西，是我们自己写在注册表里的列名，
// 以及一组 switch 里的 SQL 关键字。用户输入没有任何一条路径能到达语句文本。

// ---------------------------------------------------------------------------
// MySQL 侧
// ---------------------------------------------------------------------------

// stockQuery 构造针对 stocks 表的基础查询：退市过滤 + 用户条件 + 候选代码。
//
// delisted = false 是**无条件**加上的，不受用户条件影响：退市股不可交易，
// 出现在选股结果里只会误导。把它放在查询构造的唯一入口处，
// 就不会出现「某条筛选路径忘了排除退市股」的那一天。
func (s *StockScreener) stockQuery(
	ctx context.Context, criteria []value_objects.Criterion, candidates []string,
) (*gorm.DB, error) {
	q := s.db.WithContext(ctx).Table(stocksTable).Where("delisted = ?", false)
	for _, c := range criteria {
		expr, args, err := sqlPredicate(c)
		if err != nil {
			return nil, err
		}
		q = q.Where(expr, args...)
	}
	if candidates != nil {
		// 跨存储交集作为 IN 条件下推。它可能有几千个元素——那是本方案
		// 明确接受的代价，见 Screen 的长注释。
		q = q.Where("symbol IN ?", candidates)
	}
	return q, nil
}

// sqlPredicate 把一条筛选条件翻译成 (SQL 片段, 绑定参数)。
//
// 片段里出现的唯一变量是 col，而 col 来自封闭枚举的注册表。
// 值一律是 ?，由驱动转义。这就是为什么用户传进来的 `pe` 也好、
// `pe; DROP TABLE stocks--` 也好，后者根本走不到这里——
// NewFieldName 在 HTTP 边界就把它拒了，而即便它以某种方式混进了库，
// FieldName.Column() 也只会返回空串，被下面第一行挡住。
func sqlPredicate(c value_objects.Criterion) (string, []any, error) {
	col := c.Field().Column()
	if col == "" {
		return "", nil, custom_errors.Invalid("筛选条件包含无法识别的字段: %s", c.Field().String())
	}
	// 反引号包裹是为了兼容与 MySQL 关键字同名的列（比如 `close`），
	// 不是为了转义用户输入——这里根本没有用户输入。
	ref := "`" + col + "`"
	args := c.Args()

	switch c.Operator() {
	case value_objects.OpGT:
		return ref + " > ?", args[:1], nil
	case value_objects.OpGTE:
		return ref + " >= ?", args[:1], nil
	case value_objects.OpLT:
		return ref + " < ?", args[:1], nil
	case value_objects.OpLTE:
		return ref + " <= ?", args[:1], nil
	case value_objects.OpEQ:
		return ref + " = ?", args[:1], nil
	case value_objects.OpNE:
		return ref + " <> ?", args[:1], nil
	case value_objects.OpBetween:
		// 元数（恰好 2 个）已经由 NewCriterion 保证，这里不再判——
		// 判两次意味着两个定义点。真要出问题也是编译期就能发现的越界 panic，
		// 而不是一条语义被悄悄改写的 SQL。
		return ref + " BETWEEN ? AND ?", args[:2], nil
	case value_objects.OpIn:
		return ref + " IN ?", []any{args}, nil
	case value_objects.OpNotIn:
		return ref + " NOT IN ?", []any{args}, nil
	default:
		return "", nil, custom_errors.Invalid("不支持的比较符: %s", c.Operator())
	}
}

// sqlOrderBy 构造 ORDER BY 子句。
//
// 末尾恒定补一个 symbol 作为 tie-break：排序字段出现并列时（比如一批
// PE 都是 0 的未盈利股），没有次级排序键的话每次查询返回的顺序都可能不同，
// 用户会以为结果在随机跳动。
func sqlOrderBy(sort value_objects.SortSpec) (string, error) {
	sort = sort.OrDefault()
	col := sort.Field().Column()
	if col == "" {
		return "", custom_errors.Invalid("排序字段无法识别: %s", sort.Field().String())
	}
	dir := "ASC"
	if sort.Descending() {
		dir = "DESC"
	}
	return "`" + col + "` " + dir + ", `symbol` ASC", nil
}

// loadStockValues 一条查询取回全部入选标的在 stocks 表上的展示字段与名称。
//
// 返回两张表而不是把名称塞进值表：名称不是一个可筛选字段，
// 混进去会让它出现在结果的 Fields 列表里，而那份列表的语义是
// 「用户筛选所依据的指标」。
func (s *StockScreener) loadStockValues(
	ctx context.Context, symbols []string, fields []value_objects.FieldName,
) (values map[string]map[string]any, names map[string]string, err error) {
	cols := []string{"symbol", "name"}
	for _, f := range fields {
		if col := f.Column(); col != "" {
			cols = append(cols, "`"+col+"`")
		}
	}

	var rows []map[string]any
	err = s.db.WithContext(ctx).Table(stocksTable).
		Select(cols).
		Where("symbol IN ?", symbols).
		Find(&rows).Error
	if err != nil {
		return nil, nil, translate(err, "读取股票主数据")
	}

	values = make(map[string]map[string]any, len(rows))
	names = make(map[string]string, len(rows))
	for _, row := range rows {
		sym, _ := row["symbol"].(string)
		if sym == "" {
			continue
		}
		values[sym] = row
		names[sym] = toString(row["name"])
	}
	return values, names, nil
}

// ---------------------------------------------------------------------------
// MongoDB 侧
// ---------------------------------------------------------------------------

// symbolDoc 是只投影代码时的解码目标。
type symbolDoc struct {
	Symbol string `bson:"symbol"`
	Market string `bson:"market"`
	Raw    string `bson:"raw_code"`
}

// mongoFilter 把一组筛选条件翻译成 Mongo 查询文档。
//
// 全部条件平铺进一个 $and 数组，而不是按键合并成 {pe: {$gt: 10, $lt: 30}}。
// 合并写法在同一字段上出现两条区间条件时会静默丢掉一条
// （比如 pe >= 5 与 pe between 10 and 20 都要写 $gte，后者覆盖前者），
// 而 $and 数组没有这个问题，索引的使用也不受影响。
func mongoFilter(criteria []value_objects.Criterion, base bson.M, candidates []string) (bson.M, error) {
	conds := make(bson.A, 0, len(criteria)+2)
	for k, v := range base {
		conds = append(conds, bson.M{k: v})
	}
	if candidates != nil {
		conds = append(conds, bson.M{"symbol": bson.M{"$in": candidates}})
	}
	for _, c := range criteria {
		key := c.Field().Column()
		if key == "" {
			return nil, custom_errors.Invalid("筛选条件包含无法识别的字段: %s", c.Field().String())
		}
		cond, err := mongoCondition(c)
		if err != nil {
			return nil, err
		}
		conds = append(conds, bson.M{key: cond})
	}
	if len(conds) == 0 {
		return bson.M{}, nil
	}
	return bson.M{"$and": conds}, nil
}

// mongoCondition 是比较符到 Mongo 操作符的映射。
//
// 与 sqlPredicate 一样，出现在结果里的操作符都是写死的常量，
// 用户给的值只作为 BSON 值出现——BSON 没有「把值当语句解析」这回事，
// 所以这一侧天然没有注入面；真正的风险在键名上，而键名来自注册表。
func mongoCondition(c value_objects.Criterion) (bson.M, error) {
	args := c.Args()
	switch c.Operator() {
	case value_objects.OpGT:
		return bson.M{"$gt": args[0]}, nil
	case value_objects.OpGTE:
		return bson.M{"$gte": args[0]}, nil
	case value_objects.OpLT:
		return bson.M{"$lt": args[0]}, nil
	case value_objects.OpLTE:
		return bson.M{"$lte": args[0]}, nil
	case value_objects.OpEQ:
		return bson.M{"$eq": args[0]}, nil
	case value_objects.OpNE:
		return bson.M{"$ne": args[0]}, nil
	case value_objects.OpBetween:
		return bson.M{"$gte": args[0], "$lte": args[1]}, nil
	case value_objects.OpIn:
		return bson.M{"$in": args}, nil
	case value_objects.OpNotIn:
		return bson.M{"$nin": args}, nil
	default:
		return nil, custom_errors.Invalid("不支持的比较符: %s", c.Operator())
	}
}

// mongoSort 构造排序文档，同样补 symbol 做 tie-break，理由见 sqlOrderBy。
func mongoSort(sort value_objects.SortSpec) (bson.D, error) {
	sort = sort.OrDefault()
	key := sort.Field().Column()
	if key == "" {
		return nil, custom_errors.Invalid("排序字段无法识别: %s", sort.Field().String())
	}
	dir := 1
	if sort.Descending() {
		dir = -1
	}
	return bson.D{{Key: key, Value: dir}, {Key: "symbol", Value: 1}}, nil
}

// latestQuoteDate 取行情集合里最新的交易日。
//
// 一条 find + sort + limit(1)，走 (symbol, trade_date desc) 索引的逆序扫描，
// 只读一条文档。它存在的意义是把后续所有行情查询从「按 symbol 分组取最新」
// （全集合扫描）降级成「trade_date 等值匹配」（一个 5000 文档的横截面）。
//
// 代价是：在最新交易日停牌、没有行情文档的标的会被排除在筛选之外。
// 这对选股来说恰恰是对的——不能交易的票不该出现在选股结果里。
func (s *StockScreener) latestQuoteDate(ctx context.Context) (shared_vo.TradeDate, error) {
	if s.mongo == nil {
		return shared_vo.TradeDate{}, custom_errors.Unavailable("行情存储未配置，无法按量价指标筛选")
	}
	var doc struct {
		TradeDate string `bson:"trade_date"`
	}
	err := s.mongo.Collection(collQuotes).FindOne(ctx, bson.M{},
		options.FindOne().
			SetSort(bson.D{{Key: "trade_date", Value: -1}}).
			SetProjection(bson.M{"trade_date": 1, "_id": 0}),
	).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// 没有任何行情数据不是故障，是「同步还没跑过」。
			return shared_vo.TradeDate{}, nil
		}
		return shared_vo.TradeDate{}, translate(err, "最新交易日")
	}
	return shared_vo.MustTradeDate(doc.TradeDate), nil
}

// latestFinancialPipeline 构造「每只票最近一期财报」的聚合管道前半段。
//
// 阶段顺序的理由见 matchFinancialSymbols 的注释，核心是两条：
// 先用 report_date 区间把扫描量压下来，再分组；用户条件必须在分组之后，
// 否则会变成「历史上任何一期达标即命中」。
func (s *StockScreener) latestFinancialPipeline(
	criteria []value_objects.Criterion, candidates []string,
) (mongo.Pipeline, error) {
	cutoff := shared_vo.Today().AddDays(-financialLookbackDays).String()
	pre := bson.M{"report_date": bson.M{"$gte": cutoff}}
	if candidates != nil {
		// 候选代码尽可能提前：它把 $group 要处理的文档数从「全市场一年半」
		// 压到「候选集一年半」，是这条管道上最有效的一次剪枝。
		pre["symbol"] = bson.M{"$in": candidates}
	}

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: pre}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "symbol", Value: 1}, {Key: "report_date", Value: -1}}}},
		// $$ROOT 整份文档进 $first，出来的子文档可以直接被 $replaceRoot 摊平，
		// 省掉逐字段罗列——加一列财务字段时这里不用跟着改。
		bson.D{{Key: "$group", Value: bson.M{"_id": "$symbol", "latest": bson.M{"$first": "$$ROOT"}}}},
		bson.D{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$latest"}}},
	}
	if len(criteria) > 0 {
		filter, err := mongoFilter(criteria, nil, nil)
		if err != nil {
			return nil, err
		}
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: filter}})
	}
	return pipeline, nil
}

// pluckSymbols 执行一次只投影代码的 find。
func (s *StockScreener) pluckSymbols(
	ctx context.Context, coll string, filter bson.M, opts *options.FindOptions, subject string,
) ([]string, bool, error) {
	cur, err := s.mongo.Collection(coll).Find(ctx, filter, opts)
	if err != nil {
		return nil, false, translate(err, subject)
	}
	// cur.All 内部会关闭游标，但出错路径下不会，补一个 defer 防止游标泄漏。
	defer func() { _ = cur.Close(ctx) }()

	var docs []symbolDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, false, translate(err, subject)
	}
	symbols := make([]string, 0, len(docs))
	for _, d := range docs {
		symbols = append(symbols, d.Symbol)
	}
	if len(symbols) > maxCandidateSymbols {
		return symbols[:maxCandidateSymbols], true, nil
	}
	return symbols, false, nil
}

// aggregateSymbols 执行一次只投影代码的聚合。
func (s *StockScreener) aggregateSymbols(
	ctx context.Context, coll string, pipeline mongo.Pipeline, subject string,
) ([]string, bool, error) {
	cur, err := s.mongo.Collection(coll).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, false, translate(err, subject)
	}
	defer func() { _ = cur.Close(ctx) }()

	var docs []symbolDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, false, translate(err, subject)
	}
	symbols := make([]string, 0, len(docs))
	for _, d := range docs {
		symbols = append(symbols, d.Symbol)
	}
	if len(symbols) > maxCandidateSymbols {
		return symbols[:maxCandidateSymbols], true, nil
	}
	return symbols, false, nil
}

// findScreenRows 把一次 find 的结果解码成锚定行。
func (s *StockScreener) findScreenRows(
	ctx context.Context, coll *mongo.Collection, filter bson.M, opts *options.FindOptions,
) ([]screenRow, error) {
	cur, err := coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, translate(err, "执行选股筛选")
	}
	defer func() { _ = cur.Close(ctx) }()

	var docs []symbolDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, translate(err, "执行选股筛选")
	}
	rows := make([]screenRow, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, screenRow{Symbol: d.Symbol, Market: d.Market, Raw: d.Raw})
	}
	return rows, nil
}

// countPipeline 复用主管道算一次命中数。
func (s *StockScreener) countPipeline(ctx context.Context, coll *mongo.Collection, pipeline mongo.Pipeline) (int64, error) {
	counting := make(mongo.Pipeline, len(pipeline), len(pipeline)+1)
	// 必须拷贝：pipeline 随后还要追加 $sort/$limit，直接 append 到原切片上
	// 会在容量够用时改写底层数组，让两条管道互相污染。
	copy(counting, pipeline)
	counting = append(counting, bson.D{{Key: "$count", Value: "n"}})

	cur, err := coll.Aggregate(ctx, counting)
	if err != nil {
		return 0, translate(err, "统计筛选命中数")
	}
	defer func() { _ = cur.Close(ctx) }()

	var docs []struct {
		N int64 `bson:"n"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return 0, translate(err, "统计筛选命中数")
	}
	if len(docs) == 0 {
		return 0, nil
	}
	return docs[0].N, nil
}

// loadQuoteValues 一条查询取回入选标的的行情展示字段。
func (s *StockScreener) loadQuoteValues(
	ctx context.Context, symbols []string, fields []value_objects.FieldName, asOf shared_vo.TradeDate,
) (map[string]map[string]any, error) {
	projection := bson.M{"symbol": 1, "_id": 0}
	for _, f := range fields {
		if col := f.Column(); col != "" {
			projection[col] = 1
		}
	}
	cur, err := s.mongo.Collection(collQuotes).Find(ctx,
		bson.M{"trade_date": asOf.String(), "symbol": bson.M{"$in": symbols}},
		options.Find().SetProjection(projection),
	)
	if err != nil {
		return nil, translate(err, "读取行情指标")
	}
	defer func() { _ = cur.Close(ctx) }()
	return decodeValueDocs(ctx, cur, "读取行情指标")
}

// loadFinancialValues 一条聚合取回入选标的最近一期财报的展示字段。
func (s *StockScreener) loadFinancialValues(
	ctx context.Context, symbols []string, fields []value_objects.FieldName,
) (map[string]map[string]any, error) {
	pipeline, err := s.latestFinancialPipeline(nil, symbols)
	if err != nil {
		return nil, err
	}
	projection := bson.M{"symbol": 1, "_id": 0}
	for _, f := range fields {
		if col := f.Column(); col != "" {
			projection[col] = 1
		}
	}
	pipeline = append(pipeline, bson.D{{Key: "$project", Value: projection}})

	cur, err := s.mongo.Collection(collFinancials).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, translate(err, "读取财务指标")
	}
	defer func() { _ = cur.Close(ctx) }()
	return decodeValueDocs(ctx, cur, "读取财务指标")
}

func decodeValueDocs(ctx context.Context, cur *mongo.Cursor, subject string) (map[string]map[string]any, error) {
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, translate(err, subject)
	}
	out := make(map[string]map[string]any, len(docs))
	for _, d := range docs {
		sym, _ := d["symbol"].(string)
		if sym == "" {
			continue
		}
		row := make(map[string]any, len(d))
		for k, v := range d {
			row[k] = v
		}
		out[sym] = row
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// groupFieldsBySource 按存储把要展示的字段分组，让补齐阶段做到「一个存储一条查询」。
func groupFieldsBySource(fields []value_objects.FieldName) map[value_objects.FieldSource][]value_objects.FieldName {
	out := make(map[value_objects.FieldSource][]value_objects.FieldName, 3)
	for _, f := range fields {
		if f.IsZero() {
			continue
		}
		out[f.Source()] = append(out[f.Source()], f)
	}
	return out
}

// sourceInOutputs 判断某个存储是否出现在要展示的字段里。
// 它决定「要不要为了补齐展示值而多取一次最新交易日」。
func sourceInOutputs(q value_objects.ScreenQuery, src value_objects.FieldSource) bool {
	for _, f := range q.OutputFields() {
		if f.Source() == src {
			return true
		}
	}
	return false
}

// toDecimal 把存储返回的原始值转成 decimal.Decimal。
//
// 分支这么多不是防御性编程过度：decimal 列在 MySQL 驱动那里回来的是 []byte
// （驱动不敢替你损失精度），Mongo 那边同一个字段可能是 int32 / int64 / double
// （取决于写入时的数值形态）。少一个分支，某一列就会静默变成「无数据」，
// 而那种缺失在界面上和真正的缺失长得一模一样。
func toDecimal(v any) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case decimal.Decimal:
		// Mongo 侧经编解码器解出来的就是它；MySQL 侧走下面的 []byte 分支。
		return n, true
	case primitive.Decimal128:
		d, err := decimal.NewFromString(n.String())
		return d, err == nil
	case float64:
		return decimal.NewFromFloat(n), true
	case float32:
		return decimal.NewFromFloat32(n), true
	case int:
		return decimal.NewFromInt(int64(n)), true
	case int32:
		return decimal.NewFromInt32(n), true
	case int64:
		return decimal.NewFromInt(n), true
	case uint64:
		return decimal.NewFromInt(int64(n)), true
	case []byte:
		// MySQL 的 DECIMAL 列经 database/sql 出来就是字节串，
		// 直接 NewFromString 是无损的——这正是当初用 ParseFloat 会丢精度的地方。
		d, err := decimal.NewFromString(strings.TrimSpace(string(n)))
		return d, err == nil
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(n))
		return d, err == nil
	default:
		return decimal.Zero, false
	}
}

func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}
