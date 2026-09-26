package domain_services

import (
	"context"
	"sync"
	"testing"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// asOfSpy 记录数据准备阶段每一类读取所使用的日期右端。
//
// # 这个 spy 为什么必须实现完整的 MarketReader
//
// 它的价值不在于本次断言，而在于将来：给 MarketReader 添一个新的数据源方法时，
// 这个类型会立刻编译失败，加方法的人必须回到本文件、把新数据源的日期上界
// 也记进 bounds——于是他不得不面对「我这条查询的右端锁在哪一天」这个问题。
//
// 换成 interface 嵌入或部分实现的 mock，新方法会被静默继承，
// 测试照常通过，而那正是行情快照当初漏掉的方式。
type asOfSpy struct {
	mu     sync.Mutex
	bounds map[string]string
	quote  stock_vo.Quote
}

func newAsOfSpy(q stock_vo.Quote) *asOfSpy {
	return &asOfSpy{bounds: map[string]string{}, quote: q}
}

// record 用互斥量保护：collect 的四类读取是并发跑的（concurrency.Settle），
// 不加锁时这个测试本身就是一次 map 并发写，-race 下必挂。
func (s *asOfSpy) record(kind string, bound shared_vo.TradeDate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bounds[kind] = bound.String()
}

func (s *asOfSpy) QuoteAsOf(
	_ context.Context, _ shared_vo.StockCode, tradeDate shared_vo.TradeDate,
) (*stock_vo.Quote, error) {
	s.record("行情快照", tradeDate)
	q := s.quote
	return &q, nil
}

func (s *asOfSpy) Klines(
	_ context.Context, _ shared_vo.StockCode, _ stock_vo.Period,
	rng shared_vo.DateRange, _ int,
) ([]stock_vo.Kline, error) {
	s.record("K线", rng.End)
	return nil, nil
}

func (s *asOfSpy) News(
	_ context.Context, _ shared_vo.StockCode, rng shared_vo.DateRange, _ int,
) ([]stock_vo.News, error) {
	s.record("资讯", rng.End)
	return nil, nil
}

func (s *asOfSpy) SocialPosts(
	_ context.Context, _ shared_vo.StockCode, rng shared_vo.DateRange, _ int,
) ([]stock_vo.SocialPost, error) {
	s.record("社交舆情", rng.End)
	return nil, nil
}

// Financials 的上界是「截至哪一天已经公开披露」。
//
// 这一条曾经是个已知缺口：端口签名一度是 Financials(ctx, code, limit)，
// 取的是「最近 limit 期」。而财报的 report_date 是报告期不是公布日，
// 回测 2024-03-01 时，一份报告期 2023-12-31、实际 2024-04 才披露的年报照样会被取到。
// 缺口由端口补上 asOf 参数修掉，本 spy 随之记录它——
// 这也正是「spy 必须实现完整接口」那条设计的兑现：端口一改，这里编译不过，
// 改的人不得不回来面对这个问题。
func (s *asOfSpy) Financials(
	_ context.Context, _ shared_vo.StockCode, asOf shared_vo.TradeDate, _ int,
) ([]stock_vo.Financial, error) {
	s.record("财务", asOf)
	return nil, nil
}

// 编译期钉住：spy 必须是一个完整的 MarketReader，不允许退化成部分实现。
var _ MarketReader = (*asOfSpy)(nil)

// TestCollectNeverReadsBeyondTradeDate 锁定数据准备阶段的核心纪律：
// 所有对外读取的日期右端都不得越过被分析的那个交易日。
//
// 这条纪律此前只写在 lookbackRange 的注释里，靠每个调用点自觉遵守，
// 结果新闻与 K 线遵守了，行情快照漏了——它调的是不带交易日的 LatestQuote，
// 于是回测历史某一天时，十四位成员看到的是今天的价格。
// 端口上已经把 LatestQuote 拿掉，本测试负责拦住下一次同类回归。
func TestCollectNeverReadsBeyondTradeDate(t *testing.T) {
	const tradeDate = "2024-03-01"

	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	quote, err := stock_vo.NewQuote(code, shared_vo.MustTradeDate(tradeDate))
	if err != nil {
		t.Fatalf("构造行情快照失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate(tradeDate),
		analysis_vo.DepthStandard, nil, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	spy := newAsOfSpy(quote)
	// indicators/runs/evals 传 nil 是刻意的：本测试只关心读取的日期边界，
	// 指标仓储缺失会被 collect 记进 Missing 而不阻断，正好省掉一个 Mongo 依赖。
	svc := NewEngineService(nil, spy, nil, nil, nil, nil, nil, nil, EngineConfig{})

	brief, err := svc.collect(context.Background(), req)
	if err != nil {
		t.Fatalf("数据准备失败: %v", err)
	}

	// 每一类读取都必须真的发生过。少一条说明数据准备的结构变了，
	// 而这个测试的覆盖面已经跟不上——此时假绿比失败更危险。
	wantKinds := []string{"行情快照", "K线", "资讯", "社交舆情", "财务"}
	for _, kind := range wantKinds {
		if _, ok := spy.bounds[kind]; !ok {
			t.Errorf("%s 没有被读取，本测试已覆盖不到它", kind)
		}
	}

	for kind, got := range spy.bounds {
		// 空右端必须单独判，不能只比大小。
		//
		// 零值交易日在下游一律被解释成「取最新」——这正是要防的那件事，
		// 而空串的字典序小于任何日期，`got > tradeDate` 会让它安然通过。
		// 一个把 req.TradeDate 漏传成零值的回归，恰好从这个缺口溜走。
		if got == "" {
			t.Errorf("%s 的查询右端是空的，等于「取最新」—— 这是未来函数", kind)
			continue
		}
		// 交易日是 ISO 格式（2006-01-02），字典序即时间序，可以直接比较。
		if got > tradeDate {
			t.Errorf("%s 的查询右端是 %s，越过了分析交易日 %s —— 这是未来函数",
				kind, got, tradeDate)
		}
	}

	// 行情快照必须真的落进 brief：只断言日期而不断言结果，
	// 会让一个「日期传对了但结果被丢弃」的实现照样通过。
	if brief.Quote.Code.IsZero() {
		t.Error("行情快照没有落进 MarketBrief")
	}
	if got := brief.Quote.TradeDate.String(); got != tradeDate {
		t.Errorf("行情快照的交易日是 %s，期望 %s", got, tradeDate)
	}
}

// TestQuoteToolUsesInvocationTradeDate 锁定 get_quote 工具的同一条纪律。
//
// 它与数据准备走的是两条独立路径：数据准备由 collect 组装 MarketBrief，
// 而工具是模型在对话中途主动调的。先前两条路径都读了今天的价格，
// 修其中一条不会让另一条自动正确，因此断言也必须分开。
func TestQuoteToolUsesInvocationTradeDate(t *testing.T) {
	const tradeDate = "2024-03-01"

	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	quote, err := stock_vo.NewQuote(code, shared_vo.MustTradeDate(tradeDate))
	if err != nil {
		t.Fatalf("构造行情快照失败: %v", err)
	}

	spy := newAsOfSpy(quote)
	tool := &quoteTool{market: spy}

	if _, err := tool.Invoke(context.Background(), ToolInvocation{
		Code:      code,
		TradeDate: shared_vo.MustTradeDate(tradeDate),
	}); err != nil {
		t.Fatalf("调用 get_quote 失败: %v", err)
	}

	got, ok := spy.bounds["行情快照"]
	if !ok {
		t.Fatal("get_quote 没有读取行情快照")
	}
	if got != tradeDate {
		t.Errorf("get_quote 的查询日期是 %s，期望锁定在分析交易日 %s", got, tradeDate)
	}
}
