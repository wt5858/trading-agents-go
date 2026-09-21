package domain_services

import (
	"context"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// WatchlistService 编排自选分组与自选股的全部用例。
//
// 依赖仓储的具体类型而不是接口：仓储在本项目里只有一个实现，
// 多声明一层接口既不能换实现，又让「改一个方法要动三个文件」。
// 而 QuoteReader 是接口，因为它的实现确实在另一个上下文里。
type WatchlistService struct {
	groups    *repositories.WatchlistGroupRepository
	quotes    QuoteReader
	publisher domain_event.Publisher
}

func NewWatchlistService(
	groups *repositories.WatchlistGroupRepository,
	quotes QuoteReader,
	publisher domain_event.Publisher,
) *WatchlistService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &WatchlistService{groups: groups, quotes: quotes, publisher: publisher}
}

// ---------------------------------------------------------------------------
// 分组 CRUD
// ---------------------------------------------------------------------------

// CreateGroup 新建自选分组。
//
// 没有「先查有没有重名」这一步：那是 TOCTOU，两个并发请求会双双查到「不重名」
// 然后双双插入。重名由 (user_id, name) 唯一索引挡住，仓储把 1062 翻译成
// AlreadyExists。少一次查询，还多一层真正的保证。
func (s *WatchlistService) CreateGroup(ctx context.Context, op Operator, rawName string) (*entities.WatchlistGroup, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	name, err := value_objects.NewGroupName(rawName)
	if err != nil {
		return nil, err
	}
	g, err := entities.NewWatchlistGroup(op.UserID, name)
	if err != nil {
		return nil, err
	}
	// 交出去的是整个聚合根，哪怕它此刻一个子实体都没有。
	if err := s.groups.Save(ctx, g); err != nil {
		return nil, err
	}
	// 事件在领域决策与落库都完成之后才发布，保证消费方不会读到尚未落库的聚合。
	s.publish(ctx, g)
	return g, nil
}

// RenameGroup 重命名分组。
func (s *WatchlistService) RenameGroup(ctx context.Context, op Operator, groupID uint64, rawName string) (*entities.WatchlistGroup, error) {
	name, err := value_objects.NewGroupName(rawName)
	if err != nil {
		return nil, err
	}
	g, err := s.loadOwned(ctx, op, groupID)
	if err != nil {
		return nil, err
	}
	// 领域决策在聚合内完成，本层不重复判定。
	if err := g.Rename(name); err != nil {
		return nil, err
	}
	if err := s.groups.Save(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// DeleteGroup 删除分组及其全部自选股。
//
// 先加载再删，是为了让「不是你的分组」和「分组不存在」走同一条 NotFound 路径；
// 真正的并发保证在仓储的 DELETE 谓词里（user_id 进了 WHERE），
// 本层的这次加载只是为了给出可读的错误，不是安全边界。
func (s *WatchlistService) DeleteGroup(ctx context.Context, op Operator, groupID uint64) error {
	g, err := s.loadOwned(ctx, op, groupID)
	if err != nil {
		return err
	}
	// 子实体的删除不在这里：调用方交出的是根的标识，自选项怎么跟着消失
	// 是仓储在同一个事务里的实现细节。本层连 watchlist_items 这个名字都不该知道。
	return s.groups.Delete(ctx, g.ID, op.UserID)
}

// ListGroups 列出当前用户的全部分组（含各自的自选股）。
//
// 不带行情：分组列表页展示的是「名字 + 数量」，为它去拉一遍所有分组、
// 所有标的的行情是纯粹的浪费。需要行情的是单个分组的明细页，见 ListItems。
func (s *WatchlistService) ListGroups(ctx context.Context, op Operator) ([]*entities.WatchlistGroup, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	// 管理员也只看自己的自选股：自选股是个人数据，不是可运维的对象。
	// 这是本上下文与 analysis 的区别——那边管理员需要排查别人的任务。
	return s.groups.ListByUser(ctx, op.UserID)
}

// ---------------------------------------------------------------------------
// 自选股增删改
// ---------------------------------------------------------------------------

// AddStockInput 是加自选的入参形状。
type AddStockInput struct {
	GroupID uint64
	Code    string
	Market  string
	Note    string
}

// AddStock 把一只股票加入分组。
//
// 参考价在这里取一次并随子实体落库：它是「加入自选那一刻的价格」这个事实，
// 事后无从补算。取不到也不影响加自选成功——见 fetchReferencePrice。
func (s *WatchlistService) AddStock(ctx context.Context, op Operator, in AddStockInput) (*entities.WatchlistItem, error) {
	code, err := parseCode(in.Code, in.Market)
	if err != nil {
		return nil, err
	}
	note, err := value_objects.NewItemNote(in.Note)
	if err != nil {
		return nil, err
	}
	g, err := s.loadOwned(ctx, op, in.GroupID)
	if err != nil {
		return nil, err
	}

	refPrice := s.fetchReferencePrice(ctx, code)

	// 重复、上限、排序全部由聚合根判定，本层一条都不重复判。
	// 子实体也正是在这里、且只可能在这里诞生。
	item, err := g.AddItem(code, note, refPrice)
	if err != nil {
		return nil, err
	}
	if err := s.groups.Save(ctx, g); err != nil {
		return nil, err
	}
	s.publish(ctx, g)
	return item, nil
}

// StockRef 定位「某个分组里的某只股票」。
type StockRef struct {
	GroupID uint64
	Code    string
	Market  string
}

// RemoveStock 把一只股票移出分组。
func (s *WatchlistService) RemoveStock(ctx context.Context, op Operator, ref StockRef) error {
	code, err := parseCode(ref.Code, ref.Market)
	if err != nil {
		return err
	}
	g, err := s.loadOwned(ctx, op, ref.GroupID)
	if err != nil {
		return err
	}
	// 移除之后的序号重排由聚合根完成，本层不碰 SortOrder。
	if err := g.RemoveItem(code); err != nil {
		return err
	}
	if err := s.groups.Save(ctx, g); err != nil {
		return err
	}
	s.publish(ctx, g)
	return nil
}

// UpdateNote 修改某只自选股的备注。
func (s *WatchlistService) UpdateNote(ctx context.Context, op Operator, ref StockRef, rawNote string) error {
	code, err := parseCode(ref.Code, ref.Market)
	if err != nil {
		return err
	}
	note, err := value_objects.NewItemNote(rawNote)
	if err != nil {
		return err
	}
	g, err := s.loadOwned(ctx, op, ref.GroupID)
	if err != nil {
		return err
	}
	// 注意这里没有 item.Note = note：子实体的修改方法不导出，本层写不出那一行。
	if err := g.UpdateItemNote(code, note); err != nil {
		return err
	}
	return s.groups.Save(ctx, g)
}

// ReorderGroup 重排分组内自选股的顺序。
func (s *WatchlistService) ReorderGroup(ctx context.Context, op Operator, groupID uint64, rawCodes []string) error {
	if len(rawCodes) == 0 {
		return custom_errors.Invalid("排序列表不能为空")
	}
	codes := make([]shared_vo.StockCode, 0, len(rawCodes))
	for _, raw := range rawCodes {
		// 这里不容错：排序列表里混进一个坏代码，说明前端传的顺序本身就是错的，
		// 静默跳过会让用户拖出来的顺序和看到的顺序对不上，却又没有任何提示。
		code, err := parseCode(raw, "")
		if err != nil {
			return err
		}
		codes = append(codes, code)
	}
	g, err := s.loadOwned(ctx, op, groupID)
	if err != nil {
		return err
	}
	if err := g.Reorder(codes); err != nil {
		return err
	}
	return s.groups.Save(ctx, g)
}

// MoveStockInput 是跨分组移动的入参。
type MoveStockInput struct {
	FromGroupID uint64
	ToGroupID   uint64
	Code        string
	Market      string
}

// MoveStock 把一只股票从一个分组移到另一个分组。
//
// ===========================================================================
// 一致性故事：两个聚合根，两次 Save，**没有**共享事务
// ===========================================================================
//
// # 为什么不用一个事务包住两次 Save
//
// 因为源分组和目标分组是**两个独立的聚合实例**。聚合是一致性边界的定义：
// 边界之内强一致（根和它的子实体在一个事务里），边界之间最终一致。
// 为了让这次移动「原子」而把两个聚合塞进一个事务，等于宣布这两个分组
// 其实是同一个一致性单元——那么下一次「一个用户的全部分组」也会被这么论证，
// 最后整张表变成一个巨型聚合，所有写操作互相阻塞。
//
// 技术上也做不到：事务只存在于 repositories/，本层没有、也不该有事务句柄。
// 要做成一个事务，就得在仓储上开一个 MoveItem(ctx, fromID, toID, code) 方法，
// 那是一个同时写两个聚合的方法——本项目明确不允许。
//
// # 那么失败了会怎样
//
// 顺序是刻意的：**先加到目标分组，成功之后再从源分组移除**。
//
//	第一步失败（加不进去：目标组满了 / 已经有这只票 / 库挂了）
//	    → 什么都没变，直接把错误返回给用户，可以原样重试。
//	第二步失败（加进去了，但没删掉）
//	    → 中间态是「两个分组里都有这只票」。
//
// 这个中间态是刻意选的，因为它的反面糟糕得多：如果先删后加，第二步失败时
// 用户的自选股就**凭空消失**了，而且没有任何痕迹告诉他曾经有过。
// 相比之下「多出现一次」是用户一眼能看见、一次点击能修正的问题。
// 这就是在两种不一致里选那个可观测、可自愈的——分布式系统里几乎总是这个取舍。
//
// 需要更强保证时（比如将来要做批量移动），正确的做法是引入领域事件 +
// 幂等的补偿消费者，而不是把两个聚合绑进一个事务。
//
// # 参考价跟着搬家
//
// 移动不是「取消关注再重新关注」，用户的持有视角没有中断，所以参考价
// 必须原样带过去。不带的话，用户把票拖进另一个分组，「自选以来 +30%」
// 会瞬间归零——那看起来就像系统丢了数据。备注同理。
func (s *WatchlistService) MoveStock(ctx context.Context, op Operator, in MoveStockInput) error {
	if in.FromGroupID == in.ToGroupID {
		return custom_errors.Invalid("源分组与目标分组相同")
	}
	code, err := parseCode(in.Code, in.Market)
	if err != nil {
		return err
	}

	src, err := s.loadOwned(ctx, op, in.FromGroupID)
	if err != nil {
		return err
	}
	dst, err := s.loadOwned(ctx, op, in.ToGroupID)
	if err != nil {
		return err
	}

	// 先把要搬走的东西读出来。读子实体是允许的，改它才不允许——
	// 而本层也确实改不动它（修改方法不导出）。
	item, ok := src.ItemByCode(code)
	if !ok {
		return custom_errors.NotFound("分组「%s」中不存在 %s", src.Name.String(), code.FullSymbol())
	}
	note, refPrice := item.Note, item.RefPrice

	// 第一步：加到目标分组。重复与上限由目标分组的聚合根判定。
	if _, err := dst.AddItem(code, note, refPrice); err != nil {
		return err
	}
	if err := s.groups.Save(ctx, dst); err != nil {
		return err
	}
	s.publish(ctx, dst)

	// 第二步：从源分组移除。失败时中间态是「两个分组都有」，理由见上面的长注释。
	if err := src.RemoveItem(code); err != nil {
		return err
	}
	if err := s.groups.Save(ctx, src); err != nil {
		return err
	}
	s.publish(ctx, src)
	return nil
}

// ---------------------------------------------------------------------------
// 带行情的读路径
// ---------------------------------------------------------------------------

// WatchlistRow 是自选股列表的一行：子实体 + 它当前的行情。
//
// Quote 为 nil 表示这只票暂时没有行情（新股、停牌、数据源未覆盖）。
// 用 nil 而不是一条价格为 0 的占位：0.00 元会被前端画成一根跌停线，
// 而「暂无行情」是一句用户看得懂的话。
type WatchlistRow struct {
	Item  *entities.WatchlistItem
	Quote *value_objects.QuoteSnapshot
}

// GroupDetail 是一个分组的完整读模型。
type GroupDetail struct {
	Group *entities.WatchlistGroup
	Rows  []WatchlistRow
}

// ListItems 列出一个分组的自选股，并补上当前行情。
//
// # 整条路径上只有两次 IO
//
//	一次 FindByID（其内部是两条查询：分组 + 它的全部自选项，不是 N+1）
//	一次 LatestQuotes（一把取回全部标的的行情，不是每只票一次）
//
// 绝不会出现「for 里查行情」——QuoteReader 上压根没有单只方法可调。
// 这也是为什么这里不需要 concurrency 包做扇出：没有可并行的 N 次调用，
// 只有一次批量调用，那本来就是最优解。
func (s *WatchlistService) ListItems(ctx context.Context, op Operator, groupID uint64) (*GroupDetail, error) {
	g, err := s.loadOwned(ctx, op, groupID)
	if err != nil {
		return nil, err
	}

	quotes := s.latestQuotes(ctx, g.Codes())
	rows := make([]WatchlistRow, 0, g.ItemCount())
	for _, it := range g.Items {
		row := WatchlistRow{Item: it}
		// map 查出来的已经是一份拷贝（map 元素不可取址），对它取址是安全的，
		// 每一行拿到的都是独立的快照。
		if q, ok := quotes[quoteKey(it.Code)]; ok {
			row.Quote = &q
		}
		rows = append(rows, row)
	}
	return &GroupDetail{Group: g, Rows: rows}, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// loadOwned 加载分组并校验归属。
//
// 归属不符时返回 NotFound 而不是 Forbidden：Forbidden 等于确认「这个 id 存在」，
// 会让分组 ID 空间变成可探测的信道——攻击者可以靠遍历 id 数出别人有几个分组。
// 权限判定属于本层（谁能调用），不属于 entities/（业务规则）。
func (s *WatchlistService) loadOwned(ctx context.Context, op Operator, groupID uint64) (*entities.WatchlistGroup, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	if groupID == 0 {
		return nil, custom_errors.Invalid("分组 ID 不能为空")
	}
	g, err := s.groups.FindByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !g.OwnedBy(op.UserID) {
		return nil, custom_errors.NotFound("自选分组(id=%d) 不存在", groupID)
	}
	return g, nil
}

// latestQuotes 批量取行情并按代码索引。行情取不到就返回空表。
//
// 拿不到行情不算错误：自选股列表的核心是那份清单，行情是锦上添花。
// 因为行情源抖动就让用户打不开自选股页面，是把辅助信息的可用性
// 传染给了主功能。缺失的行情在界面上显示成「暂无行情」。
func (s *WatchlistService) latestQuotes(ctx context.Context, codes []shared_vo.StockCode) map[string]value_objects.QuoteSnapshot {
	if s.quotes == nil || len(codes) == 0 {
		return nil
	}
	list, err := s.quotes.LatestQuotes(ctx, codes)
	if err != nil {
		return nil
	}
	out := make(map[string]value_objects.QuoteSnapshot, len(list))
	for _, q := range list {
		out[quoteKey(q.Code)] = q
	}
	return out
}

// fetchReferencePrice 取「此刻」的价格作为参考价。
//
// 用的仍然是批量端口，只是这一次切片里只有一个元素——端口上没有单只方法，
// 也不该为了这个场景加一个：加了它，自选股列表迟早会有人在 for 里调它。
//
// 任何失败都退化成「没有参考价」：行情源不可用、标的停牌、数据源没覆盖，
// 这些都不该让「把一只票加进自选」这件事失败。参考价缺失的后果只是
// 「自选以来涨跌幅」这一列显示为空，而那本来就是个可有可无的增强信息。
func (s *WatchlistService) fetchReferencePrice(ctx context.Context, code shared_vo.StockCode) value_objects.ReferencePrice {
	if s.quotes == nil {
		return value_objects.ReferencePrice{}
	}
	list, err := s.quotes.LatestQuotes(ctx, []shared_vo.StockCode{code})
	if err != nil || len(list) == 0 {
		return value_objects.ReferencePrice{}
	}
	q := list[0]
	return value_objects.NewReferencePrice(q.Price, q.TradeDate, time.Now())
}

// publish 取出聚合累积的事件并发布。取出即清空，保证同一事件不会被重复发布。
func (s *WatchlistService) publish(ctx context.Context, g *entities.WatchlistGroup) {
	if evts := g.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// parseCode 把接口层传来的裸字符串解析成值对象。市场留空时由代码自动推断。
//
// 这是「形状校验」，属于本层；而「组内不能重复」那种要看聚合状态的判定属于 entities/。
func parseCode(rawCode, rawMarket string) (shared_vo.StockCode, error) {
	return shared_vo.NewStockCode(rawCode, shared_vo.Market(rawMarket))
}

// quoteKey 是行情与自选项对齐用的键。
// 用 (market, symbol) 的理由与聚合内的判重键相同：StockCode 还带着用户输入的
// Raw 字段，直接比结构体会让 "600519" 和 "600519.SH" 对不上。
func quoteKey(code shared_vo.StockCode) string {
	return code.Market.String() + ":" + code.Symbol
}
