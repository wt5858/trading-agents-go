package providers

import (
	"github.com/google/wire"

	agent_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/domain_services"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	scheduling_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// CoreSet 是全部业务上下文加上它们之间的接缝。
//
// ===========================================================================
// 跨上下文的 wire.Bind 为什么必须集中在这里
// ===========================================================================
//
// Wire 有一条硬规则：一条 wire.Bind 只能写在**同时包含了那个具体类型的 provider**
// 的 set 里。跨上下文的绑定（analysis 需要一个 Engine，而实现它的是 agent）
// 因此没法写在任何一个单独的上下文 set 里——AnalysisSet 不提供 EngineService。
//
// 这条规则其实在帮我们：它逼着「哪个上下文实现了哪个上下文的端口」这件事
// 集中到一处，而不是散落在各个 set 里。下面这四行就是本服务全部的跨上下文依赖，
// 它是一份可以一眼读完的清单——新增一条就必须在这里显式写下来。
//
// 依赖方向也一目了然：
//
//	analysis  -> agent     跑一次完整流水线
//	agent     -> stock     读行情、冷启动回源
//	scheduling -> pkg/mq   投递到期消息
//
// 反过来一条都没有：stock 不知道 agent 存在，agent 不知道 analysis 存在。
var CoreSet = wire.NewSet(
	InfraSet,
	IdentitySet,
	SystemConfigSet,
	StockSet,
	AgentSet,
	AnalysisSet,
	SchedulingSet,

	// agent 读行情走 stock 的仓储，冷启动回源走 stock 的服务。
	// 两个端口都由 agent 自己声明（消费方定义接口），stock 对此一无所知。
	wire.Bind(new(agent_services.MarketReader), new(*stock_repo.MarketDataRepository)),
	wire.Bind(new(agent_services.MarketBackfiller), new(*stock_services.StockService)),

	// analysis 只认「能跑一次完整流水线」这个能力，不认 agent 的服务类型。
	wire.Bind(new(analysis_services.Engine), new(*agent_services.EngineService)),

	// 读决策链是另一个能力，绑到另一个实现上：能看轨迹不等于能发起一次分析。
	wire.Bind(new(analysis_services.DecisionChainReader), new(*agent_services.DecisionChainService)),

	// scheduling 只认「把一个字符串发到某个路由键上」，不认整个消息队列客户端。
	wire.Bind(new(scheduling_repo.MessagePublisher), new(*mq.AMQP)),
)
