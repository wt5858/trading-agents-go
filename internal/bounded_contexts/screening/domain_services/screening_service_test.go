// 本测试放在外部测试包 domain_services_test：它要站在 handler 的位置上看本层。
package domain_services_test

import (
	"context"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// stubScreener 是 StockScreener 端口的桩。
//
// 它能被写出来这件事本身就说明了端口的形状是对的：实现一个筛选器只需要
// 「收一个 ScreenQuery、回一个结果集」，不需要任何「把股票池给我」的能力。
type stubScreener struct {
	got    value_objects.ScreenQuery
	called int
}

func (s *stubScreener) Screen(_ context.Context, q value_objects.ScreenQuery) (value_objects.ScreeningResultSet, error) {
	s.called++
	s.got = q
	return value_objects.EmptyResultSet(), nil
}

// newService 造一个只用于执行路径的服务。
//
// 模板仓储传 nil 是安全的：Execute 这条路径完全不碰模板（临时筛选不落库），
// 而传 nil 恰好能证明这一点——它一旦碰了就会 panic。
func newService(screener domain_services.StockScreener) *domain_services.ScreeningService {
	return domain_services.NewScreeningService(nil, screener, nil)
}

const loggedIn = uint64(7)

// TestExecute_PushesSortAndLimitIntoQuery 是「把过滤下推到数据库」这条规则
// 在本层的可测断言。
//
// 本层唯一该做的事是构造一个合法的 ScreenQuery 然后交出去：条件、排序、条数
// 一并进入查询，由实现翻译成 WHERE / ORDER BY / LIMIT。
// 如果排序或条数被留在本层用内存处理，就意味着实现必须先把全部命中行拉回来——
// 那正是「把 5000 只票加载进内存」的另一种写法。
func TestExecute_PushesSortAndLimitIntoQuery(t *testing.T) {
	screener := &stubScreener{}
	svc := newService(screener)

	_, err := svc.Execute(context.Background(), domain_services.Operator{UserID: loggedIn},
		domain_services.ScreenInput{
			Criteria: []domain_services.CriterionInput{
				{Field: value_objects.FieldPE, Operator: "lt", Values: []string{"20"}},
				{Field: value_objects.FieldIndustry, Operator: "in", Values: []string{"银行", "保险"}},
			},
			SortField:     value_objects.FieldTotalMV,
			SortDirection: "desc",
			Limit:         30,
		})
	if err != nil {
		t.Fatalf("执行筛选失败: %v", err)
	}
	if screener.called != 1 {
		t.Fatalf("筛选器应当被调用恰好一次，实际 %d 次", screener.called)
	}

	q := screener.got
	if n := len(q.Criteria()); n != 2 {
		t.Fatalf("两条条件都应当下推，实际 %d 条", n)
	}
	if q.Limit() != 30 {
		t.Fatalf("条数应当下推，实际 %d", q.Limit())
	}
	if q.Sort().Field().String() != value_objects.FieldTotalMV || !q.Sort().Descending() {
		t.Fatalf("排序应当下推，实际 %s %s", q.Sort().Field().String(), q.Sort().Direction())
	}

	// 条件按存储分组，这是跨存储查询规划的输入：pe 在行情、industry 在主数据。
	bySource := q.CriteriaBySource()
	if len(bySource[value_objects.FieldSourceQuote]) != 1 {
		t.Fatalf("pe 应当归到行情存储，实际分组: %v", bySource)
	}
	if len(bySource[value_objects.FieldSourceStock]) != 1 {
		t.Fatalf("industry 应当归到主数据存储，实际分组: %v", bySource)
	}
}

// TestExecute_RejectsBadInputBeforeTouchingScreener 确认非法入参不会打到存储。
//
// 字段白名单、比较符相容性、元数——全部在构造 ScreenQuery 的过程中判完。
// 一个带着非法字段的请求绝不该变成一次数据库往返，更不该有机会接近查询构造器。
func TestExecute_RejectsBadInputBeforeTouchingScreener(t *testing.T) {
	cases := []struct {
		name  string
		input domain_services.ScreenInput
	}{
		{"未知字段", domain_services.ScreenInput{Criteria: []domain_services.CriterionInput{
			{Field: "pe; DROP TABLE stocks--", Operator: "lt", Values: []string{"20"}},
		}}},
		{"类别字段用序关系比较符", domain_services.ScreenInput{Criteria: []domain_services.CriterionInput{
			{Field: value_objects.FieldIndustry, Operator: "between", Values: []string{"银行", "白酒"}},
		}}},
		{"区间只给一个值", domain_services.ScreenInput{Criteria: []domain_services.CriterionInput{
			{Field: value_objects.FieldPE, Operator: "between", Values: []string{"10"}},
		}}},
		{"零条件", domain_services.ScreenInput{}},
		{"重复条件", domain_services.ScreenInput{Criteria: []domain_services.CriterionInput{
			{Field: value_objects.FieldPE, Operator: "lt", Values: []string{"20"}},
			{Field: value_objects.FieldPE, Operator: "lt", Values: []string{"15"}},
		}}},
		{"非法排序字段", domain_services.ScreenInput{
			Criteria:  []domain_services.CriterionInput{{Field: value_objects.FieldPE, Operator: "lt", Values: []string{"20"}}},
			SortField: "(SELECT 1)",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screener := &stubScreener{}
			svc := newService(screener)
			_, err := svc.Execute(context.Background(), domain_services.Operator{UserID: loggedIn}, tc.input)
			if err == nil {
				t.Fatal("非法入参必须被拒绝")
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInvalidArgument, code)
			}
			if screener.called != 0 {
				t.Fatal("非法入参绝不该打到存储层")
			}
		})
	}
}

// TestExecute_RequiresLogin 确认未登录调用在本层就被拦下。
func TestExecute_RequiresLogin(t *testing.T) {
	screener := &stubScreener{}
	svc := newService(screener)
	_, err := svc.Execute(context.Background(), domain_services.Operator{}, domain_services.ScreenInput{
		Criteria: []domain_services.CriterionInput{{Field: value_objects.FieldPE, Operator: "lt", Values: []string{"20"}}},
	})
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeUnauthorized {
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeUnauthorized, code)
	}
	if screener.called != 0 {
		t.Fatal("未登录的请求不该打到存储层")
	}
}

// TestAvailableFields_NeedsNoStorage 确认字段字典不碰任何存储。
//
// 服务是用 nil 仓储造出来的，这个调用能返回就说明它没有去查库——
// 字段白名单是编译期常量，为它查一次库既没有数据来源，
// 也会让「加一个可筛选字段」变成一次数据迁移。
func TestAvailableFields_NeedsNoStorage(t *testing.T) {
	svc := newService(nil)
	specs := svc.AvailableFields()
	if len(specs) == 0 {
		t.Fatal("可筛选字段列表不应为空")
	}
	for _, spec := range specs {
		if _, err := value_objects.NewFieldName(spec.Name.String()); err != nil {
			t.Fatalf("字段字典里的 %q 无法通过白名单校验，前端照它填完表单会被拒绝", spec.Name.String())
		}
	}
}
