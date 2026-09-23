// Package mcp_tools 把选股上下文暴露成 MCP 工具。
// 定位与 application/http_handlers 相同，理由见 analysis 上下文的同名包。
package mcp_tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	screening_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	screening_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
)

// OperatorResolver 从 context 里取出调用者身份，由组装根注入实现。
type OperatorResolver func(ctx context.Context) (screening_services.Operator, error)

// Tools 是选股上下文的 MCP 工具集。
type Tools struct {
	svc     *screening_services.ScreeningService
	resolve OperatorResolver
}

func NewTools(svc *screening_services.ScreeningService, resolve OperatorResolver) *Tools {
	return &Tools{svc: svc, resolve: resolve}
}

type screenCriterionInput struct {
	Field    string `json:"field" jsonschema:"筛选字段名，用 list_screening_fields 查可用字段"`
	Operator string `json:"operator" jsonschema:"比较运算符，如 gt/gte/lt/lte/eq/between/in"`
	// Values 是一个列表而不是 value/value2 两个字段，与领域层的 CriterionInput 同形：
	// between 要两个值、in 要任意多个，拆成固定的两个字段表达不了 in，
	// 而多出来的那个「第三个值放哪」的问题会一路传染到前端。
	Values []string `json:"values" jsonschema:"比较值列表；eq/gt 等用一个值，between 用两个，in 用多个"`
}

type runScreeningInput struct {
	Criteria []screenCriterionInput `json:"criteria" jsonschema:"筛选条件列表，至少一条"`
	// 排序字段不在 criteria 里时也会被带进结果的 fields，
	// 这条规则在领域层（ScreeningResult.Fields 的注释），这里只如实转述。
	SortField     string `json:"sortField,omitempty" jsonschema:"排序字段名"`
	SortDirection string `json:"sortDirection,omitempty" jsonschema:"排序方向 asc/desc"`
	Limit         int    `json:"limit,omitempty" jsonschema:"返回条数上限"`
}

type screenFieldOut struct {
	Field string `json:"field"`
	// Value 在该字段对这只票缺失时留空而不是填 0。
	// 「市盈率没有数据」（亏损股）和「市盈率是 0」是两回事，
	// 而模型看到 0 只会把它当成一个极便宜的估值。
	Value string `json:"value,omitempty"`
}

// screenFieldOf 把领域的 FieldValue 投影成给模型看的一行。
//
// FieldValue 同时带 Number 与 Text 两个槽，由 Present 表示这只票到底有没有值。
// 取哪一个看构造器用的是 NumberValue 还是 TextValue，这里按 Text 非空来区分——
// 数值字段的 Text 恒为空，文本字段的 Number 恒为零值。
func screenFieldOf(f screening_vo.FieldValue) screenFieldOut {
	out := screenFieldOut{Field: f.Field.String()}
	if !f.Present {
		return out
	}
	if f.Text != "" {
		out.Value = f.Text
		return out
	}
	out.Value = f.Number.String()
	return out
}

type screenResultOut struct {
	Symbol string           `json:"symbol" jsonschema:"股票代码"`
	Name   string           `json:"name" jsonschema:"股票名称"`
	Fields []screenFieldOut `json:"fields" jsonschema:"命中的字段取值"`
}

type runScreeningOutput struct {
	Results []screenResultOut `json:"results"`
	Total   int64             `json:"total" jsonschema:"符合条件的总数"`
	AsOf    string            `json:"asOf,omitempty" jsonschema:"本次筛选依据的行情交易日"`
	// Truncated 必须原样透出：选股结果的价值完全建立在「符合条件的都在这里」
	// 这个承诺上，破坏了它却不说，比返回错误更糟（见 ScreeningResultSet 的注释）。
	Truncated bool   `json:"truncated" jsonschema:"结果是否因触到上限而被截断"`
	Hint      string `json:"hint,omitempty" jsonschema:"结果被截断时的提示"`
}

type listScreeningFieldsInput struct{}

type screeningFieldSpecOut struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

type listScreeningFieldsOutput struct {
	Fields []screeningFieldSpecOut `json:"fields" jsonschema:"可用于 run_screening 的全部筛选字段"`
}

// RegisterTools 把本上下文的工具注册到 MCP 服务器上。
// 签名是 mcpserver 那边声明的窄接口的形状，本包不 import 那个接口。
func (t *Tools) RegisterTools(srv *mcp.Server) {
	svc := t.svc

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_screening_fields",
		Description: "列出选股筛选可用的全部字段。" +
			"调用 run_screening 之前先用它确认字段名，不要猜。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listScreeningFieldsInput,
	) (*mcp.CallToolResult, listScreeningFieldsOutput, error) {
		// AvailableFields 是一张静态表，不需要身份。但仍然要求登录：
		// 一个不鉴权的工具会成为整个 MCP 端点上唯一的匿名入口，
		// 而「哪些字段可筛」本身就是产品能力的描述。
		if _, err := t.resolve(ctx); err != nil {
			return nil, listScreeningFieldsOutput{}, err
		}
		specs := svc.AvailableFields()
		out := listScreeningFieldsOutput{Fields: make([]screeningFieldSpecOut, 0, len(specs))}
		for _, f := range specs {
			out.Fields = append(out.Fields, screeningFieldSpecOut{
				Name:  f.Name.String(),
				Label: f.Label,
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "run_screening",
		Description: "按给定条件筛选股票。字段名必须来自 list_screening_fields。" +
			"结果是对某一个交易日横截面的查询，返回的 asOf 就是该交易日。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runScreeningInput,
	) (*mcp.CallToolResult, runScreeningOutput, error) {
		op, err := t.resolve(ctx)
		if err != nil {
			return nil, runScreeningOutput{}, err
		}

		criteria := make([]screening_services.CriterionInput, 0, len(in.Criteria))
		for _, c := range in.Criteria {
			criteria = append(criteria, screening_services.CriterionInput{
				Field:    c.Field,
				Operator: c.Operator,
				Values:   c.Values,
			})
		}
		// 条件的全部不变式（非空、不超上限、不重复、条数收敛）由值对象构造器判定，
		// 这里一条都不预判——预判会让 MCP 与 REST 对同一份非法输入给出不同的错误。
		set, err := svc.Execute(ctx, op, screening_services.ScreenInput{
			Criteria:      criteria,
			SortField:     in.SortField,
			SortDirection: in.SortDirection,
			Limit:         in.Limit,
		})
		if err != nil {
			return nil, runScreeningOutput{}, err
		}

		out := runScreeningOutput{
			Results:   make([]screenResultOut, 0, len(set.Results)),
			Total:     set.Total,
			Truncated: set.Truncated,
		}
		if !set.AsOf.IsZero() {
			out.AsOf = set.AsOf.String()
		}
		if set.Truncated {
			out.Hint = "候选集触到上限，这是符合条件股票的一个子集，不是完整清单。"
		}
		for _, r := range set.Results {
			row := screenResultOut{
				Symbol: r.Code.FullSymbol(),
				Name:   r.Name,
				Fields: make([]screenFieldOut, 0, len(r.Fields)),
			}
			for _, f := range r.Fields {
				row.Fields = append(row.Fields, screenFieldOf(f))
			}
			out.Results = append(out.Results, row)
		}
		return nil, out, nil
	})
}
