// Package domain_services 编排 agent 上下文的用例。
//
// 分层职责：
//   - 业务不变式属于 entities/，本层不重复判定；
//   - 事务属于 repositories/，本层不持有任何事务句柄；
//   - application/ 只放 handler，所有编排都在这里。
//
// 本层同时声明它所消费的全部外部端口。按 Go 惯例由消费方声明接口：
// 本包只认这些签名，实现分别位于 helpers/llm（模型客户端）与 stock 上下文（行情读取），
// 测试里可以直接换成返回固定结果的桩。
package domain_services

import (
	"context"
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// LLMClient 是单次模型调用端口。
//
// 它的粒度刻意停在「一次请求一次响应」：工具调用循环、重试语义、预算控制
// 都是业务决策，属于 RuntimeService；客户端只负责把领域模型翻译成某一家的线上协议。
// 这条分界让新增一家厂商不需要理解任何业务逻辑。
type LLMClient interface {
	// Provider 是厂商标识，用于路由与日志归因。
	Provider() string
	Complete(ctx context.Context, req value_objects.CompletionRequest) (*value_objects.CompletionResult, error)
}

// ModelRouter 按模型名解析出客户端与厂商侧的真实模型名。
//
// 返回真实模型名是必要的：路由支持 "deepseek/deepseek-chat" 这种显式写法，
// 前缀是路由指令而不是模型名的一部分，发给厂商前必须剥掉。
type ModelRouter interface {
	Resolve(model string) (LLMClient, string, error)
	DefaultModel() string
}

// ToolInvocation 是一次工具调用的入参。
//
// Code / TradeDate 由运行时下发而不是从模型参数里读：分析的标的是本次任务定死的，
// 让模型自己指定 symbol 意味着一次提示词注入就能让它去查另一只票，
// 而报告的标题还写着原来那只。模型能影响的只有 Arguments 里的辅助参数。
type ToolInvocation struct {
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Arguments json.RawMessage
}

// Tool 是一个可执行工具。
//
// 返回值是字符串而不是结构体：工具结果最终要作为一条消息塞回对话，
// 格式化成模型好读的文本是工具自己的职责——它最清楚哪些字段值得占用上下文。
type Tool interface {
	Spec() value_objects.ToolSpec
	Invoke(ctx context.Context, in ToolInvocation) (string, error)
}

// ToolRegistry 是工具注册表。
//
// Specs 只返回授权范围内的声明，Lookup 则在执行前做第二次校验。
// 两处都做不是冗余：只靠「没把声明发给模型」来限制工具，
// 在模型凭空幻觉出一个工具名时就失效了。
type ToolRegistry interface {
	Lookup(name value_objects.ToolName) (Tool, bool)
	Specs(access value_objects.DataAccess) []value_objects.ToolSpec
}

// MarketReader 是本上下文对 stock 限界上下文的只读依赖。
//
// 全部签名都用值对象，因此 stock 上下文的 MarketDataRepository 原样满足它，
// 不需要任何适配层。声明在消费方这一侧的意义是：agent 依赖的是
// 「能读到行情」这个能力，而不是 stock 的某个具体类型；
// 换成缓存、换成另一个数据服务，本包一行都不用改。
//
// 注意这里只有读、没有取数：跨上下文的外部数据补齐由 stock 上下文自己负责。
// 让十四位成员在工具调用里触发外部数据源拉取，就是在并发扇出里发 RPC——
// 一次分析可能打出几十个外部请求，而这些数据本该在数据准备阶段一次性备齐。
//
// # 为什么这里没有 LatestQuote
//
// MarketDataRepository 有 LatestQuote，本接口刻意不声明它。
// 本上下文的每一次读取都必须锚定在被分析的那个交易日上：回测 2024-03-01 的决策时，
// 让任何一位成员看到 3 月 5 日的价格都是未来函数——得出的结论准得可疑却毫无意义。
// 这条纪律在 lookbackRange（新闻、舆情）与 loadKlines（K 线）上一直成立，
// 唯独行情快照上漏过一次，而漏的原因正是端口里摆着一个不带交易日的方法。
//
// 因此这里不是「同时提供两个方法、请调用方选对那个」，而是把错的那个拿走：
// 少一个选项，就少一处需要靠人记住的约定。要「此刻的价格」的地方
// （自选股看板、模拟盘估值）不在本上下文，它们各自声明自己的端口。
type MarketReader interface {
	// QuoteAsOf 取截至 tradeDate 的行情快照；tradeDate 为零值时即「截至此刻」。
	QuoteAsOf(ctx context.Context, code shared_vo.StockCode, tradeDate shared_vo.TradeDate) (*stock_vo.Quote, error)
	Klines(ctx context.Context, code shared_vo.StockCode, period stock_vo.Period, rng shared_vo.DateRange, limit int) ([]stock_vo.Kline, error)
	// Financials 取截至 asOf 已**公开披露**的财报；asOf 为零值时不做披露过滤。
	//
	// 必须带 asOf 的理由与 QuoteAsOf 完全相同，只是更隐蔽：财报的自然键是报告期，
	// 而报告期 2023-12-31 的年报要到 2024 年 4 月底才公布。
	// 按报告期取「最近 N 期」，回测 2024-03-01 时会拿到一个月后才存在的年报——
	// 一处不会报错、只会让结论准得可疑的未来函数。
	Financials(ctx context.Context, code shared_vo.StockCode, asOf shared_vo.TradeDate, limit int) ([]stock_vo.Financial, error)
	News(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]stock_vo.News, error)
	SocialPosts(ctx context.Context, code shared_vo.StockCode, rng shared_vo.DateRange, limit int) ([]stock_vo.SocialPost, error)
}

// MarketBackfiller 是数据准备阶段的可选回源端口，由 stock 上下文的 StockService 满足。
//
// 它只在整条流水线的第一步、且本地 K 线为空时被调用一次——
// 冷启动或新标的的场景下总得有人去把数据拉回来，而那一次调用发生在
// 任何扇出开始之前，不在任何循环里。注入 nil 表示「只用本地数据」，
// 此时缺数据的分析师会如实报告「数据不足」。
type MarketBackfiller interface {
	Klines(ctx context.Context, in stock_services.KlineQuery) ([]stock_vo.Kline, error)
}
