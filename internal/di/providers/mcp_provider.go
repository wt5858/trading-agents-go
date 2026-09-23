package providers

import (
	"context"

	"github.com/google/wire"

	analysis_mcp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/mcp_tools"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	screening_mcp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/application/mcp_tools"
	screening_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	watchlist_mcp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/application/mcp_tools"
	watchlist_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_services"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/mcpserver"
)

// ===========================================================================
// MCP 侧的 Operator 适配器
// ===========================================================================
//
// 与本文件隔壁 http_provider.go 里那一组是同一件事的两个版本，
// 差别只有入参：那边吃 *gin.Context，这边吃 context.Context。
// 两边都住在组装根，理由也相同——identity 不必知道有多少上下文在用它的身份，
// 各上下文也不必知道身份是从 JWT 还是从别的什么地方来的。
//
// 不能直接复用 http_provider.go 那几个函数：它们的签名要求 *gin.Context，
// 而 MCP 走的是原生 http.Handler，手上只有 context.Context，
// 也不该为了复用在这里凭空造一个假的 gin 上下文。

func NewAnalysisMCPOperator() analysis_mcp.OperatorResolver {
	return func(ctx context.Context) (analysis_services.Operator, error) {
		claims := mcpserver.ClaimsOf(ctx)
		if claims == nil {
			return analysis_services.Operator{}, custom_errors.Unauthorized("未登录")
		}
		return analysis_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, nil
	}
}

func NewWatchlistMCPOperator() watchlist_mcp.OperatorResolver {
	return func(ctx context.Context) (watchlist_services.Operator, error) {
		claims := mcpserver.ClaimsOf(ctx)
		if claims == nil {
			return watchlist_services.Operator{}, custom_errors.Unauthorized("未登录")
		}
		return watchlist_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, nil
	}
}

func NewScreeningMCPOperator() screening_mcp.OperatorResolver {
	return func(ctx context.Context) (screening_services.Operator, error) {
		claims := mcpserver.ClaimsOf(ctx)
		if claims == nil {
			return screening_services.Operator{}, custom_errors.Unauthorized("未登录")
		}
		return screening_services.Operator{UserID: claims.UserID, IsAdmin: claims.Role.IsAdmin()}, nil
	}
}

// NewMCPRegistrars 收齐要挂上 MCP 端点的全部工具集。
//
// 参数表就是「MCP 暴露了哪些上下文」这个问题的答案，且由编译器守着：
// 新增一个上下文的工具集要改这里的签名，漏改则那组工具静默地不出现在
// tools/list 里——而那是一种没有任何报错的故障。
func NewMCPRegistrars(
	analysis *analysis_mcp.Tools,
	watchlist *watchlist_mcp.Tools,
	screening *screening_mcp.Tools,
) []mcpserver.ToolRegistrar {
	return []mcpserver.ToolRegistrar{analysis, watchlist, screening}
}

// MCPSet 装配 MCP 工具服务器。
//
// ===========================================================================
// 它为什么是一个独立的 set，而不是并进 HTTPSet
// ===========================================================================
//
// 因为它们暴露的是两套互不相同的东西：HTTPSet 装的是十三个 gin 处理器，
// MCPSet 装的是三个上下文的 MCP 工具集加一个传输层。
// 并在一起之后，「MCP 到底暴露了哪些上下文」这个问题就只能靠读 wire_gen 回答了。
//
// 三个领域服务分别来自 CoreSet、WatchlistSet、ScreeningSet，
// 因此本 set 只能和它们一起用。缺任何一个，Wire 会在生成期报
// 「找不到 provider」——这正是我们要的：依赖关系由编译期报错守住，
// 而不是靠注释提醒。
var MCPSet = wire.NewSet(
	NewAnalysisMCPOperator,
	NewWatchlistMCPOperator,
	NewScreeningMCPOperator,

	analysis_mcp.NewTools,
	watchlist_mcp.NewTools,
	screening_mcp.NewTools,

	NewMCPRegistrars,
	mcpserver.New,
)
