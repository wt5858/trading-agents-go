// Package domain_services 编排股票上下文的用例。
//
// 本层只做四件事：把裸入参解析成值对象、鉴权判定、调用聚合与仓储、在领域决策落库后发布事件。
// 业务不变式一律不在这里——它们属于 entities/；事务也不在这里——它属于 repositories/。
//
// 本上下文的用例几乎全是查询，核心模式是 read-through：
// 先问本地仓储，未命中再走 DataProvider 取数并回写。这样做的理由是
// 外部数据源普遍按次限流（Tushare 按分钟、Finnhub 60 次/分钟），
// 不落地就意味着每个页面刷新都在烧配额。
package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// warmFanOutLimit 是补数扇出的并发上限。
// 市场只有 CN/HK/US 三个，3 就是全量并行；写成常量而不是裸 3 是为了让
// 「这里有一个显式的并发天花板」在阅读时一眼可见。
const warmFanOutLimit = 3

// Operator 是调用方身份在本上下文里的最小视图。
//
// 刻意不直接依赖 identity 上下文的 Claims：跨聚合引用只允许传 ID 或值对象，
// 传实体会把 stock 绑进 identity 的演进节奏。由接口层负责把 Claims 适配成 Operator。
type Operator struct {
	UserID  uint64
	IsAdmin bool
}

// StockService 是本上下文的领域服务。
//
// 依赖的是仓储的具体类型而不是接口：仓储在本项目里只有一个实现，
// 多声明一层接口既不能换实现，又让「改一个方法要动三个文件」。
type StockService struct {
	stockRepo  *repositories.StockRepository
	marketRepo *repositories.MarketDataRepository
	provider   DataProvider
	publisher  domain_event.Publisher
}

func NewStockService(
	stockRepo *repositories.StockRepository,
	marketRepo *repositories.MarketDataRepository,
	provider DataProvider,
	publisher domain_event.Publisher,
) *StockService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &StockService{
		stockRepo:  stockRepo,
		marketRepo: marketRepo,
		provider:   provider,
		publisher:  publisher,
	}
}

// ---------------------------------------------------------------------------
// 入参解析
//
// 接口层传进来的永远是裸字符串，本层的第一件事就是把它们变成值对象。
// 这是「形状校验」，属于本层；而「流通市值不能大于总市值」那种属于 entities/。
// 两者的分界是：前者只看输入本身，后者要看聚合的状态。
// ---------------------------------------------------------------------------

// parseCode 把原始代码与可选市场解析成 StockCode。市场留空时由代码自动推断。
func parseCode(rawCode, rawMarket string) (shared_vo.StockCode, error) {
	return shared_vo.NewStockCode(rawCode, shared_vo.Market(rawMarket))
}

func parseMarket(raw string) (shared_vo.Market, error) {
	m := shared_vo.Market(raw)
	if !m.Valid() {
		return "", custom_errors.Invalid("非法市场: %s", raw)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 查询用例
// ---------------------------------------------------------------------------

// Search 按关键词搜索标的。
//
// read-through 的触发条件是「该市场一条数据都没有」，而不是「本次搜索无结果」：
// 用户搜一个不存在的词本来就该返回空，为此去拉一次全市场列表是纯粹的浪费。
// 冷启动（刚部署、库是空的）才是需要补数的场景。
func (s *StockService) Search(ctx context.Context, keyword, rawMarket string, page shared_vo.Page) ([]*entities.Stock, int64, error) {
	// 市场是可选过滤条件，非法值按「不限市场」处理而不是报错——
	// 搜索框的市场下拉允许「全部」。
	market := shared_vo.Market(rawMarket)

	list, total, err := s.stockRepo.Search(ctx, keyword, market, page)
	if err != nil {
		return nil, 0, err
	}
	if total > 0 || !market.Valid() {
		return list, total, nil
	}

	warmed, err := s.warmMarkets(ctx, []shared_vo.Market{market})
	if err != nil || !warmed {
		// 补数失败不算搜索失败：本地确实没有匹配项，返回空结果比抛错更符合调用方预期。
		return list, total, nil
	}
	return s.stockRepo.Search(ctx, keyword, market, page)
}

// GetBasicInfo 取单只标的的主数据。
func (s *StockService) GetBasicInfo(ctx context.Context, rawCode, rawMarket string) (*entities.Stock, error) {
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return nil, err
	}

	found, err := s.stockRepo.FindByCode(ctx, code)
	if err == nil {
		return found, nil
	}
	if custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
		return nil, err
	}

	// DataProvider 刻意没有「取单只股票基本信息」的方法，只能拉整个市场的列表。
	// 这看起来重，实际上是对的：一次拉取把该市场后续所有标的的查询都变成了本地命中，
	// 而逐只取数会在一篮子标的的场景下退化成 N 次外部调用。
	if _, err := s.warmMarkets(ctx, []shared_vo.Market{code.Market}); err != nil {
		return nil, err
	}
	return s.stockRepo.FindByCode(ctx, code)
}

// GetBasicInfos 批量取主数据，用于自选股列表、批量分析这类一次涉及几十上百只票的场景。
//
// 这条路径上绝不能出现「for 里调 GetBasicInfo」：那是 N 次数据库往返 + 最坏 N 次外部 API 调用。
// 实现方式是一次 FindByCodes 拿回全部命中，再把未命中的按市场归拢交给 warmMarkets，
// 由它并行补数并合并成一次写入。
func (s *StockService) GetBasicInfos(ctx context.Context, rawCodes []string) ([]*entities.Stock, error) {
	codes := make([]shared_vo.StockCode, 0, len(rawCodes))
	for _, raw := range rawCodes {
		code, err := parseCode(raw, "")
		if err != nil {
			// 单个代码非法不应该让整批失败：批量入口的调用方通常是用户自选股列表，
			// 里面混进一个历史遗留的坏代码是常态。
			continue
		}
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return []*entities.Stock{}, nil
	}

	found, err := s.stockRepo.FindByCodes(ctx, codes)
	if err != nil {
		return nil, err
	}

	missingMarkets := missingMarketsOf(codes, found)
	if len(missingMarkets) == 0 {
		return found, nil
	}
	warmed, err := s.warmMarkets(ctx, missingMarkets)
	if err != nil || !warmed {
		return found, nil
	}
	return s.stockRepo.FindByCodes(ctx, codes)
}

// LatestQuote 取最新行情。
func (s *StockService) LatestQuote(ctx context.Context, rawCode, rawMarket string) (*value_objects.Quote, error) {
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return nil, err
	}

	q, err := s.marketRepo.LatestQuote(ctx, code)
	if err == nil {
		return q, nil
	}
	if custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
		return nil, err
	}

	fetched, err := s.provider.FetchQuote(ctx, code)
	if err != nil {
		return nil, err
	}
	// 回写失败不阻断本次查询：数据已经在手上了，落库只是为了下一次少走一趟网络。
	_ = s.marketRepo.SaveQuotes(ctx, []value_objects.Quote{*fetched})
	return fetched, nil
}

// LatestQuotes 批量取最新行情，返回命中的行情与未命中的代码。
//
// 未命中的部分刻意不在这里补数：DataProvider 只有单只标的的 FetchQuote，
// 在 for 里调用它就是 N 次外部 API 往返，一个 300 只票的看板足以打爆任何一家的限流。
// 把 missing 原样交给调用方，由它决定是异步排队补数还是直接显示「暂无行情」——
// 这个取舍需要业务语境，不该由本层替它做。
func (s *StockService) LatestQuotes(ctx context.Context, rawCodes []string) (quotes []value_objects.Quote, missing []shared_vo.StockCode, err error) {
	codes := make([]shared_vo.StockCode, 0, len(rawCodes))
	for _, raw := range rawCodes {
		code, parseErr := parseCode(raw, "")
		if parseErr != nil {
			continue
		}
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return []value_objects.Quote{}, nil, nil
	}

	quotes, err = s.marketRepo.LatestQuotes(ctx, codes)
	if err != nil {
		return nil, nil, err
	}

	hit := make(map[string]struct{}, len(quotes))
	for i := range quotes {
		hit[quotes[i].Symbol()] = struct{}{}
	}
	for _, c := range codes {
		if _, ok := hit[c.Symbol]; !ok {
			missing = append(missing, c)
		}
	}
	return quotes, missing, nil
}

// KlineQuery 是 K 线查询入参。字段多且同为字符串，用结构体避免位置参数写错顺序。
type KlineQuery struct {
	Code   string
	Market string
	Period string
	Start  string
	End    string
	Limit  int
}

// Klines 取 K 线序列。
func (s *StockService) Klines(ctx context.Context, in KlineQuery) ([]value_objects.Kline, error) {
	code, err := parseCode(in.Code, in.Market)
	if err != nil {
		return nil, err
	}
	period, err := value_objects.NewPeriod(in.Period)
	if err != nil {
		return nil, err
	}
	rng, err := shared_vo.NewDateRange(in.Start, in.End)
	if err != nil {
		return nil, err
	}

	local, err := s.marketRepo.Klines(ctx, code, period, rng, in.Limit)
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		return local, nil
	}

	// K 线的「未命中」是空切片而不是 NotFound：区间内没有数据和标的不存在，
	// 在仓储层是同一回事，只有补数一条路。
	fetched, err := s.provider.FetchKlines(ctx, code, period, rng)
	if err != nil {
		return nil, err
	}
	_ = s.marketRepo.SaveKlines(ctx, fetched)
	return fetched, nil
}

// Financials 取财务数据序列，limit 为期数。
func (s *StockService) Financials(ctx context.Context, rawCode, rawMarket string, limit int) ([]value_objects.Financial, error) {
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return nil, err
	}

	local, err := s.marketRepo.Financials(ctx, code, limit)
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		return local, nil
	}

	fetched, err := s.provider.FetchFinancials(ctx, code, limit)
	if err != nil {
		return nil, err
	}
	_ = s.marketRepo.SaveFinancials(ctx, fetched)
	return fetched, nil
}

// News 取区间内的资讯。
func (s *StockService) News(ctx context.Context, rawCode, rawMarket, start, end string, limit int) ([]value_objects.News, error) {
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return nil, err
	}
	rng, err := shared_vo.NewDateRange(start, end)
	if err != nil {
		return nil, err
	}

	local, err := s.marketRepo.News(ctx, code, rng, limit)
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		return local, nil
	}

	fetched, err := s.provider.FetchNews(ctx, code, rng, limit)
	if err != nil {
		return nil, err
	}
	_ = s.marketRepo.SaveNews(ctx, fetched)
	return fetched, nil
}

// Industries 返回行业字典，供前端筛选框使用。
func (s *StockService) Industries(ctx context.Context, rawMarket string) ([]string, error) {
	return s.stockRepo.Industries(ctx, shared_vo.Market(rawMarket))
}

// ---------------------------------------------------------------------------
// 写用例
// ---------------------------------------------------------------------------

// SyncMarket 全量同步某市场的股票主数据。仅管理员可调用，鉴权由本层负责。
//
// 整条链路只有两次 IO：一次 FetchStockList，一次 BulkUpsert（内部分批）。
// 绝不逐只 Upsert——5000 只票就是 5000 次往返。
func (s *StockService) SyncMarket(ctx context.Context, operator *Operator, rawMarket string) (int, error) {
	if err := RequireAdmin(operator); err != nil {
		return 0, err
	}
	market, err := parseMarket(rawMarket)
	if err != nil {
		return 0, err
	}
	if !s.provider.Supports(market) {
		return 0, custom_errors.Unavailable("没有数据源支持市场 %s", market)
	}

	list, err := s.provider.FetchStockList(ctx, market)
	if err != nil {
		return 0, err
	}
	if len(list) == 0 {
		return 0, nil
	}
	if err := s.stockRepo.BulkUpsert(ctx, list); err != nil {
		return 0, err
	}
	return len(list), nil
}

// Delist 标记退市。仅管理员可调用。
//
// 退市判定本身在聚合里（Delist 方法），本层只负责鉴权、落库、以及在落库成功之后发布事件。
// 「该市场是否还剩在市标的」必须在事务内统计，而本层不允许持有事务，
// 因此交给仓储的原子方法一次完成，本层只根据结果决定要不要告警。
func (s *StockService) Delist(ctx context.Context, operator *Operator, rawCode, rawMarket string) error {
	if err := RequireAdmin(operator); err != nil {
		return err
	}
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return err
	}

	stock, err := s.stockRepo.FindByCode(ctx, code)
	if err != nil {
		return err
	}
	// 领域决策在聚合内完成：重复退市会被 Delist 拒绝，本层不重复判断。
	// 真正的并发保证在仓储的 UPDATE 谓词里，这里的判断只是快速失败。
	if err := stock.Delist(); err != nil {
		return err
	}
	remaining, err := s.stockRepo.DelistAndCount(ctx, stock)
	if err != nil {
		return err
	}
	if remaining == 0 {
		// 整个市场一只在市标的都不剩，几乎必然是同步链路出了问题而不是真实情况。
		// 这里不回滚（退市本身是管理员的明确意图），只把异常状态作为冲突抛出去。
		return custom_errors.Conflict("市场 %s 已无在市标的，请先核对同步数据", code.Market)
	}

	// 事件在领域决策与落库都完成之后才发布，保证消费方不会读到尚未落库的聚合。
	s.publish(ctx, stock)
	return nil
}

// Relist 撤销误报的退市。数据源在停牌/重组期间会把 list_status 短暂标成 D，
// 必须有一条人工撤销路径。
//
// 落库走仓储的 Relist 而不是通用 Upsert：Upsert 会无条件覆盖 delisted 列，
// 两个并发的撤销请求都会「成功」；Relist 把 delisted = true 写进 WHERE，
// 只有真正完成状态跃迁的那一次才算数。
func (s *StockService) Relist(ctx context.Context, operator *Operator, rawCode, rawMarket string) error {
	if err := RequireAdmin(operator); err != nil {
		return err
	}
	code, err := parseCode(rawCode, rawMarket)
	if err != nil {
		return err
	}
	stock, err := s.stockRepo.FindByCode(ctx, code)
	if err != nil {
		return err
	}
	if err := stock.Relist(); err != nil {
		return err
	}
	if err := s.stockRepo.Relist(ctx, stock); err != nil {
		return err
	}
	s.publish(ctx, stock)
	return nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// warmMarkets 把若干市场的主数据灌进本地库，是本上下文唯一的补数入口。
//
// 为什么不是「for 里逐个市场取数再逐个写库」：那是循环里发 RPC。
// 这里用 concurrency.Settle 做有界扇出——选 Settle 而不是 Map，
// 是因为补数是尽力而为的旁路：港股源挂了不该让 A 股的补数也一起失败。
// 拿到全部结果后合并成一次 BulkUpsert，写库同样只发生一次。
//
// 返回 warmed=false 表示这次没有真正补到数据（数据源不支持这些市场或全部返回空），
// 调用方据此决定是否值得重试查询。
func (s *StockService) warmMarkets(ctx context.Context, markets []shared_vo.Market) (bool, error) {
	targets := make([]shared_vo.Market, 0, len(markets))
	for _, m := range markets {
		if m.Valid() && s.provider.Supports(m) {
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		return false, nil
	}

	outcomes, err := concurrency.Settle(ctx, targets, warmFanOutLimit,
		func(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
			return s.provider.FetchStockList(ctx, market)
		})
	if err != nil {
		// Settle 只在父 ctx 被取消时返回错误，单个市场的失败在 outcomes 里。
		return false, err
	}

	merged := make([]*entities.Stock, 0, len(targets)*16)
	for _, o := range outcomes {
		if o.Err != nil {
			continue
		}
		merged = append(merged, o.Value...)
	}
	if len(merged) == 0 {
		return false, nil
	}
	if err := s.stockRepo.BulkUpsert(ctx, merged); err != nil {
		return false, err
	}
	return true, nil
}

// missingMarketsOf 算出「有代码没查到」的市场集合，去重后返回。
// 用市场而不是代码作为补数粒度，是把外部调用次数从 O(标的数) 压到 O(市场数)。
func missingMarketsOf(codes []shared_vo.StockCode, found []*entities.Stock) []shared_vo.Market {
	hit := make(map[string]struct{}, len(found))
	for _, s := range found {
		hit[string(s.Market())+":"+s.Symbol()] = struct{}{}
	}
	seen := make(map[shared_vo.Market]struct{}, warmFanOutLimit)
	var out []shared_vo.Market
	for _, c := range codes {
		if _, ok := hit[string(c.Market)+":"+c.Symbol]; ok {
			continue
		}
		if _, ok := seen[c.Market]; ok {
			continue
		}
		seen[c.Market] = struct{}{}
		out = append(out, c.Market)
	}
	return out
}

// publish 取出聚合累积的事件并发布。取出即清空，保证同一事件不会被重复发布。
func (s *StockService) publish(ctx context.Context, st *entities.Stock) {
	if evts := st.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// RequireAdmin 是授权检查，不是业务不变式，因此归本层而不是 entities/。
func RequireAdmin(op *Operator) error {
	if op == nil {
		return custom_errors.Unauthorized("未登录")
	}
	if !op.IsAdmin {
		return custom_errors.Forbidden("需要管理员权限")
	}
	return nil
}
