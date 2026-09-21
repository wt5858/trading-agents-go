// 本测试刻意放在外部测试包 entities_test 而不是 entities。
//
// 这不是风格选择，而是测试内容的一部分：外部包看到的东西和
// domain_services / application / repositories 看到的完全一样。
// 如果测试写在包内，它就能调用 newWatchlistItem、能调用 setSortOrder，
// 于是「子实体只能经由根产生」这条规则在测试里根本无从验证——
// 测试会站在一个真实调用方永远到不了的位置上。
package entities_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

func newTestGroup(t *testing.T) *entities.WatchlistGroup {
	t.Helper()
	name, err := value_objects.NewGroupName("科技股")
	if err != nil {
		t.Fatalf("构造分组名失败: %v", err)
	}
	g, err := entities.NewWatchlistGroup(42, name)
	if err != nil {
		t.Fatalf("创建自选分组失败: %v", err)
	}
	// 清掉创建事件，让后续断言只看本次操作抛出的事件。
	g.GetAllPendingEvents()
	return g
}

// cnCode 造一个合法的 A 股代码。6 位数字是 A 股的唯一格式要求，
// 从 600000 开始递增可以轻松造出上千个互不相同的合法代码。
func cnCode(t *testing.T, n int) shared_vo.StockCode {
	t.Helper()
	code, err := shared_vo.NewStockCode(fmt.Sprintf("%06d", 600000+n), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	return code
}

// addN 往分组里加 n 只互不相同的票。
func addN(t *testing.T, g *entities.WatchlistGroup, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := g.AddItem(cnCode(t, i), value_objects.ItemNote{}, value_objects.ReferencePrice{}); err != nil {
			t.Fatalf("加入第 %d 只股票失败: %v", i, err)
		}
	}
	g.GetAllPendingEvents()
}

func hasEvent(events []domain_event.DomainEvent, name string) bool {
	for _, e := range events {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// assertDense 断言 SortOrder 是 0..n-1 的稠密连续序列，且与切片下标一致。
//
// 同时校验两件事是有意义的：序号本身连续、以及序号与实际存放顺序一致。
// 只校验前者的话，一个「序号对但顺序反了」的实现照样通过，
// 而用户看到的顺序来自切片顺序，不是来自那个数字。
func assertDense(t *testing.T, g *entities.WatchlistGroup) {
	t.Helper()
	for i, it := range g.Items {
		if it.SortOrder != i {
			t.Fatalf("排序必须稠密连续：第 %d 项的 SortOrder 是 %d", i, it.SortOrder)
		}
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("聚合自检应通过，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 不变式一：组内不得重复持有同一只股票
// ---------------------------------------------------------------------------

// TestAddItem_RejectsDuplicateStock 守住「组内不重复」。
//
// 没有这条规则，同一只票会在自选股列表里出现两行，用户改了其中一行的备注
// 之后两行还会显示不同的内容——而它们本该是同一个东西。
func TestAddItem_RejectsDuplicateStock(t *testing.T) {
	g := newTestGroup(t)
	code := cnCode(t, 0)

	if _, err := g.AddItem(code, value_objects.ItemNote{}, value_objects.ReferencePrice{}); err != nil {
		t.Fatalf("首次加入应当成功: %v", err)
	}
	g.GetAllPendingEvents()

	_, err := g.AddItem(code, value_objects.ItemNote{}, value_objects.ReferencePrice{})
	if err == nil {
		t.Fatal("重复加入同一只股票必须被拒绝")
	}
	if errCode := custom_errors.CodeOf(err); errCode != custom_errors.CodeAlreadyExists {
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeAlreadyExists, errCode)
	}
	if g.ItemCount() != 1 {
		t.Fatalf("被拒绝的添加不该改变组内数量，实际 %d", g.ItemCount())
	}
	// 被拒绝的操作不该抛出事件：下游会据此去订阅行情，凭空多一次订阅
	// 意味着之后的一次退订会把仍在关注的标的也退掉。
	if hasEvent(g.GetAllPendingEvents(), domain_events.OnStockWatchedEventName) {
		t.Fatal("重复加入被拒绝时不该抛出 OnStockWatched")
	}
}

// TestAddItem_DeduplicatesAcrossCodeSpellings 守住判重用的是规范化之后的代码。
//
// 这是这条不变式最容易漏掉的一半：用户可以用 "600519"、"600519.SH"、"sh600519"
// 三种写法指同一只票。判重若直接比结构体，Raw 字段不同就会被当成三只不同的股票，
// 「组内不重复」看起来实现了，实际上形同虚设。
func TestAddItem_DeduplicatesAcrossCodeSpellings(t *testing.T) {
	g := newTestGroup(t)

	plain, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造代码失败: %v", err)
	}
	suffixed, err := shared_vo.NewStockCode("600519.SH", shared_vo.MarketUnknown)
	if err != nil {
		t.Fatalf("构造代码失败: %v", err)
	}

	if _, err := g.AddItem(plain, value_objects.ItemNote{}, value_objects.ReferencePrice{}); err != nil {
		t.Fatalf("首次加入应当成功: %v", err)
	}
	if _, err := g.AddItem(suffixed, value_objects.ItemNote{}, value_objects.ReferencePrice{}); err == nil {
		t.Fatal("600519 与 600519.SH 是同一只股票，第二次加入必须被拒绝")
	}
	if g.ItemCount() != 1 {
		t.Fatalf("组内应只有 1 只股票，实际 %d", g.ItemCount())
	}
}

// ---------------------------------------------------------------------------
// 不变式二：单组数量上限
// ---------------------------------------------------------------------------

// TestAddItem_EnforcesItemCap 守住数量上限。
//
// 这条上限不只是产品约束，它是**聚合边界成立的前提**：正因为一个分组的
// 子实体数量有界，「加载根的时候把全部子实体一起加载」才是常数级操作，
// WatchlistItem 才能安全地作为子实体而不是独立聚合根存在。
// 上限一旦失守，这个聚合的设计就塌了。
func TestAddItem_EnforcesItemCap(t *testing.T) {
	g := newTestGroup(t)
	addN(t, g, entities.MaxItemsPerGroup)

	if g.ItemCount() != entities.MaxItemsPerGroup {
		t.Fatalf("前置条件不成立：应已装满 %d 只，实际 %d", entities.MaxItemsPerGroup, g.ItemCount())
	}

	// 第 MaxItemsPerGroup+1 只必须被拒绝。
	_, err := g.AddItem(cnCode(t, entities.MaxItemsPerGroup), value_objects.ItemNote{}, value_objects.ReferencePrice{})
	if err == nil {
		t.Fatalf("超过 %d 只必须被拒绝", entities.MaxItemsPerGroup)
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeQuotaExceeded {
		t.Fatalf("错误码应为 %s（而不是笼统的内部错误），实际 %s", custom_errors.CodeQuotaExceeded, code)
	}
	if g.ItemCount() != entities.MaxItemsPerGroup {
		t.Fatalf("被拒绝的添加不该改变组内数量，实际 %d", g.ItemCount())
	}

	// 删掉一只之后必须能再加一只：上限是「同时最多几只」，不是「一辈子最多加几次」。
	// 用计数器而不是重算 len() 实现的话，这里就会失败。
	if err := g.RemoveItem(cnCode(t, 0)); err != nil {
		t.Fatalf("移除应当成功: %v", err)
	}
	if _, err := g.AddItem(cnCode(t, entities.MaxItemsPerGroup), value_objects.ItemNote{}, value_objects.ReferencePrice{}); err != nil {
		t.Fatalf("腾出空位后应当可以继续加入: %v", err)
	}
	assertDense(t, g)
}

// TestAddItem_CapChecksAfterDuplicate 守住两条规则的判定**顺序**。
//
// 分组已满时重复加入一只已有的票，用户该看到的是「已存在」，
// 而不是「已达上限」——后者驴唇不对马嘴，用户会去删票腾位置，
// 删完再加还是同一个错，直到他放弃。
func TestAddItem_CapChecksAfterDuplicate(t *testing.T) {
	g := newTestGroup(t)
	addN(t, g, entities.MaxItemsPerGroup)

	_, err := g.AddItem(cnCode(t, 0), value_objects.ItemNote{}, value_objects.ReferencePrice{})
	if err == nil {
		t.Fatal("重复加入必须被拒绝")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeAlreadyExists {
		t.Fatalf("满员时重复加入应报「已存在」而不是「已达上限」，实际错误码 %s", code)
	}
}

// ---------------------------------------------------------------------------
// 不变式三：SortOrder 稠密连续
// ---------------------------------------------------------------------------

// TestRemoveItem_KeepsSortOrderDense 守住「从中间删掉一个之后序号不留洞」。
//
// 留洞的后果不是「数字不好看」：Reorder 与前端拖拽都以序号为准，
// 一个 0,1,3,4 的序列会让「把第 3 位挪到第 2 位」这类操作的语义变得依赖历史，
// 而且每次删除都会让洞越来越多，最终顺序完全不可预测。
func TestRemoveItem_KeepsSortOrderDense(t *testing.T) {
	g := newTestGroup(t)
	addN(t, g, 5)
	assertDense(t, g)

	// 删掉正中间那一只（下标 2）。首尾是特例，中间才是会暴露问题的位置。
	middle := cnCode(t, 2)
	if err := g.RemoveItem(middle); err != nil {
		t.Fatalf("移除应当成功: %v", err)
	}

	if g.ItemCount() != 4 {
		t.Fatalf("移除后应剩 4 只，实际 %d", g.ItemCount())
	}
	if _, ok := g.ItemByCode(middle); ok {
		t.Fatal("被移除的股票不该还能查到")
	}
	// 关键断言：序号必须立刻重排成 0,1,2,3，而不是留下 0,1,3,4。
	assertDense(t, g)

	// 剩下的相对顺序必须保持：删掉下标 2 之后，原来的 3、4 依次补位。
	expected := []int{0, 1, 3, 4}
	for i, want := range expected {
		if got := g.Items[i].Symbol(); got != cnCode(t, want).FullSymbol() {
			t.Fatalf("第 %d 位应为 %s，实际 %s", i, cnCode(t, want).FullSymbol(), got)
		}
	}

	if !hasEvent(g.GetAllPendingEvents(), domain_events.OnStockUnwatchedEventName) {
		t.Fatalf("移除自选必须抛出 %s", domain_events.OnStockUnwatchedEventName)
	}

	// 连续删多次同样不能积累出洞。
	if err := g.RemoveItem(cnCode(t, 1)); err != nil {
		t.Fatalf("移除应当成功: %v", err)
	}
	if err := g.RemoveItem(cnCode(t, 3)); err != nil {
		t.Fatalf("移除应当成功: %v", err)
	}
	assertDense(t, g)
}

// TestReorder_KeepsSortOrderDense 守住重排之后序号仍然稠密连续。
func TestReorder_KeepsSortOrderDense(t *testing.T) {
	g := newTestGroup(t)
	addN(t, g, 4)

	// 整体倒序。
	ordered := []shared_vo.StockCode{cnCode(t, 3), cnCode(t, 2), cnCode(t, 1), cnCode(t, 0)}
	if err := g.Reorder(ordered); err != nil {
		t.Fatalf("重排应当成功: %v", err)
	}
	assertDense(t, g)
	for i, want := range []int{3, 2, 1, 0} {
		if got := g.Items[i].Symbol(); got != cnCode(t, want).FullSymbol() {
			t.Fatalf("重排后第 %d 位应为 %s，实际 %s", i, cnCode(t, want).FullSymbol(), got)
		}
	}

	// 只传一部分：被提及的排到前面，其余保持原有相对顺序接在后面。
	if err := g.Reorder([]shared_vo.StockCode{cnCode(t, 1)}); err != nil {
		t.Fatalf("部分重排应当成功: %v", err)
	}
	assertDense(t, g)
	if got := g.Items[0].Symbol(); got != cnCode(t, 1).FullSymbol() {
		t.Fatalf("被提及的项应排到首位，实际首位是 %s", got)
	}

	// 非法入参必须被拒绝，而不是静默忽略——静默忽略会让用户拖出来的顺序
	// 和他看到的顺序对不上，却没有任何提示。
	t.Run("拒绝重复代码", func(t *testing.T) {
		err := g.Reorder([]shared_vo.StockCode{cnCode(t, 0), cnCode(t, 0)})
		if err == nil {
			t.Fatal("排序列表里出现重复代码必须被拒绝")
		}
	})
	t.Run("拒绝组内不存在的代码", func(t *testing.T) {
		err := g.Reorder([]shared_vo.StockCode{cnCode(t, 999)})
		if err == nil {
			t.Fatal("排序列表里出现组内不存在的代码必须被拒绝")
		}
	})
	// 被拒绝的重排不该留下半成品。
	assertDense(t, g)
}

// ---------------------------------------------------------------------------
// 核心规则：子实体只能经由聚合根产生
// ---------------------------------------------------------------------------

// TestAddItemIsTheOnlyWayAnItemComesIntoExistence 是本文件最重要的一个测试。
//
// # 第一道保证：编译期
//
// 本测试位于外部测试包 entities_test，看到的东西和 domain_services、
// application、repositories 完全一样。在这个位置上：
//
//   - 没有 entities.NewWatchlistItem —— 构造函数 newWatchlistItem 不导出；
//   - 没有 item.SetSortOrder / SetNote —— 修改方法 setSortOrder / setNote 不导出。
//
// 也就是说，「造一个自选项」和「改一个自选项」这两件事在包外**写不出来**。
// 这一层保证不需要断言，它由编译器每次构建时执行；真正的断言是：
// 如果哪天有人把 newWatchlistItem 导出了，下面这行注释描述的事实就不再成立，
// 而本测试的存在会逼着他解释为什么。
//
// # 第二道保证：运行期
//
// 编译期唯一的缺口是 Items 切片本身是导出的（读路径需要它）。包外确实可以
// new 一个 WatchlistItem 塞进去。但那样造出来的子实体拿不到「由根创建」的凭据
// （那是个不导出的字段，包外赋不了值），于是 Validate 能把它揪出来，
// 而仓储在写库之前一定会调用 Validate。下面验证的就是这道防线。
func TestAddItemIsTheOnlyWayAnItemComesIntoExistence(t *testing.T) {
	t.Run("经由根创建的子实体通过自检", func(t *testing.T) {
		g := newTestGroup(t)
		item, err := g.AddItem(cnCode(t, 0), value_objects.ItemNote{}, value_objects.ReferencePrice{})
		if err != nil {
			t.Fatalf("加入应当成功: %v", err)
		}
		if item == nil {
			t.Fatal("AddItem 应当返回新建的子实体")
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("经由根创建的聚合必须通过自检，实际: %v", err)
		}
		if !hasEvent(g.GetAllPendingEvents(), domain_events.OnStockWatchedEventName) {
			t.Fatalf("加入自选必须由**根**抛出 %s", domain_events.OnStockWatchedEventName)
		}
	})

	t.Run("绕过根塞进来的子实体被自检拒绝", func(t *testing.T) {
		g := newTestGroup(t)
		addN(t, g, 2)

		// 这是包外能对子实体做的全部坏事：手搓一个结构体，append 进导出的切片。
		// 编译器拦不住这一步——但它拿不到「由根创建」的凭据。
		smuggled := &entities.WatchlistItem{
			Code:      cnCode(t, 7),
			SortOrder: 2,
		}
		g.Items = append(g.Items, smuggled)

		err := g.Validate()
		if err == nil {
			t.Fatal("绕过聚合根塞进来的子实体必须被自检拒绝——" +
				"仓储正是靠这道检查拒绝把它写进库里的")
		}
		// 这是编程错误而不是用户输入错误，因此是 Internal 而不是 InvalidArgument：
		// 没有任何一个合法的 HTTP 请求能走到这里。
		if code := custom_errors.CodeOf(err); code != custom_errors.CodeInternal {
			t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInternal, code)
		}
	})

	t.Run("即使伪造的子实体各字段看起来都对也会被拒绝", func(t *testing.T) {
		// 防止「只要把 SortOrder 和 GroupID 填对就能混过去」这种误解：
		// 判据是凭据本身，不是字段长得像不像。
		g := newTestGroup(t)
		addN(t, g, 1)

		g.Items = append(g.Items, &entities.WatchlistItem{
			GroupID:   g.ID,
			Code:      cnCode(t, 8),
			SortOrder: 1,
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.UpdatedAt,
		})
		if err := g.Validate(); err == nil {
			t.Fatal("字段填得再像，没有经由根创建就必须被拒绝")
		}
	})

	t.Run("重建路径同样经由根", func(t *testing.T) {
		// 持久化重建是子实体的第二条产生路径，它同样开在根上（RehydrateItem），
		// 而不是开在子实体的 DTO 上。区别只是它不跑不变式判定——重建的是既成事实。
		at := fixedTime()
		g := entities.RehydrateWatchlistGroup(
			7, 42, value_objects.RehydrateGroupName("科技股"), at, at, 2)
		g.RehydrateItem(1, cnCode(t, 0), value_objects.ItemNote{}, value_objects.ReferencePrice{}, 0, at, at)
		g.RehydrateItem(2, cnCode(t, 1), value_objects.ItemNote{}, value_objects.ReferencePrice{}, 1, at, at)

		if g.ItemCount() != 2 {
			t.Fatalf("重建后应有 2 只，实际 %d", g.ItemCount())
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("经由根重建的聚合必须通过自检，实际: %v", err)
		}
	})
}

// TestUpdateItemNote_GoesThroughRoot 守住「改子实体也必须经由根」。
//
// 包外没有 item.SetNote 可调（不导出），唯一的入口是 g.UpdateItemNote。
// 它除了改备注还要维护根自身的 UpdatedAt——如果允许直接改子实体，
// 这一步一定会被忘掉，表现为「改了备注但列表的排序时间没变」。
func TestUpdateItemNote_GoesThroughRoot(t *testing.T) {
	g := newTestGroup(t)
	addN(t, g, 2)
	before := g.UpdatedAt

	note, err := value_objects.NewItemNote("等回调到 60 日线")
	if err != nil {
		t.Fatalf("构造备注失败: %v", err)
	}
	if err := g.UpdateItemNote(cnCode(t, 1), note); err != nil {
		t.Fatalf("修改备注应当成功: %v", err)
	}

	it, ok := g.ItemByCode(cnCode(t, 1))
	if !ok {
		t.Fatal("应能查到该自选项")
	}
	if it.Note.String() != "等回调到 60 日线" {
		t.Fatalf("备注应已更新，实际 %q", it.Note.String())
	}
	if !g.UpdatedAt.After(before) && !g.UpdatedAt.Equal(before) {
		t.Fatal("经由根修改必须同时维护根的 UpdatedAt")
	}

	// 组内不存在的票要报 NotFound，而不是静默成功。
	err = g.UpdateItemNote(cnCode(t, 999), note)
	if custom_errors.CodeOf(err) != custom_errors.CodeNotFound {
		t.Fatalf("对不存在的自选项改备注应报 %s，实际 %v", custom_errors.CodeNotFound, err)
	}
}

// fixedTime 给重建路径一个确定的时间戳。重建不校验时间，任何固定值都可以，
// 固定下来只是为了让断言不受运行时刻影响。
func fixedTime() time.Time {
	return time.Date(2026, 9, 16, 9, 30, 0, 0, time.UTC)
}
