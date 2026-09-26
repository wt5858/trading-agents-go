//go:build wireinject

// Package injectors 是组装根的**声明**：每个函数说明一个入口点需要什么，
// 具体怎么造由 Wire 按 providers 里的 provider set 推导出来。
//
// 本文件带 wireinject 构建标签，正常构建时不参与编译；
// 真正跑起来的是 wire 生成的 wire_gen.go。改完本文件必须重新生成：
//
//	go generate ./internal/di/injectors/
//
// ===========================================================================
// 为什么按入口点切成几个注入器，而不是造一个大容器
// ===========================================================================
//
// 因为**不同进程需要的东西不一样**，而这个差别应当由类型系统表达，不是由注释。
//
// 最要紧的一条：HTTP 进程绝不能消费任何队列。两个进程都消费的话，一条领域事件
// 会随机落到其中一个，而它们注册的处理器并不相同——故障表现是「有时候好使」，
// 没有任何错误日志。切成几个注入器之后，CreateHTTPServer 的依赖图里根本
// 不包含任何消费者，想错都错不了。
//
// 次要但同样实在：一个大容器意味着 HTTP 进程启动时也要把分析引擎、调度器、
// LLM 路由全都造出来——包括那次为了加载供应商而发生的数据库查询。
package injectors

import (
	"context"

	"github.com/google/wire"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	"github.com/wt5858/trading-agents-go/internal/helpers/event_handlers"
	"github.com/wt5858/trading-agents-go/internal/server"
)

// CreateHTTPServer 装配 HTTP 服务。
//
// 它需要几乎全部上下文的领域服务（接口要暴露它们），但**不包含任何消费者**。
func CreateHTTPServer(ctx context.Context, cfg *config.Config, log *zap.Logger) (*server.Server, error) {
	panic(wire.Build(
		providers.CoreSet,
		providers.ReportSet,
		providers.WatchlistSet,
		providers.PaperTradingSet,
		providers.ScreeningSet,
		providers.NotificationSet,
		providers.HTTPSet,
		providers.MCPSet,
	))
}

// CreateUserService 只装配身份上下文，供启动期创建初始管理员使用。
//
// 单独开一个注入器而不是从 HTTP 那套里顺手拿：创建管理员发生在 HTTP 服务
// 启动**之前**，它不需要 LLM 路由，也不该因为消息队列连不上而失败。
func CreateUserService(ctx context.Context, cfg *config.Config, log *zap.Logger) (*identity_services.UserService, error) {
	panic(wire.Build(
		providers.InfraSet,
		providers.IdentitySet,
	))
}

// CreateAmqpHandlers 装配消息队列消费者。只有 worker 进程调用。
func CreateAmqpHandlers(ctx context.Context, cfg *config.Config, log *zap.Logger) (*event_handlers.AmqpHandlers, error) {
	panic(wire.Build(
		providers.CoreSet,
		providers.SchedulingAmqpSet,
		event_handlers.NewAmqpHandlers,
	))
}

// CreateDomainEventHandlers 装配领域事件订阅者。只有 worker 进程调用。
func CreateDomainEventHandlers(ctx context.Context, cfg *config.Config, log *zap.Logger) (*event_handlers.DomainEventHandlers, error) {
	panic(wire.Build(
		providers.CoreSet,
		providers.DomainEventHandlersSet,
	))
}

// CreateWorkerRunners 装配 worker 的两个常驻循环。
func CreateWorkerRunners(ctx context.Context, cfg *config.Config, log *zap.Logger) (*providers.WorkerRunners, error) {
	panic(wire.Build(
		providers.CoreSet,
		providers.WorkerSet,
	))
}

// CreateAnalysisEngine 装配一台可直接驱动的分析引擎，供 backfill 子命令使用。
//
// # 为什么回填不走任务队列
//
// 走 AnalysisService.Submit 看起来能白捡认领、重试与停滞巡检，但它有两个
// 对批量回填致命的性质：提交时要占一个 Redis 并发名额（PerUser / Global 上限），
// 因此几百条一次提交必然在中途被自己的限流挡住；而且那些名额与线上用户共用，
// 一次回填会把交互式提交的分析全部挤掉——回填是可以慢慢跑的，
// 有人正等着看的那次不行。
//
// 自己驱动则可以把节奏完全捏在手里：一次跑一格、可中断、可续跑，
// 而成本上限由 EngineConfig.MaxCostUSD 在引擎内部把关，不依赖调用方自律。
//
// 它复用 CoreSet 而不是手工装配：LLM 路由要先从数据库读出供应商配置才能解析模型名，
// 这类依赖顺序由 Wire 从函数签名推导，手写一份迟早和 serve 那边分叉。
func CreateBackfillDeps(ctx context.Context, cfg *config.Config, log *zap.Logger) (*providers.BackfillDeps, error) {
	panic(wire.Build(
		providers.CoreSet,
		providers.BackfillSet,
	))
}
