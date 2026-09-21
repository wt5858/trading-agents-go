package providers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/wire"

	agent_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/application/http_handlers"
	analysis_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/http_handlers"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	identity_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/application/http_handlers"
	notification_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/application/http_handlers"
	notification_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/domain_services"
	paper_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/application/http_handlers"
	paper_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/domain_services"
	report_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/application/http_handlers"
	report_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_services"
	scheduling_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/application/http_handlers"
	scheduling_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_services"
	screening_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/application/http_handlers"
	screening_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	stock_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/application/http_handlers"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	system_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/application/http_handlers"
	system_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_services"
	watchlist_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/application/http_handlers"
	watchlist_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_services"
	"github.com/wt5858/trading-agents-go/internal/server"
)

// 本文件把 HTTP 处理器装配起来，并提供各上下文所需的 Operator 适配器。
//
// ===========================================================================
// Operator 适配器为什么住在组装根
// ===========================================================================
//
// 每个上下文都用自己的 Operator 值对象表达「谁在调用」，而登录身份由 identity
// 上下文的 Claims 承载。适配发生在这里，两边因此都不必认识对方：
// identity 不知道有多少上下文在用它的身份，各上下文也不知道身份是怎么来的。
//
// 换成让各上下文直接吃 identity 的 Claims，就等于所有上下文在编译期
// 依赖身份上下文——而那是一个会随着权限模型演进而频繁改动的地方。

func NewAnalysisOperator() analysis_handlers.OperatorResolver {
	return func(c *gin.Context) (analysis_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return analysis_services.Operator{}, false
		}
		return analysis_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewReportOperator() report_handlers.OperatorResolver {
	return func(c *gin.Context) (report_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return report_services.Operator{}, false
		}
		return report_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewStockOperator() stock_handlers.SyncOperatorResolver {
	return func(c *gin.Context) (stock_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return stock_services.Operator{}, false
		}
		return stock_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewSchedulingOperator() scheduling_handlers.OperatorResolver {
	return func(c *gin.Context) (scheduling_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return scheduling_services.Operator{}, false
		}
		return scheduling_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewSystemOperator() system_handlers.OperatorResolver {
	return func(c *gin.Context) (system_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return system_services.Operator{}, false
		}
		return system_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewWatchlistOperator() watchlist_handlers.OperatorResolver {
	return func(c *gin.Context) (watchlist_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return watchlist_services.Operator{}, false
		}
		return watchlist_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewPaperOperator() paper_handlers.OperatorResolver {
	return func(c *gin.Context) (paper_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return paper_services.Operator{}, false
		}
		return paper_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewNotificationOperator() notification_handlers.OperatorResolver {
	return func(c *gin.Context) (notification_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return notification_services.Operator{}, false
		}
		return notification_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

func NewScreeningOperator() screening_handlers.OperatorResolver {
	return func(c *gin.Context) (screening_services.Operator, bool) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			return screening_services.Operator{}, false
		}
		return screening_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, true
	}
}

// HTTPSet 装配全部 HTTP 处理器与服务器。
//
// server.Handlers 用 wire.Struct(..., "*") 填充：这个结构没有任何行为，
// 只是一份「路由层需要哪些处理器」的清单，逐个字段写一遍赋值毫无信息量，
// 而漏掉一个的表现是某一组接口静默地没有注册。
var HTTPSet = wire.NewSet(
	NewAnalysisOperator,
	NewReportOperator,
	NewStockOperator,
	NewSchedulingOperator,
	NewSystemOperator,
	NewWatchlistOperator,
	NewPaperOperator,
	NewNotificationOperator,
	NewScreeningOperator,

	identity_handlers.NewAuthHandler,
	identity_handlers.NewUserHandler,
	stock_handlers.NewStockHandler,
	stock_handlers.NewSyncHandler,
	agent_handlers.NewAgentHandler,
	analysis_handlers.NewAnalysisHandler,
	report_handlers.NewReportHandler,
	scheduling_handlers.NewScheduledJobHandler,
	system_handlers.NewConfigHandler,
	watchlist_handlers.NewWatchlistHandler,
	paper_handlers.NewPaperTradingHandler,
	notification_handlers.NewNotificationHandler,
	screening_handlers.NewScreeningHandler,

	wire.Struct(new(server.Handlers), "*"),
	server.New,
)
