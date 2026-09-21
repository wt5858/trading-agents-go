package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// WatchlistItem 是自选项，**子实体**，不是聚合根。
//
// ===========================================================================
// 本文件是全项目「聚合根 ↔ 子实体」这条规则的参考实现，值得完整解释
// ===========================================================================
//
// # 它为什么是实体而不是值对象
//
// 它有身份：同一只票的备注被改了之后，它还是**那一条**自选记录，
// 创建时间、参考价、排序位置都跟着它走。这正是实体的定义。
// 对比 stock 上下文的 Quote——重抓一次行情，新的那份就是全部事实，
// 不存在「同一个 Quote 变了」，所以那是值对象。
//
// # 它为什么没有自己的仓储
//
// 因为它不是聚合根。自选项的所有不变式（组内不重复、数量上限、排序稠密）
// 都是**跨若干条自选项**才成立的规则，只有能看到全部兄弟节点的那个对象
// 才有资格判定它们——那个对象就是 WatchlistGroup。
// 给子实体配一个仓储，等于给了调用方一条绕过根、直接写单条记录的路，
// 上面三条不变式会在那条路上全部失效。
//
// 于是本上下文只有一个仓储：WatchlistGroupRepository。
//
// # 为什么子实体可以和根一起加载，而 analysis 的 Task 不可以
//
// 边界大小。一个分组几十到几百只票，有 MaxItemsPerGroup 钉死上限，
// 整份加载进内存是常数级开销。而 analysis.Batch 若持有 Task 实体，
// 每推进一个子任务都要加载整批，且两者各自独立并发更新——那是两个聚合根。
// 判据不是「有没有父子关系」，而是「它们是不是必须在同一个事务里一起变」。
//
// # 为什么没有嵌 EventRecorder
//
// 事件是聚合对外的声明，只有根有资格发。见 domain_events 包注释。
//
// # 字段是导出的，可修改性却不是靠编译器守的
//
// 字段导出是本项目实体层的统一风格（DTO 与接口层都要读它们）。
// 因此「不得从外部修改子实体」这条规则，走的是 identity.User.PasswordHash
// 同一条路子：约定 + 让唯一的写入路径显而易见（本类型的修改方法全部不导出，
// 只有同包的根能调），再加下面 attached 这道运行期兜底。
type WatchlistItem struct {
	// ID 为 0 表示「尚未落库」。仓储的子实体 diff 正是靠它区分 INSERT 与 UPDATE。
	ID      uint64
	GroupID uint64

	// Code 是跨聚合引用：自选项只持有股票代码值对象，绝不持有 stock.Stock 实体。
	// 持有实体会让「股票改了名」变成一次跨上下文的级联写入，
	// 也会让自选股列表的加载顺带把股票主数据整个拖进来。
	Code shared_vo.StockCode

	Note     value_objects.ItemNote
	RefPrice value_objects.ReferencePrice

	// SortOrder 由根统一维护，恒为 0..n-1 的稠密连续序列。
	// 它不是「用户填的数字」，任何外部写入都会破坏这个不变式。
	SortOrder int

	CreatedAt time.Time
	UpdatedAt time.Time

	// attached 是「这个子实体确实由根创建」的凭据，刻意不导出。
	//
	// 包外代码可以写出 &entities.WatchlistItem{...} 并 append 进 g.Items，
	// 编译器拦不住；但它无论如何也设不了这个字段（不导出的字段在包外
	// 既不能用字面量赋值，也不能赋值访问）。于是 Validate() 能在仓储写库之前
	// 把这种「绕过根偷偷塞进来的子实体」揪出来，让约定有一道实际的防线。
	attached bool
}

// newWatchlistItem 是子实体唯一的构造函数，且**不导出**。
//
// 包外没有任何办法调用它 —— 这就是「子实体只能经由根产生」在编译期的表达。
// 包内它只有两个调用点，都在 watchlist_group.go 里：
//   - (*WatchlistGroup).AddItem      领域路径，执行全部不变式
//   - (*WatchlistGroup).RehydrateItem 持久化重建路径，跳过校验（既成事实）
func newWatchlistItem(
	groupID uint64,
	code shared_vo.StockCode,
	note value_objects.ItemNote,
	refPrice value_objects.ReferencePrice,
	sortOrder int,
	now time.Time,
) *WatchlistItem {
	return &WatchlistItem{
		GroupID:   groupID,
		Code:      code,
		Note:      note,
		RefPrice:  refPrice,
		SortOrder: sortOrder,
		CreatedAt: now,
		UpdatedAt: now,
		attached:  true,
	}
}

// Symbol 返回带交易所后缀的完整代码，供展示与对接外部数据源。
func (it *WatchlistItem) Symbol() string { return it.Code.FullSymbol() }

// Market 是访问器而非独立字段：市场已经包含在 StockCode 里，
// 单独存一份迟早会和 Code.Market 不一致。
func (it *WatchlistItem) Market() shared_vo.Market { return it.Code.Market }

// GainPctSinceWatched 返回「自加入自选以来的涨跌幅」。
// 口径与它唯一的计算位置都在 ReferencePrice.GainPctSince，这里只是转调。
func (it *WatchlistItem) GainPctSinceWatched(currentPrice decimal.Decimal) (decimal.Decimal, bool) {
	return it.RefPrice.GainPctSince(currentPrice)
}

// ---------------------------------------------------------------------------
// 以下修改方法全部不导出：只有同包的聚合根能调用它们。
// domain_services 与 application 拿到 *WatchlistItem 也只能读，不能改。
// ---------------------------------------------------------------------------

func (it *WatchlistItem) setSortOrder(n int, now time.Time) {
	if it.SortOrder == n {
		// 没变就不动 UpdatedAt：仓储据此判断这一行要不要发 UPDATE，
		// 一次只挪动两只票的排序不该把整组 200 行全部重写一遍。
		return
	}
	it.SortOrder = n
	it.UpdatedAt = now
}

func (it *WatchlistItem) setNote(note value_objects.ItemNote, now time.Time) {
	if it.Note.Equal(note) {
		return
	}
	it.Note = note
	it.UpdatedAt = now
}

func (it *WatchlistItem) bindGroup(groupID uint64) { it.GroupID = groupID }

func (it *WatchlistItem) isAttached() bool { return it.attached }
