// Package entities 承载自选股上下文的聚合。
//
// 本包是全项目「聚合根 ↔ 子实体」的参考实现：
//
//	WatchlistGroup（聚合根，有仓储）
//	  └── WatchlistItem（子实体，无仓储，只能经由根产生与修改）
//
// 全部业务不变式都住在这里，domain_services 不重复判定，仓储也不重复判定
// （仓储只把其中一条——组名唯一——落成数据库的唯一索引，因为那一条在并发下
// 内存判断给不出保证，见 repositories 层注释）。
package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MaxItemsPerGroup 是单个分组的自选股数量上限。
//
// 它是业务不变式而非配置项，理由有两层：
//   - 业务上：自选股是「盯盘清单」，几百只之后它已经不是清单而是一个市场切片，
//     用户真正需要的是选股器（screening 上下文），不是更长的自选股。
//   - 技术上：正是这个上限让「加载根的时候把全部子实体一起加载」成为一个
//     常数级操作，从而让 WatchlistItem 可以安全地作为子实体存在。
//     去掉它，这个聚合的边界就塌了。
//
// 定在 entities 而不是 config：任何入口（HTTP、批量导入、将来的 OpenAPI）
// 都绕不过聚合，也就都绕不过这个数。
const MaxItemsPerGroup = 200

// WatchlistGroup 是自选股上下文的**聚合根**。
//
// # 子实体只能经由根触达
//
// 对 WatchlistItem 的一切访问与修改都必须走本类型的方法：
// AddItem / RemoveItem / ItemByCode / Reorder / UpdateItemNote。
// 包外既造不出一个 WatchlistItem（构造函数不导出），也改不动一个
// （修改方法不导出）。这不是风格洁癖，是下面三条不变式能够成立的前提——
// 它们全都需要「看得见全部兄弟节点」才能判定：
//
//   - 同一分组内不得重复持有同一只股票；
//   - 单组数量不超过 MaxItemsPerGroup；
//   - SortOrder 恒为 0..n-1 的稠密连续序列（增、删、重排之后都成立）。
//
// # 组名唯一由数据库守
//
// 「同一用户下分组名唯一」是跨聚合实例的规则，内存里的这一个聚合看不见
// 用户的其它分组，因此它在这里没有对应的判断。它由 (user_id, name) 唯一索引
// 在写入语句里保证——先查再插是 TOCTOU，挡不住并发的两次创建。
type WatchlistGroup struct {
	domain_event.EventRecorder

	// ID 为 0 表示尚未落库，仓储据此在 Save 里选择 INSERT 还是 UPDATE。
	ID     uint64
	UserID uint64
	Name   value_objects.GroupName

	// Items 是子实体集合，随根一起加载、随根一起保存。
	//
	// 切片本身导出是为了让读路径（DTO 投影、接口层渲染）不必绕一层拷贝；
	// 但其中的元素只能读。想增删元素请用 AddItem / RemoveItem——
	// 直接 append 进来的元素会因为缺少 attached 凭据而被 Validate 拒绝，
	// 仓储在写库前会调用它。
	Items []*WatchlistItem

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewWatchlistGroup 是分组进入系统的唯一入口。
func NewWatchlistGroup(userID uint64, name value_objects.GroupName) (*WatchlistGroup, error) {
	if userID == 0 {
		return nil, custom_errors.Invalid("自选分组必须归属于一个用户")
	}
	if name.IsZero() {
		return nil, custom_errors.Invalid("分组名不能为空")
	}
	now := time.Now()
	g := &WatchlistGroup{
		UserID: userID,
		Name:   name,
		// 非 nil 空切片：让「新建的空分组」和「还没加载子实体的分组」在
		// 序列化时都是 []，前端不必区分 null 与 []。
		Items:     make([]*WatchlistItem, 0, 8),
		CreatedAt: now,
		UpdatedAt: now,
	}
	g.AddDomainEvent(domain_events.NewOnGroupCreated(userID, name.String()))
	return g, nil
}

// RehydrateWatchlistGroup 从持久化数据重建聚合根，不做任何校验。
//
// 理由与 identity 的 RehydrateUsername 一致：落库的行是既成事实。
// 举个具体的例子——若某天把 MaxItemsPerGroup 从 200 收紧到 100，
// 重建时再校验一次，会让所有存量的大分组**整个查不出来**，
// 用户连删掉几只票以符合新规则的机会都没有。校验属于写入路径。
func RehydrateWatchlistGroup(
	id, userID uint64,
	name value_objects.GroupName,
	createdAt, updatedAt time.Time,
	itemCapacity int,
) *WatchlistGroup {
	if itemCapacity < 0 {
		itemCapacity = 0
	}
	return &WatchlistGroup{
		ID:        id,
		UserID:    userID,
		Name:      name,
		Items:     make([]*WatchlistItem, 0, itemCapacity),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

// RehydrateItem 是持久化重建路径上挂载子实体的唯一入口。
//
// 它同样开在根上——「子实体只能经由根产生」这条规则对读路径一视同仁。
// 与 AddItem 的区别只有一条：它不跑任何不变式判定，因为它重建的是既成事实，
// 顺序与数量由数据库里的行决定。命名上带 Rehydrate 前缀是为了让它在
// 代码审查里一眼可辨——业务代码里出现它就是错的。
func (g *WatchlistGroup) RehydrateItem(
	id uint64,
	code shared_vo.StockCode,
	note value_objects.ItemNote,
	refPrice value_objects.ReferencePrice,
	sortOrder int,
	createdAt, updatedAt time.Time,
) *WatchlistItem {
	it := newWatchlistItem(g.ID, code, note, refPrice, sortOrder, createdAt)
	it.ID = id
	it.UpdatedAt = updatedAt
	g.Items = append(g.Items, it)
	return it
}

// ---------------------------------------------------------------------------
// 根自身的行为
// ---------------------------------------------------------------------------

// Rename 改分组名。
//
// 改成原名直接返回成功而不是报错：重复提交同一份表单不该是一个错误。
// 与别的分组重名这条规则在这里判不了（看不见别的分组），由唯一索引兜底。
func (g *WatchlistGroup) Rename(name value_objects.GroupName) error {
	if name.IsZero() {
		return custom_errors.Invalid("分组名不能为空")
	}
	if g.Name.Equal(name) {
		return nil
	}
	g.Name = name
	g.UpdatedAt = time.Now()
	return nil
}

func (g *WatchlistGroup) OwnedBy(userID uint64) bool { return g.UserID == userID }

// ItemCount 返回子实体数量。
func (g *WatchlistGroup) ItemCount() int { return len(g.Items) }

// Codes 返回组内全部股票代码，按当前排序。
//
// 它的存在是为了让读路径能一次性拿到全部代码去批量取行情——
// 没有它，调用方就会在 for 里逐只查行情，那正是本项目明令禁止的形状。
func (g *WatchlistGroup) Codes() []shared_vo.StockCode {
	out := make([]shared_vo.StockCode, 0, len(g.Items))
	for _, it := range g.Items {
		out = append(out, it.Code)
	}
	return out
}

// ---------------------------------------------------------------------------
// 经由根操作子实体
// ---------------------------------------------------------------------------

// AddItem 把一只股票加入分组，是自选项进入系统的**唯一领域入口**。
//
// 三条不变式在这里一次性判完，顺序是刻意的：
//  1. 代码合法（形状）；
//  2. 组内不重复——先判重复再判上限，否则一个已经满员的分组里重复添加
//     已有的票，用户会收到「已达上限」这种驴唇不对马嘴的提示；
//  3. 未超上限。
//
// 新项排在末尾（SortOrder = 当前数量），天然保持序列稠密连续。
//
// 返回新建的子实体是为了让调用方读它（比如回显参考价），不是为了让它改。
// 修改方法都不导出，包外拿到指针也改不动。
func (g *WatchlistGroup) AddItem(
	code shared_vo.StockCode,
	note value_objects.ItemNote,
	refPrice value_objects.ReferencePrice,
) (*WatchlistItem, error) {
	if code.IsZero() {
		return nil, custom_errors.Invalid("股票代码不能为空")
	}
	if existing, ok := g.ItemByCode(code); ok {
		return nil, custom_errors.AlreadyExists(
			"分组「%s」中已存在 %s", g.Name.String(), existing.Symbol())
	}
	if len(g.Items) >= MaxItemsPerGroup {
		return nil, custom_errors.QuotaExceeded(
			"分组「%s」最多容纳 %d 只股票，当前已有 %d 只",
			g.Name.String(), MaxItemsPerGroup, len(g.Items))
	}

	now := time.Now()
	it := newWatchlistItem(g.ID, code, note, refPrice, len(g.Items), now)
	g.Items = append(g.Items, it)
	g.UpdatedAt = now

	g.AddDomainEvent(domain_events.NewOnStockWatched(
		g.UserID, g.ID, code.FullSymbol(), code.Market.String()))
	return it, nil
}

// RemoveItem 把一只股票移出分组。
//
// 删除之后立刻重排序号：SortOrder 的稠密连续是一条不变式，
// 「删完留个洞，下次加的时候再补」这种拖延会让洞在前端表现为顺序跳变，
// 也会让 Reorder 的语义变得依赖历史。
func (g *WatchlistGroup) RemoveItem(code shared_vo.StockCode) error {
	idx := g.indexOf(code)
	if idx < 0 {
		return custom_errors.NotFound(
			"分组「%s」中不存在 %s", g.Name.String(), code.FullSymbol())
	}

	now := time.Now()
	removed := g.Items[idx]
	g.Items = append(g.Items[:idx], g.Items[idx+1:]...)
	g.resequence(now)
	g.UpdatedAt = now

	g.AddDomainEvent(domain_events.NewOnStockUnwatched(
		g.UserID, g.ID, removed.Code.FullSymbol(), removed.Code.Market.String()))
	return nil
}

// ItemByCode 按代码查子实体。这是包外读取单个子实体的唯一入口。
func (g *WatchlistGroup) ItemByCode(code shared_vo.StockCode) (*WatchlistItem, bool) {
	idx := g.indexOf(code)
	if idx < 0 {
		return nil, false
	}
	return g.Items[idx], true
}

// UpdateItemNote 改某只自选股的备注。
//
// 开在根上而不是 item 上：包外根本拿不到能改动 item 的方法，
// 所有写入都必须先经过根，根才有机会维护 UpdatedAt 之类的整体状态。
func (g *WatchlistGroup) UpdateItemNote(code shared_vo.StockCode, note value_objects.ItemNote) error {
	it, ok := g.ItemByCode(code)
	if !ok {
		return custom_errors.NotFound(
			"分组「%s」中不存在 %s", g.Name.String(), code.FullSymbol())
	}
	now := time.Now()
	it.setNote(note, now)
	g.UpdatedAt = now
	return nil
}

// Reorder 按给定顺序重排组内自选股。
//
// # 入参语义
//
// ordered 里的代码按给定次序排到最前面；未提及的自选项保持它们原有的相对顺序
// 接在后面。允许只传一部分，是因为前端的拖拽只会告诉你「被拖动的那几个去哪了」，
// 要求它每次回传完整的两百条纯属浪费。
//
// 但**不允许**出现重复代码或组内不存在的代码：那两种都是客户端的真实错误
// （重复提交、拿着已被删掉的票去排序），静默忽略只会让顺序诡异地对不上，
// 用户以为是自己没拖准。
//
// 重排完成后强制重新编号，稠密连续因此是构造出来的，而不是「希望调用方保持」的。
func (g *WatchlistGroup) Reorder(ordered []shared_vo.StockCode) error {
	if len(ordered) == 0 {
		return custom_errors.Invalid("排序列表不能为空")
	}

	seen := make(map[string]struct{}, len(ordered))
	picked := make([]*WatchlistItem, 0, len(g.Items))
	for _, code := range ordered {
		key := codeKey(code)
		if _, dup := seen[key]; dup {
			return custom_errors.Invalid("排序列表中 %s 出现了多次", code.FullSymbol())
		}
		it, ok := g.ItemByCode(code)
		if !ok {
			return custom_errors.Invalid(
				"排序列表包含分组「%s」中不存在的股票 %s", g.Name.String(), code.FullSymbol())
		}
		seen[key] = struct{}{}
		picked = append(picked, it)
	}

	// 未提及的部分按原有相对顺序补在后面。
	for _, it := range g.Items {
		if _, ok := seen[codeKey(it.Code)]; !ok {
			picked = append(picked, it)
		}
	}

	now := time.Now()
	g.Items = picked
	g.resequence(now)
	g.UpdatedAt = now
	return nil
}

// ---------------------------------------------------------------------------
// 完整性自检
// ---------------------------------------------------------------------------

// Validate 在写库之前自检整个聚合，由仓储的 Save 调用。
//
// # 为什么需要它
//
// 「子实体只能经由根产生与修改」这条规则的绝大部分由包可见性守住了：
// newWatchlistItem 与 setXxx 都不导出。唯一的缺口是 Items 切片本身导出——
// 包外可以 append 一个自己 new 出来的 WatchlistItem 进去。
// 编译器拦不住这一步，但那样造出来的子实体拿不到 attached 凭据
// （不导出的字段包外赋不了值），于是这里能把它揪出来。
//
// 它同时复查另外几条不变式。这不是不信任 AddItem——它们在那里已经判过了；
// 而是因为这是聚合离开内存、变成持久化事实之前的最后一道关：
// 一旦写进去，破损的状态就会被后续的每一次加载当成既成事实接受。
func (g *WatchlistGroup) Validate() error {
	if g.UserID == 0 {
		return custom_errors.Invalid("自选分组必须归属于一个用户")
	}
	if g.Name.IsZero() {
		return custom_errors.Invalid("分组名不能为空")
	}
	if len(g.Items) > MaxItemsPerGroup {
		return custom_errors.QuotaExceeded(
			"分组「%s」最多容纳 %d 只股票，当前 %d 只",
			g.Name.String(), MaxItemsPerGroup, len(g.Items))
	}

	seen := make(map[string]struct{}, len(g.Items))
	for i, it := range g.Items {
		if it == nil {
			return custom_errors.Internal("分组「%s」的第 %d 个自选项为空", g.Name.String(), i)
		}
		if !it.isAttached() {
			return custom_errors.Internal(
				"自选项 %s 不是由聚合根创建的：自选项只能经由 WatchlistGroup.AddItem 产生",
				it.Code.FullSymbol())
		}
		if it.GroupID != g.ID {
			return custom_errors.Internal(
				"自选项 %s 归属分组(id=%d) 与当前分组(id=%d) 不一致",
				it.Code.FullSymbol(), it.GroupID, g.ID)
		}
		if it.Code.IsZero() {
			return custom_errors.Invalid("分组「%s」的第 %d 个自选项缺少股票代码", g.Name.String(), i)
		}
		key := codeKey(it.Code)
		if _, dup := seen[key]; dup {
			return custom_errors.AlreadyExists(
				"分组「%s」中 %s 重复", g.Name.String(), it.Code.FullSymbol())
		}
		seen[key] = struct{}{}
		if it.SortOrder != i {
			return custom_errors.Internal(
				"分组「%s」的排序不连续：第 %d 项的 SortOrder 为 %d",
				g.Name.String(), i, it.SortOrder)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// resequence 把 SortOrder 重新刷成 0..n-1。
//
// 只有真正变了的那几项会被打上新的 UpdatedAt（见 setSortOrder），
// 仓储据此只 UPDATE 发生了变化的行。
func (g *WatchlistGroup) resequence(now time.Time) {
	for i, it := range g.Items {
		it.setSortOrder(i, now)
	}
}

// bindPersistedID 在根首次落库拿到自增主键之后，把它同步给全部子实体。
// 仅供同包与仓储通过导出包装调用，见 AssignPersistedID。
func (g *WatchlistGroup) bindPersistedID(id uint64) {
	g.ID = id
	for _, it := range g.Items {
		it.bindGroup(id)
	}
}

// AssignPersistedID 供仓储在 INSERT 之后回填自增主键。
//
// 它必须开在根上：新建分组时子实体的 group_id 还是 0，回填只发生一次，
// 且必须同时刷到根和全部子实体上，否则紧接着的子实体 INSERT 会写进
// group_id = 0 这条黑洞记录里。让仓储自己去遍历 Items 改 GroupID 是不行的——
// 那是包外代码在修改子实体。
func (g *WatchlistGroup) AssignPersistedID(id uint64) { g.bindPersistedID(id) }

// PendingItems 返回尚未落库的子实体（ID == 0），按它们在组内的顺序。
//
// 它是仓储做子实体 diff 的输入之一：仓储据此知道「这几行要 INSERT」，
// 而不需要自己去猜。返回的仍然是只读视图——包外拿到指针也调不动任何修改方法。
func (g *WatchlistGroup) PendingItems() []*WatchlistItem {
	out := make([]*WatchlistItem, 0, len(g.Items))
	for _, it := range g.Items {
		if it.ID == 0 {
			out = append(out, it)
		}
	}
	return out
}

// AssignPersistedItemIDs 在子实体批量 INSERT 之后回填它们的自增主键。
//
// ids 必须与 PendingItems() 的返回顺序一一对应——批量 INSERT 的主键
// 就是按这个顺序生成的。长度对不上说明仓储与聚合对「哪些是新行」的理解出现了
// 分歧，那种情况下继续回填只会把主键错配到别的行上，不如立刻失败。
//
// 同样开在根上：子实体的任何字段都只能由根写入，主键也不例外。
func (g *WatchlistGroup) AssignPersistedItemIDs(ids []uint64) error {
	pending := g.PendingItems()
	if len(pending) != len(ids) {
		return custom_errors.Internal(
			"自选项主键回填数量不匹配：待回填 %d 条，实际拿到 %d 个主键",
			len(pending), len(ids))
	}
	for i, it := range pending {
		it.ID = ids[i]
	}
	return nil
}

// indexOf 线性查找。组内上限 200，线性扫比维护一个需要跟着增删同步的 map 更划算，
// 也让聚合保持「纯数据 + 方法」的形状，便于直接从 DTO 重建。
func (g *WatchlistGroup) indexOf(code shared_vo.StockCode) int {
	if code.IsZero() {
		return -1
	}
	key := codeKey(code)
	for i, it := range g.Items {
		if codeKey(it.Code) == key {
			return i
		}
	}
	return -1
}

// codeKey 是股票代码在组内的判重键。
//
// 用 (market, symbol) 而不是整个 StockCode 结构体：StockCode 还带一个 Raw 字段
// 保存用户的原始输入，"600519" 和 "600519.SH" 的 Raw 不同、Symbol 相同，
// 直接比结构体会把同一只票判成两只，重复自选就这样漏进来了。
func codeKey(code shared_vo.StockCode) string {
	return code.Market.String() + ":" + code.Symbol
}
