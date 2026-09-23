// Package mcp_tools 把自选股上下文暴露成 MCP 工具。
// 定位与 application/http_handlers 相同，理由见 analysis 上下文的同名包。
package mcp_tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	watchlist_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/watchlist/domain_services"
)

// OperatorResolver 从 context 里取出调用者身份，由组装根注入实现。
type OperatorResolver func(ctx context.Context) (watchlist_services.Operator, error)

// Tools 是自选股上下文的 MCP 工具集。
type Tools struct {
	svc     *watchlist_services.WatchlistService
	resolve OperatorResolver
}

func NewTools(svc *watchlist_services.WatchlistService, resolve OperatorResolver) *Tools {
	return &Tools{svc: svc, resolve: resolve}
}

// ===========================================================================
// 输出为什么是投影而不是实体本身
// ===========================================================================
//
// 直接把 *entities.WatchlistGroup 交给 SDK 序列化有两个问题：
// 一是实体带着 EventRecorder 这类内部状态，它们对模型毫无意义却要占 token；
// 二是输出 schema 是给模型读的说明书，字段越多它越容易挑错字段去做下一步推理。
// 这与 REST 层为每个接口写 view 结构是同一个判断，不是为 MCP 新造领域模型。

type watchlistGroupOut struct {
	ID    uint64 `json:"id" jsonschema:"分组 ID"`
	Name  string `json:"name" jsonschema:"分组名称"`
	Count int    `json:"count" jsonschema:"分组内自选股数量"`
}

type listWatchlistGroupsInput struct{}

type listWatchlistGroupsOutput struct {
	Groups []watchlistGroupOut `json:"groups" jsonschema:"当前用户的全部自选股分组"`
}

type getWatchlistGroupInput struct {
	GroupID uint64 `json:"groupId" jsonschema:"分组 ID，来自 list_watchlist_groups 的返回"`
}

type watchlistItemOut struct {
	Symbol string `json:"symbol" jsonschema:"股票代码，如 600519.SH"`
	Note   string `json:"note,omitempty" jsonschema:"用户备注"`
	// 行情可能取不到（停牌、数据未同步），此时这三项缺席而不是填 0——
	// 填 0 会让模型把「没数据」读成「价格为零」。
	Price     string `json:"price,omitempty" jsonschema:"最新价"`
	ChangePct string `json:"changePct,omitempty" jsonschema:"当日涨跌幅百分比"`
	TradeDate string `json:"tradeDate,omitempty" jsonschema:"行情所属交易日"`
}

type getWatchlistGroupOutput struct {
	Group watchlistGroupOut  `json:"group"`
	Items []watchlistItemOut `json:"items" jsonschema:"分组内的自选股及其最新行情"`
}

// RegisterTools 把本上下文的工具注册到 MCP 服务器上。
// 签名是 mcpserver 那边声明的窄接口的形状，本包不 import 那个接口。
func (t *Tools) RegisterTools(srv *mcp.Server) {
	svc := t.svc

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_watchlist_groups",
		Description: "列出当前用户的全部自选股分组。" +
			"要查看某个分组里具体有哪些股票，用返回的 id 调用 get_watchlist_group。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listWatchlistGroupsInput,
	) (*mcp.CallToolResult, listWatchlistGroupsOutput, error) {
		op, err := t.resolve(ctx)
		if err != nil {
			return nil, listWatchlistGroupsOutput{}, err
		}
		groups, err := svc.ListGroups(ctx, op)
		if err != nil {
			return nil, listWatchlistGroupsOutput{}, err
		}

		out := listWatchlistGroupsOutput{Groups: make([]watchlistGroupOut, 0, len(groups))}
		for _, g := range groups {
			out.Groups = append(out.Groups, watchlistGroupOut{
				ID:    g.ID,
				Name:  g.Name.String(),
				Count: len(g.Items),
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_watchlist_group",
		Description: "查看一个自选股分组里的股票清单及其最新行情。" +
			"只能查看属于当前用户的分组。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getWatchlistGroupInput,
	) (*mcp.CallToolResult, getWatchlistGroupOutput, error) {
		op, err := t.resolve(ctx)
		if err != nil {
			return nil, getWatchlistGroupOutput{}, err
		}
		// 归属校验在领域服务里（loadOwned），这里不重复判定：
		// 多写一遍等于让「谁能看哪个分组」这条规则有两个定义处。
		detail, err := svc.ListItems(ctx, op, in.GroupID)
		if err != nil {
			return nil, getWatchlistGroupOutput{}, err
		}

		out := getWatchlistGroupOutput{
			Group: watchlistGroupOut{
				ID:    detail.Group.ID,
				Name:  detail.Group.Name.String(),
				Count: len(detail.Rows),
			},
			Items: make([]watchlistItemOut, 0, len(detail.Rows)),
		}
		for _, row := range detail.Rows {
			item := watchlistItemOut{
				Symbol: row.Item.Code.FullSymbol(),
				Note:   row.Item.Note.String(),
			}
			if row.Quote != nil {
				item.Price = row.Quote.Price.String()
				item.ChangePct = row.Quote.ChangePct.String()
				item.TradeDate = row.Quote.TradeDate.String()
			}
			out.Items = append(out.Items, item)
		}
		return nil, out, nil
	})
}
