package repositories

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ===========================================================================
// 选股执行器：把筛选条件下推到数据库
// ===========================================================================
//
// # 本文件存在的全部理由：不要把股票池加载进内存
//
// 天真的实现是「SELECT * FROM stocks（5000 行）→ 逐只取行情 → 在 Go 里 if 一遍」。
// 它有三个各自独立的致命问题：
//
//  1. 逐只取行情是 N+1，5000 次往返，一次筛选几十秒；
//  2. 全量进内存，5000 只 × 行情 + 财务 ≈ 几十 MB，并发十个用户就是 OOM；
//  3. 排序与 limit 只能在内存里做，于是「取市值最大的 50 只」也得先materialize 全部。
//
// 本文件的做法是把**过滤、排序、截断**三件事全部交给数据库：
//
//   - 过滤：每条筛选条件翻译成一个索引可用的谓词，进 WHERE / $match；
//   - 排序：ORDER BY / $sort 由承载排序字段的那个存储执行；
//   - 截断：LIMIT / $limit 跟在排序之后，数据库只返回那 50 行。
//
// 整条路径的往返次数是**常数**（最多 7 次，见 Screen 的注释），
// 与股票池大小无关；进程内驻留的行数是 O(候选代码数 + limit)，不是 O(全市场 × 字段数)。
//
// # 为什么这里没有用 helpers/concurrency 扇出
//
// 因为没有可以扇出的 N 次调用。跨存储的几次查询之间有数据依赖
// （后一次要用前一次筛出的代码集合），并行不了；同一存储内部则本来就是一条语句。
// 「用并发把 5000 次查询跑快一点」正是本文件要消灭的形状，不是要优化的形状。
//
// # 注入防线在哪
//
// 进入语句的标识符（列名 / BSON 键）一律来自 FieldName.Column()，
// 那是 value_objects/field_name.go 里写死的常量表；用户给的字符串只能用来
// **查这张表**，查不到就报错，永远不会被拼进语句。
// 进入语句的值一律走占位符（? / bson 值），一个字符串拼接都没有。
// 见 value_objects 包注释里那段长解释。

const (
	// maxCandidateSymbols 是跨存储求交时单个存储允许返回的代码数上限。
	//
	// 它不是业务语义上的截断，而是一道内存护栏：本上下文的每个候选集
	// 都是「一只股票一条」（行情取最新交易日的横截面、财务取最近一期），
	// 所以它的自然上界就是**股票池大小**——A 股约 5400，加上港股美股也在万级。
	// 20000 这个数因此在真实数据下永远触不到，一旦触到只说明数据出了问题
	// （比如行情集合里混进了多个交易日），那时截断比 OOM 好，而且会如实上报
	// Truncated 让调用方知道结果不完整。
	maxCandidateSymbols = 20000

	// financialLookbackDays 限定「最近一期财报」的回溯窗口。
	//
	// 取最近一期需要按 symbol 分组取第一条，而分组的代价与扫过的文档数成正比。
	// 先用 report_date >= 今天-550 天 把每只票的候选压到 4~6 期（年报 + 季报），
	// 再分组，扫过的文档数就从「全部历史财报」降到「一年半的财报」。
	// 550 天（约 18 个月）的余量能兜住年报延迟披露与停牌补报。
	financialLookbackDays = 550

	collQuotes     = "quotes"
	collFinancials = "financials"
	stocksTable    = "stocks"
)

// StockScreener 把筛选条件翻译成对 MySQL + MongoDB 的只读查询。
//
// 它不是仓储：本上下文的聚合（ScreeningTemplate）不由它持久化，它也不开事务
// （只读路径不需要）。它住在 repositories/ 只是因为它要碰存储句柄，
// 而那是本层的专属权限。对上它以 domain_services.StockScreener 这个
// 由消费方声明的窄接口出现。
type StockScreener struct {
	db    *gorm.DB
	mongo *mongo.Database
}

func NewStockScreener(db *gorm.DB, mongoDB *mongo.Database) *StockScreener {
	return &StockScreener{db: db, mongo: mongoDB}
}

// Screen 执行一次筛选。
//
// ===========================================================================
// 跨存储策略：锚定存储 + 有界代码交集
// ===========================================================================
//
// 筛选条件可能同时落在两个存储上（典型：`pe` 在 Mongo 的 quotes 里，
// `industry` 在 MySQL 的 stocks 里）。没有任何一条语句能同时查两个库，
// 所以必须选一条路。可选的方案与它们的代价：
//
//	A. 各自查全量再在内存里求交          → 就是本文件要消灭的「全量进内存」
//	B. 定时把三个源打平成一张宽表/集合    → 查询是一条语句，但引入了同步延迟、
//	                                       一套新的落库链路和一份冗余存储
//	C. 锚定一个存储，其余存储只回代码集合 → 本文件选的方案
//
// # C 怎么做
//
//  1. 选出**锚定存储**：承载排序字段的那一个。
//     为什么是它——排序与 limit 必须由同一个存储完成，否则「按市值取前 50」
//     就得先把全部候选行拉回内存再排，limit 也就失去了意义。
//  2. 其余每个有条件的存储，各发一条**只投影代码**的查询，拿回符合它自己那部分
//     条件的股票代码集合。只投影代码是关键：一条 5000 个代码的集合是几十 KB，
//     而 5000 行完整行情是几十 MB。
//  3. 把这些集合求交，得到候选代码集。
//  4. 锚定存储执行「自己的条件 + symbol IN 候选集 + ORDER BY + LIMIT」，
//     数据库只返回 limit 行。
//  5. 只为这 limit 只票去各存储补齐要展示的字段值。
//
// # 这个方案的代价（必须说清楚）
//
// 中间那个代码集合会被搬进进程内存一次。它的大小是**股票池量级**（万级字符串，
// 约 1MB），不是「行数 × 字段数」量级，所以可接受；但它确实让本方案的内存占用
// 随股票池线性增长，而方案 B（打平成一张宽表）是常数。
// 另一处代价是这个集合作为 IN 列表发回数据库，会让那条语句的网络包变大，
// 也会让优化器更倾向于用 IN 列表驱动而不是走锚定存储自己的索引。
//
// 换来的是：不需要维护第三份数据，不引入同步延迟，筛选结果永远与源数据一致。
// 在当前规模（单市场 5000~6000 只）下这个权衡是划算的；当股票池上到十万级
// （比如接入全球市场）时，正确的做法是切到方案 B——那时本方法会退化成
// 一条对宽表的查询，而调用方的接口一行都不用改，因为它只认 ScreenQuery。
//
// # 往返次数
//
//	1  取最新交易日（仅当有行情条件或行情排序时）
//	≤2 非锚定存储的代码投影查询
//	1  锚定存储的主查询（含排序与截断）
//	1  锚定存储的计数查询（为了给出「共命中多少只」）
//	≤3 为最终那几十只票补齐展示字段
//	--------------------------------------------------
//	≤8 条，与股票池大小无关，且没有一条在循环里
func (s *StockScreener) Screen(ctx context.Context, q value_objects.ScreenQuery) (value_objects.ScreeningResultSet, error) {
	if q.IsZero() {
		return value_objects.EmptyResultSet(), custom_errors.Invalid("至少需要一个筛选条件")
	}

	bySource := q.CriteriaBySource()
	anchor := q.Sort().Source()
	if anchor == "" {
		// 排序字段无法识别（只可能来自坏数据），回落到默认排序所在的存储。
		anchor = value_objects.DefaultSortSpec().Source()
	}

	// 取最新交易日。它既是行情条件的等值过滤键，也是结果集要回给用户的 AsOf。
	//
	// 为什么必须先定住这一天：quotes 集合里一只票有几百个交易日的文档，
	// 不定住日期就得「按 symbol 分组取最新」，那是一次全集合扫描。
	// 定住之后，筛选退化成对一个 5000 文档横截面的等值查询。
	var asOf shared_vo.TradeDate
	needQuotes := len(bySource[value_objects.FieldSourceQuote]) > 0 ||
		anchor == value_objects.FieldSourceQuote ||
		sourceInOutputs(q, value_objects.FieldSourceQuote)
	if needQuotes {
		d, err := s.latestQuoteDate(ctx)
		if err != nil {
			return value_objects.EmptyResultSet(), err
		}
		if d.IsZero() {
			// 一条行情都没有，说明同步还没跑过。返回空集而不是报错：
			// 「今天还没有行情数据」是一个用户看得懂的空结果，不是一次故障。
			return value_objects.EmptyResultSet(), nil
		}
		asOf = d
	}

	// --- 第 2、3 步：非锚定存储投影出代码集合并求交 ---
	//
	// stocks 无条件参与：它带着一条隐含谓词 delisted = 0。退市股必须被排除在
	// 选股结果之外（它们不可交易），而这条约束只有 stocks 表知道。
	// 让它总是参与，退市过滤就不必在别的地方补第二次。
	candidates := newCandidateSet()
	for _, src := range []value_objects.FieldSource{
		value_objects.FieldSourceStock,
		value_objects.FieldSourceQuote,
		value_objects.FieldSourceFinancial,
	} {
		if src == anchor {
			continue
		}
		criteria := bySource[src]
		if len(criteria) == 0 && src != value_objects.FieldSourceStock {
			continue
		}
		symbols, truncated, err := s.matchSymbols(ctx, src, criteria, asOf)
		if err != nil {
			return value_objects.EmptyResultSet(), err
		}
		candidates.intersect(symbols, truncated)
		if candidates.empty() {
			// 提前退出：交集已经空了，后面的查询不会让它再变回非空。
			// 这一步省下的是实打实的往返，不是微优化。
			return value_objects.ScreeningResultSet{
				Results: []value_objects.ScreeningResult{}, AsOf: asOf,
				Truncated: candidates.truncated,
			}, nil
		}
	}

	// --- 第 4 步：锚定存储执行主查询，排序与截断都在库里完成 ---
	rows, total, err := s.queryAnchor(ctx, anchor, bySource[anchor], candidates.list(), q, asOf)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	if len(rows) == 0 {
		return value_objects.ScreeningResultSet{
			Results: []value_objects.ScreeningResult{}, Total: total, AsOf: asOf,
			Truncated: candidates.truncated,
		}, nil
	}

	// --- 第 5 步：只为这几十只票补齐展示字段 ---
	results, err := s.hydrate(ctx, rows, q.OutputFields(), asOf)
	if err != nil {
		return value_objects.EmptyResultSet(), err
	}
	return value_objects.ScreeningResultSet{
		Results:   results,
		Total:     total,
		AsOf:      asOf,
		Truncated: candidates.truncated,
	}, nil
}

// ---------------------------------------------------------------------------
// 候选代码集合
// ---------------------------------------------------------------------------

// candidateSet 是跨存储求交的中间结果。
//
// nil 的 symbols 表示「尚未受到任何约束」，与「约束到了空集」是两回事：
// 前者意味着锚定查询不加 IN 条件，后者意味着直接返回空结果。
// 用一个布尔量区分它们，而不是靠 len == 0 猜——猜错的那一次会把
// 「没有任何限制」变成「什么都不返回」。
type candidateSet struct {
	symbols     map[string]struct{}
	constrained bool
	truncated   bool
}

func newCandidateSet() *candidateSet { return &candidateSet{} }

func (cs *candidateSet) intersect(symbols []string, truncated bool) {
	cs.truncated = cs.truncated || truncated
	if !cs.constrained {
		cs.symbols = make(map[string]struct{}, len(symbols))
		for _, s := range symbols {
			cs.symbols[s] = struct{}{}
		}
		cs.constrained = true
		return
	}
	next := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		if _, ok := cs.symbols[s]; ok {
			next[s] = struct{}{}
		}
	}
	cs.symbols = next
}

func (cs *candidateSet) empty() bool { return cs.constrained && len(cs.symbols) == 0 }

// list 把集合摊成切片供 IN 条件使用；未受约束时返回 nil，调用方据此不加 IN。
func (cs *candidateSet) list() []string {
	if !cs.constrained {
		return nil
	}
	out := make([]string, 0, len(cs.symbols))
	for s := range cs.symbols {
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------------------
// 分存储的代码投影
// ---------------------------------------------------------------------------

// screenRow 是锚定查询返回的一行，只含定位一只股票所需的最小信息。
//
// 刻意不含任何指标值：指标由第 5 步统一补齐。让锚定查询多带几列看起来能省一次
// 查询，但它带不全——锚定存储只有自己那部分字段，跨存储的字段照样要补。
type screenRow struct {
	Symbol string
	Market string
	Raw    string
	Name   string
}

// matchSymbols 在一个非锚定存储上执行它那部分条件，只投影股票代码。
//
// 交集的键是 symbol 而不是 (market, symbol)：Mongo 侧的索引前缀是 symbol，
// 用复合键求交就用不上索引。这在本项目的代码规范化口径下是安全的——
// A 股 6 位数字、港股补齐到 5 位数字、美股是字母，三者不可能撞号
// （见 shared_vo.NewStockCode）。真到了需要跨市场同号的那天，
// 要改的是这一处的键，以及 Mongo 的索引。
func (s *StockScreener) matchSymbols(
	ctx context.Context,
	src value_objects.FieldSource,
	criteria []value_objects.Criterion,
	asOf shared_vo.TradeDate,
) ([]string, bool, error) {
	switch src {
	case value_objects.FieldSourceStock:
		return s.matchStockSymbols(ctx, criteria)
	case value_objects.FieldSourceQuote:
		return s.matchQuoteSymbols(ctx, criteria, asOf)
	case value_objects.FieldSourceFinancial:
		return s.matchFinancialSymbols(ctx, criteria)
	default:
		return nil, false, custom_errors.Internal("未知的筛选数据源: %s", src)
	}
}

// matchStockSymbols 在 stocks 表上筛出代码。
//
// SELECT 只投影 symbol 一列：MySQL 因此有机会走**覆盖索引**
// （idx_stocks_industry 这类二级索引的叶子里就带着主键，回表都省了）。
// 一旦把 SELECT * 写在这里，每一行都要回表读整条记录，5000 次回表。
func (s *StockScreener) matchStockSymbols(ctx context.Context, criteria []value_objects.Criterion) ([]string, bool, error) {
	q, err := s.stockQuery(ctx, criteria, nil)
	if err != nil {
		return nil, false, err
	}
	var symbols []string
	// Pluck 自己会把 SELECT 收敛成单列，不必再叠一个 Select。
	// Limit 是护栏不是分页：正常数据下永远取不满（见 maxCandidateSymbols）。
	// 多要一条是为了能分辨「刚好等于上限」与「被截断了」。
	err = q.Limit(maxCandidateSymbols+1).Pluck("symbol", &symbols).Error
	if err != nil {
		return nil, false, translate(err, "按条件筛选股票主数据")
	}
	if len(symbols) > maxCandidateSymbols {
		return symbols[:maxCandidateSymbols], true, nil
	}
	return symbols, false, nil
}

// matchQuoteSymbols 在 quotes 集合的最新交易日横截面上筛出代码。
func (s *StockScreener) matchQuoteSymbols(
	ctx context.Context, criteria []value_objects.Criterion, asOf shared_vo.TradeDate,
) ([]string, bool, error) {
	if s.mongo == nil {
		return nil, false, custom_errors.Unavailable("行情存储未配置，无法按量价指标筛选")
	}
	filter, err := mongoFilter(criteria, bson.M{"trade_date": asOf.String()}, nil)
	if err != nil {
		return nil, false, err
	}
	opts := options.Find().
		// 只投影 symbol（并显式关掉 _id）：投影是 Mongo 这边「不要把全量数据
		// 拉进内存」的等价手段，返回的文档从几百字节压到十几字节。
		SetProjection(bson.M{"symbol": 1, "_id": 0}).
		SetLimit(maxCandidateSymbols + 1)
	return s.pluckSymbols(ctx, collQuotes, filter, opts, "按量价指标筛选")
}

// matchFinancialSymbols 在「每只票最近一期财报」上筛出代码。
//
// 管道的每一级都是有意为之，顺序不能换：
//
//	$match(report_date >= cutoff)  先把文档数从「全部历史」压到一年半，
//	                              走 uk_financials_symbol_report_date 的区间扫描
//	$sort(symbol, report_date desc) 与上述索引同序，不产生额外的内存排序
//	$group(first)                  每只票只留最新那一期
//	$match(用户条件)                在横截面上过滤
//	$project(symbol)               只带回代码
//
// 用户条件为什么不能提到 $group 之前：那会变成「历史上任何一期满足条件即命中」，
// 而选股要问的是「**当前**的 ROE 大于 15」。三年前达标、如今亏损的公司
// 不该出现在结果里。
func (s *StockScreener) matchFinancialSymbols(ctx context.Context, criteria []value_objects.Criterion) ([]string, bool, error) {
	if s.mongo == nil {
		return nil, false, custom_errors.Unavailable("财务数据存储未配置，无法按财务指标筛选")
	}
	pipeline, err := s.latestFinancialPipeline(criteria, nil)
	if err != nil {
		return nil, false, err
	}
	pipeline = append(pipeline,
		bson.D{{Key: "$limit", Value: int64(maxCandidateSymbols + 1)}},
		bson.D{{Key: "$project", Value: bson.M{"symbol": 1, "_id": 0}}},
	)
	return s.aggregateSymbols(ctx, collFinancials, pipeline, "按财务指标筛选")
}

// ---------------------------------------------------------------------------
// 锚定存储主查询
// ---------------------------------------------------------------------------

// queryAnchor 在锚定存储上执行主查询：条件 + 候选代码 + 排序 + 截断，
// 外加一次计数。返回的行数恒等于 min(命中数, limit)。
func (s *StockScreener) queryAnchor(
	ctx context.Context,
	anchor value_objects.FieldSource,
	criteria []value_objects.Criterion,
	candidates []string,
	q value_objects.ScreenQuery,
	asOf shared_vo.TradeDate,
) ([]screenRow, int64, error) {
	switch anchor {
	case value_objects.FieldSourceStock:
		return s.queryStockAnchor(ctx, criteria, candidates, q)
	case value_objects.FieldSourceQuote:
		return s.queryQuoteAnchor(ctx, criteria, candidates, q, asOf)
	case value_objects.FieldSourceFinancial:
		return s.queryFinancialAnchor(ctx, criteria, candidates, q)
	default:
		return nil, 0, custom_errors.Internal("未知的锚定数据源: %s", anchor)
	}
}

func (s *StockScreener) queryStockAnchor(
	ctx context.Context,
	criteria []value_objects.Criterion,
	candidates []string,
	q value_objects.ScreenQuery,
) ([]screenRow, int64, error) {
	base, err := s.stockQuery(ctx, criteria, candidates)
	if err != nil {
		return nil, 0, err
	}
	base = base.Session(&gorm.Session{})

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, translate(err, "统计筛选命中数")
	}
	if total == 0 {
		return nil, 0, nil
	}

	orderBy, err := sqlOrderBy(q.Sort())
	if err != nil {
		return nil, 0, err
	}
	var rows []screenRow
	err = base.Select("symbol", "market", "raw_code AS raw", "name").
		Order(orderBy).
		Limit(q.Limit()).
		Find(&rows).Error
	if err != nil {
		return nil, 0, translate(err, "执行选股筛选")
	}
	return rows, total, nil
}

func (s *StockScreener) queryQuoteAnchor(
	ctx context.Context,
	criteria []value_objects.Criterion,
	candidates []string,
	q value_objects.ScreenQuery,
	asOf shared_vo.TradeDate,
) ([]screenRow, int64, error) {
	if s.mongo == nil {
		return nil, 0, custom_errors.Unavailable("行情存储未配置，无法按量价指标筛选")
	}
	filter, err := mongoFilter(criteria, bson.M{"trade_date": asOf.String()}, candidates)
	if err != nil {
		return nil, 0, err
	}
	coll := s.mongo.Collection(collQuotes)

	total, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, translate(err, "统计筛选命中数")
	}
	if total == 0 {
		return nil, 0, nil
	}

	sortDoc, err := mongoSort(q.Sort())
	if err != nil {
		return nil, 0, err
	}
	// $sort + $limit 交给 Mongo：它会在排序阶段只保留 top-N，
	// 不会把全部命中文档物化出来。
	opts := options.Find().
		SetSort(sortDoc).
		SetLimit(int64(q.Limit())).
		SetProjection(bson.M{"symbol": 1, "market": 1, "raw_code": 1, "_id": 0})

	rows, err := s.findScreenRows(ctx, coll, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *StockScreener) queryFinancialAnchor(
	ctx context.Context,
	criteria []value_objects.Criterion,
	candidates []string,
	q value_objects.ScreenQuery,
) ([]screenRow, int64, error) {
	if s.mongo == nil {
		return nil, 0, custom_errors.Unavailable("财务数据存储未配置，无法按财务指标筛选")
	}
	pipeline, err := s.latestFinancialPipeline(criteria, candidates)
	if err != nil {
		return nil, 0, err
	}
	coll := s.mongo.Collection(collFinancials)

	// 计数单独跑一条 $count 管道。它确实让这条路径多一次聚合，
	// 但「共命中多少只」是结果页必须显示的信息——只给一页数据，
	// 用户会以为全市场只有这几十只票符合条件。
	total, err := s.countPipeline(ctx, coll, pipeline)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	sortDoc, err := mongoSort(q.Sort())
	if err != nil {
		return nil, 0, err
	}
	pipeline = append(pipeline,
		bson.D{{Key: "$sort", Value: sortDoc}},
		bson.D{{Key: "$limit", Value: int64(q.Limit())}},
		bson.D{{Key: "$project", Value: bson.M{"symbol": 1, "market": 1, "raw_code": 1, "_id": 0}}},
	)

	cur, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, translate(err, "执行选股筛选")
	}
	defer func() { _ = cur.Close(ctx) }()
	var docs []symbolDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, 0, translate(err, "执行选股筛选")
	}
	rows := make([]screenRow, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, screenRow{Symbol: d.Symbol, Market: d.Market, Raw: d.Raw})
	}
	return rows, total, nil
}

// ---------------------------------------------------------------------------
// 展示字段补齐
// ---------------------------------------------------------------------------

// hydrate 为最终入选的那几十只票补齐要展示的字段值。
//
// # 为什么这一步不是 N+1
//
// 每个存储**一条**查询，用 IN 一次取回全部入选标的的值，再在内存里按 symbol 缝合。
// 绝不是「对每只票查一次行情」——那正是本文件开头列举的第 1 个致命问题。
// 入选标的最多 MaxResultLimit（500）只，一条 IN 查询的代价完全可控。
//
// # 为什么必须查 stocks
//
// 结果要显示股票名称，而名称只有 stocks 表有。锚定存储是 Mongo 时这一条
// 查询不可省；锚定存储是 stocks 时名称已经在主查询里带回来了，
// 那条查询就只用来补 industry / total_mv 这类字段（且仅当它们被筛选或排序用到）。
func (s *StockScreener) hydrate(
	ctx context.Context,
	rows []screenRow,
	fields []value_objects.FieldName,
	asOf shared_vo.TradeDate,
) ([]value_objects.ScreeningResult, error) {
	symbols := make([]string, 0, len(rows))
	needName := false
	for _, r := range rows {
		symbols = append(symbols, r.Symbol)
		if r.Name == "" {
			needName = true
		}
	}

	byField := groupFieldsBySource(fields)

	// 一个存储一条查询，三个存储最多三条。
	stockValues := map[string]map[string]any{}
	if needName || len(byField[value_objects.FieldSourceStock]) > 0 {
		v, names, err := s.loadStockValues(ctx, symbols, byField[value_objects.FieldSourceStock])
		if err != nil {
			return nil, err
		}
		stockValues = v
		for i := range rows {
			if rows[i].Name == "" {
				rows[i].Name = names[rows[i].Symbol]
			}
		}
	}

	quoteValues := map[string]map[string]any{}
	if len(byField[value_objects.FieldSourceQuote]) > 0 && !asOf.IsZero() {
		v, err := s.loadQuoteValues(ctx, symbols, byField[value_objects.FieldSourceQuote], asOf)
		if err != nil {
			return nil, err
		}
		quoteValues = v
	}

	financialValues := map[string]map[string]any{}
	if len(byField[value_objects.FieldSourceFinancial]) > 0 {
		v, err := s.loadFinancialValues(ctx, symbols, byField[value_objects.FieldSourceFinancial])
		if err != nil {
			return nil, err
		}
		financialValues = v
	}

	perSource := map[value_objects.FieldSource]map[string]map[string]any{
		value_objects.FieldSourceStock:     stockValues,
		value_objects.FieldSourceQuote:     quoteValues,
		value_objects.FieldSourceFinancial: financialValues,
	}

	out := make([]value_objects.ScreeningResult, 0, len(rows))
	for _, r := range rows {
		// 这里直接拼装 StockCode 而不走 shared_vo.NewStockCode：库里的代码
		// 在写入时已经规范化过，读路径再校验一次只会让历史数据整条查不出来。
		res := value_objects.ScreeningResult{
			Code:   shared_vo.StockCode{Symbol: r.Symbol, Market: shared_vo.Market(r.Market), Raw: r.Raw},
			Name:   r.Name,
			Fields: make([]value_objects.FieldValue, 0, len(fields)),
		}
		// 按 fields 的顺序输出——那是用户填写条件的顺序，也是前端表格的列顺序。
		for _, f := range fields {
			res.Fields = append(res.Fields, buildFieldValue(f, perSource[f.Source()][r.Symbol]))
		}
		out = append(out, res)
	}
	return out, nil
}

// buildFieldValue 把一个原始列值转成结果值对象。
//
// 取不到值时返回 MissingValue 而不是 0：一只刚上市、财报还没披露的票，
// ROE 是「没有」，不是「0%」。把两者混为一谈会让它在按 ROE 排序时
// 排到亏损股中间，用户无从判断那到底是数据缺失还是真的很差。
func buildFieldValue(f value_objects.FieldName, row map[string]any) value_objects.FieldValue {
	if row == nil {
		return value_objects.MissingValue(f)
	}
	raw, ok := row[f.Column()]
	if !ok || raw == nil {
		return value_objects.MissingValue(f)
	}
	if f.IsNumeric() {
		n, ok := toDecimal(raw)
		if !ok {
			return value_objects.MissingValue(f)
		}
		return value_objects.NumberValue(f, n)
	}
	str, ok := raw.(string)
	if !ok {
		return value_objects.MissingValue(f)
	}
	return value_objects.TextValue(f, str)
}
