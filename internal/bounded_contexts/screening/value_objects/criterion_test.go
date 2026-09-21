// 本测试刻意放在外部测试包 value_objects_test 而不是 value_objects。
//
// 这不是风格选择，而是测试内容的一部分：外部包看到的东西和
// entities / domain_services / repositories 看到的完全一样。
// 写在包内的话，测试能直接写出 FieldName{v: "任意串"}，
// 于是「封闭枚举挡得住伪造」这条性质在测试里根本无从验证——
// 测试会站在一个真实调用方永远到不了的位置上。
package value_objects_test

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ---------------------------------------------------------------------------
// 一、字段白名单：本上下文的注入防线
// ---------------------------------------------------------------------------

// TestNewFieldName_RejectsUnknownField 守住注入边界。
//
// 这条测试保护的不是「用户会看到一句友好的错误提示」，而是
// **不存在任何一条从 HTTP 请求体到 SQL 标识符的路径**。
// 字段名会被拼进 ORDER BY / WHERE 的列名位置，而列名在 SQL 里不能参数化，
// 占位符帮不上忙；唯一的防线就是这里的白名单。
//
// 用例里混入了真实的注入载荷形态（分号、注释符、UNION、反引号），
// 它们全都必须在构造点被拒绝，而不是「后面某一层会处理」。
func TestNewFieldName_RejectsUnknownField(t *testing.T) {
	hostile := []string{
		"",
		"   ",
		"unknown_field",
		"pe; DROP TABLE stocks--",
		"pe` , (SELECT password_hash FROM users) `",
		"1=1",
		"pe UNION SELECT 1",
		"total_mv)--",
		"$where",
		"../../etc/passwd",
	}
	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			got, err := value_objects.NewFieldName(raw)
			if err == nil {
				t.Fatalf("字段 %q 必须被拒绝，实际构造出了 %q", raw, got.String())
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInvalidArgument, code)
			}
			// 被拒绝之后拿到的是零值，而零值给不出任何列名——
			// 这是防线的第二道：即便某条路径漏用了 error，也拼不出语句。
			if got.Column() != "" {
				t.Fatalf("被拒绝的字段不该有物理列名，实际 %q", got.Column())
			}
		})
	}
}

// TestNewFieldName_AcceptsWhitelisted 确认白名单里的字段能正常通过，
// 并且它给出的物理列名来自注册表而不是原样回显用户输入。
func TestNewFieldName_AcceptsWhitelisted(t *testing.T) {
	// 大小写与空白要被归一：前端传 " PE " 是常事，把它当成非法字段
	// 会让用户对着一个看起来完全正确的字段名发懵。
	f, err := value_objects.NewFieldName("  PE  ")
	if err != nil {
		t.Fatalf("白名单字段应当通过: %v", err)
	}
	if f.String() != value_objects.FieldPE {
		t.Fatalf("字段名应归一为 %q，实际 %q", value_objects.FieldPE, f.String())
	}
	if f.Column() != "pe" {
		t.Fatalf("物理列名应来自注册表，实际 %q", f.Column())
	}
	if !f.IsNumeric() {
		t.Fatal("pe 应当是数值字段")
	}
	if f.Source() != value_objects.FieldSourceQuote {
		t.Fatalf("pe 应当来自行情集合，实际 %s", f.Source())
	}
}

// TestRehydrateFieldName_StillChecksWhitelist 守住「重建路径也不放行」。
//
// 本项目其它 Rehydrate 都刻意跳过校验（落库的行是既成事实），这里是唯一的例外。
// 理由是这个值会变成查询语句的一部分：一旦库里因为迁移脚本、误操作或更早的
// 漏洞存进了白名单之外的串，无校验的重建等于把它直接送进 SQL。
// 「数据库里的数据可信」这个前提在安全边界上不成立。
func TestRehydrateFieldName_StillChecksWhitelist(t *testing.T) {
	if got := value_objects.RehydrateFieldName("pe"); got.String() != "pe" {
		t.Fatalf("白名单字段应当重建成功，实际 %q", got.String())
	}
	poisoned := value_objects.RehydrateFieldName("pe; DROP TABLE stocks--")
	if !poisoned.IsZero() {
		t.Fatal("库里的非白名单字段必须重建成零值，否则它会被拼进查询语句")
	}
	if poisoned.Column() != "" {
		t.Fatalf("零值字段不该有物理列名，实际 %q", poisoned.Column())
	}
}

// TestAllFieldSpecs_IsSelfConsistent 确认注册表本身没有内部矛盾：
// 每条 spec 的 Name 必须能被 NewFieldName 接受，且必须有物理列名。
//
// 这条测试防的是「前端拿到一个字段，填完表单提交回来却被拒绝」——
// 字段字典接口的数据源就是 AllFieldSpecs，它与白名单必须是同一份东西。
func TestAllFieldSpecs_IsSelfConsistent(t *testing.T) {
	specs := value_objects.AllFieldSpecs()
	if len(specs) == 0 {
		t.Fatal("可筛选字段列表不应为空")
	}
	for _, spec := range specs {
		name := spec.Name.String()
		if _, err := value_objects.NewFieldName(name); err != nil {
			t.Fatalf("字段字典里的 %q 无法通过白名单校验: %v", name, err)
		}
		if spec.Column == "" {
			t.Fatalf("字段 %q 缺少物理列名", name)
		}
		if spec.Label == "" {
			t.Fatalf("字段 %q 缺少中文标签，前端表单会渲染出一个空标题", name)
		}
		if spec.Kind != value_objects.FieldKindNumeric && spec.Kind != value_objects.FieldKindCategorical {
			t.Fatalf("字段 %q 的类型 %q 不在已知取值内", name, spec.Kind)
		}
		// 每个字段至少要有一个可用的比较符，否则它在表单里是个死选项。
		if len(value_objects.OperatorsFor(spec.Kind)) == 0 {
			t.Fatalf("字段 %q 没有任何可用比较符", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 二、比较符 ↔ 字段类型相容性
// ---------------------------------------------------------------------------

// TestCriterion_RejectsOrderingOperatorOnCategoricalField 守住
// 「序关系比较符只能用于有全序的数值字段」。
//
// 没有这条规则，「行业介于『银行』和『白酒』之间」会是一条语法合法的条件，
// 数据库照着中文串的字典序安静地返回一批莫名其妙的结果——
// 那个顺序和业务毫无关系，而用户拿不到任何提示。
func TestCriterion_RejectsOrderingOperatorOnCategoricalField(t *testing.T) {
	ordering := []struct {
		op     string
		values []string
	}{
		{"gt", []string{"银行"}},
		{"gte", []string{"银行"}},
		{"lt", []string{"银行"}},
		{"lte", []string{"银行"}},
		{"between", []string{"银行", "白酒"}},
	}
	for _, tc := range ordering {
		t.Run("industry_"+tc.op, func(t *testing.T) {
			_, err := value_objects.NewCriterion(value_objects.FieldIndustry, tc.op, tc.values)
			if err == nil {
				t.Fatalf("类别字段上的序关系比较符「%s」必须被拒绝", tc.op)
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInvalidArgument, code)
			}
			// 提示里要带上字段名：用户面对一个十条件的模板，
			// 光说「between 不能用于类别字段」他还是不知道该改哪一条。
			if !strings.Contains(custom_errors.MessageOf(err), "行业") {
				t.Fatalf("错误提示应指明是哪个字段，实际: %s", custom_errors.MessageOf(err))
			}
		})
	}
}

// TestCriterion_AllowsSetOperatorsOnCategoricalField 确认相等与集合运算
// 对类别字段是放行的：「行业属于（银行, 保险）」是最常见的选股条件之一。
func TestCriterion_AllowsSetOperatorsOnCategoricalField(t *testing.T) {
	for _, op := range []string{"eq", "ne", "in", "not_in"} {
		t.Run("industry_"+op, func(t *testing.T) {
			if _, err := value_objects.NewCriterion(value_objects.FieldIndustry, op, []string{"银行", "保险"}); err != nil {
				if op == "eq" || op == "ne" {
					// eq / ne 只接受一个值，这里传了两个，元数校验拒绝是对的。
					if !strings.Contains(custom_errors.MessageOf(err), "只接受 1 个值") {
						t.Fatalf("意外的错误: %v", err)
					}
					return
				}
				t.Fatalf("类别字段上的「%s」应当放行: %v", op, err)
			}
		})
	}
}

// TestCriterion_AllowsOrderingOnNumericField 是上面那条规则的正面用例。
func TestCriterion_AllowsOrderingOnNumericField(t *testing.T) {
	c, err := value_objects.NewCriterion(value_objects.FieldPE, "between", []string{"10", "20"})
	if err != nil {
		t.Fatalf("数值字段上的区间条件应当通过: %v", err)
	}
	nums := c.Numbers()
	if len(nums) != 2 ||
		!nums[0].Equal(decimal.NewFromInt(10)) ||
		!nums[1].Equal(decimal.NewFromInt(20)) {
		t.Fatalf("数值应当被预先解析成 [10 20]，实际 %v", nums)
	}
	// 数值字段的绑定参数必须是 decimal 而不是字符串或 float64：
	// 绑成字符串会让 MySQL 做隐式转换从而放弃索引；
	// 绑成 float64 则在 Mongo 侧以 BSON double 参与比较，而行情列是 Decimal128，
	// 边界值（查询写 0.1，double 实际是 0.100000000000000005...）会被判为不满足
	// 从而漏掉本该命中的记录——两种错法都不报错，只是安静地少给几只票。
	args := c.Args()
	if len(args) != 2 {
		t.Fatalf("绑定参数应有 2 个，实际 %d 个", len(args))
	}
	if _, ok := args[0].(decimal.Decimal); !ok {
		t.Fatalf("数值字段的绑定参数应为 decimal.Decimal，实际 %T", args[0])
	}
}

// TestCriterion_PreservesBoundaryPrecision 钉死小数边界值不被浮点污染。
//
// 0.1 是 float64 表示不了的经典值：ParseFloat("0.1") 得到的是
// 0.1000000000000000055511151231257827。以它作为 $gte 下界去比 Decimal128 的 0.1，
// 库里那条记录会被判为小于下界而漏掉。decimal 解析则逐位精确。
func TestCriterion_PreservesBoundaryPrecision(t *testing.T) {
	c, err := value_objects.NewCriterion(value_objects.FieldPE, "gte", []string{"0.1"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	got := c.Numbers()[0]
	if !got.Equal(decimal.RequireFromString("0.1")) {
		t.Fatalf("0.1 解析成 %v，期望精确的 0.1", got)
	}
	if s := got.String(); s != "0.1" {
		t.Fatalf("0.1 的字符串形态 = %q，期望 \"0.1\"", s)
	}
}

// ---------------------------------------------------------------------------
// 三、比较符元数（值的个数）
// ---------------------------------------------------------------------------

// TestCriterion_ValidatesArity 守住「值的个数必须与比较符相符」。
//
// 元数不校验的后果不是一句难看的报错，而是查询构造器要么切片越界 panic，
// 要么把一个残缺的 between 悄悄降级成 >=——用户得到一份语义被改写过的结果。
func TestCriterion_ValidatesArity(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		op      string
		values  []string
		wantErr bool
	}{
		{"between 一个值不行", value_objects.FieldPE, "between", []string{"10"}, true},
		{"between 三个值不行", value_objects.FieldPE, "between", []string{"10", "20", "30"}, true},
		{"between 两个值可以", value_objects.FieldPE, "between", []string{"10", "20"}, false},
		{"between 零个值不行", value_objects.FieldPE, "between", nil, true},

		{"gt 两个值不行", value_objects.FieldPE, "gt", []string{"10", "20"}, true},
		{"gt 零个值不行", value_objects.FieldPE, "gt", nil, true},
		{"gt 一个值可以", value_objects.FieldPE, "gt", []string{"10"}, false},

		{"in 零个值不行", value_objects.FieldIndustry, "in", nil, true},
		{"in 一个值可以", value_objects.FieldIndustry, "in", []string{"银行"}, false},
		{"in 多个值可以", value_objects.FieldIndustry, "in", []string{"银行", "保险", "证券"}, false},

		// 空白项在计数之前被剔除：前端的多值输入框常常尾随一个空项，
		// 把它算进个数会让一个残缺的 between 蒙混过关。
		{"between 一值加一个空串不行", value_objects.FieldPE, "between", []string{"10", "  "}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := value_objects.NewCriterion(tc.field, tc.op, tc.values)
			if tc.wantErr && err == nil {
				t.Fatal("该条件必须被拒绝")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("该条件应当通过: %v", err)
			}
		})
	}
}

// TestCriterion_RejectsNonNumericValueOnNumericField 确认数值字段上的
// 非数值取值在构造点就被拒绝，而不是等打到数据库那一刻。
func TestCriterion_RejectsNonNumericValueOnNumericField(t *testing.T) {
	if _, err := value_objects.NewCriterion(value_objects.FieldPE, "gt", []string{"abc"}); err == nil {
		t.Fatal("数值字段上的非数值取值必须被拒绝")
	}
}

// TestCriterion_RejectsInvertedRange 确认区间的上下限写反会被拒绝。
//
// 不拒绝的话 BETWEEN 20 AND 10 在 SQL 里恒为假，用户拿到一份空清单
// 却没有任何解释。这里选择报错而不是静默交换两端：交换等于替用户猜意图，
// 而「写反了」多半意味着他把两个输入框填错了位置。
func TestCriterion_RejectsInvertedRange(t *testing.T) {
	if _, err := value_objects.NewCriterion(value_objects.FieldPE, "between", []string{"20", "10"}); err == nil {
		t.Fatal("区间下限大于上限必须被拒绝")
	}
}

// TestCriterion_RejectsUnknownOperator 守住比较符枚举。
func TestCriterion_RejectsUnknownOperator(t *testing.T) {
	for _, op := range []string{"", "like", "regex", "$where", "gt OR 1=1"} {
		if _, err := value_objects.NewCriterion(value_objects.FieldPE, op, []string{"10"}); err == nil {
			t.Fatalf("比较符 %q 必须被拒绝", op)
		}
	}
}

// TestCriterion_KeyIgnoresValues 确认判重键只由 (字段, 比较符) 组成。
//
// 这条性质是「同一模板内不重复」那条不变式的依据：同一字段上
// 「pe > 10」与「pe > 20」是同一条规则的两个版本（并存时后者恒覆盖前者），
// 而「pe > 10」与「pe < 30」是一个合法的区间表达，必须允许并存。
func TestCriterion_KeyIgnoresValues(t *testing.T) {
	a := mustCriterion(t, value_objects.FieldPE, "gt", "10")
	b := mustCriterion(t, value_objects.FieldPE, "gt", "20")
	c := mustCriterion(t, value_objects.FieldPE, "lt", "30")

	if a.Key() != b.Key() {
		t.Fatalf("同字段同比较符的两条条件应当同键: %q vs %q", a.Key(), b.Key())
	}
	if a.Key() == c.Key() {
		t.Fatal("同字段不同比较符的两条条件不该同键，否则区间条件无法表达")
	}
	if a.Equal(b) {
		t.Fatal("取值不同的两条条件不应判为相等，否则仓储的 diff 会漏掉一次更新")
	}
}

// ---------------------------------------------------------------------------
// 四、筛选入参整体
// ---------------------------------------------------------------------------

// TestNewScreenQuery_RejectsEmptyAndDuplicate 守住临时筛选这条路径上的不变式。
//
// 临时筛选不经过聚合根，所以判重必须在这里也有一份——否则
// 「保存成模板会被拒绝、直接执行却能跑」会成为一个用户无法理解的差异。
func TestNewScreenQuery_RejectsEmptyAndDuplicate(t *testing.T) {
	if _, err := value_objects.NewScreenQuery(nil, value_objects.SortSpec{}, 0); err == nil {
		t.Fatal("零条件的筛选必须被拒绝：它等价于返回全市场")
	}

	dup := []value_objects.Criterion{
		mustCriterion(t, value_objects.FieldPE, "gt", "10"),
		mustCriterion(t, value_objects.FieldPE, "gt", "20"),
	}
	if _, err := value_objects.NewScreenQuery(dup, value_objects.SortSpec{}, 0); err == nil {
		t.Fatal("重复的 (字段, 比较符) 必须被拒绝")
	}

	// 零值条件只可能来自一条坏的持久化记录。它必须让整次执行失败，
	// 而不是被静默跳过——少一个过滤器会让结果凭空变大且毫无提示。
	broken := []value_objects.Criterion{value_objects.RehydrateCriterion("no_such_field", "gt", []string{"1"})}
	if _, err := value_objects.NewScreenQuery(broken, value_objects.SortSpec{}, 0); err == nil {
		t.Fatal("包含无法识别字段的条件必须让执行失败，不能被跳过")
	}
}

// TestNewScreenQuery_ClampsLimitAndDefaultsSort 确认边界收敛与默认排序。
func TestNewScreenQuery_ClampsLimitAndDefaultsSort(t *testing.T) {
	criteria := []value_objects.Criterion{mustCriterion(t, value_objects.FieldPE, "lt", "20")}

	q, err := value_objects.NewScreenQuery(criteria, value_objects.SortSpec{}, 0)
	if err != nil {
		t.Fatalf("构造筛选入参失败: %v", err)
	}
	if q.Limit() != value_objects.DefaultResultLimit {
		t.Fatalf("未指定条数应回落到 %d，实际 %d", value_objects.DefaultResultLimit, q.Limit())
	}
	if q.Sort().IsZero() {
		t.Fatal("未指定排序应回落到默认排序，而不是留空")
	}

	q, err = value_objects.NewScreenQuery(criteria, value_objects.SortSpec{}, 100000)
	if err != nil {
		t.Fatalf("构造筛选入参失败: %v", err)
	}
	if q.Limit() != value_objects.MaxResultLimit {
		t.Fatalf("超限条数应被收敛到 %d，实际 %d", value_objects.MaxResultLimit, q.Limit())
	}
}

// TestScreenQuery_OutputFieldsIncludesSortField 确认排序字段也会被带回结果。
//
// 「PE 小于 20 的票按市值排」这条查询里，市值不在筛选条件里，
// 但它必须出现在结果表格里——否则用户看到一份排了序却看不见排序依据的清单。
func TestScreenQuery_OutputFieldsIncludesSortField(t *testing.T) {
	sort, err := value_objects.NewSortSpec(value_objects.FieldTotalMV, "desc")
	if err != nil {
		t.Fatalf("构造排序失败: %v", err)
	}
	q, err := value_objects.NewScreenQuery(
		[]value_objects.Criterion{mustCriterion(t, value_objects.FieldPE, "lt", "20")}, sort, 10)
	if err != nil {
		t.Fatalf("构造筛选入参失败: %v", err)
	}
	fields := q.OutputFields()
	if len(fields) != 2 {
		t.Fatalf("输出字段应为 [pe total_mv]，实际 %v", fields)
	}
	// 顺序即前端表格的列序：筛选条件在前，排序字段补在后。
	if fields[0].String() != value_objects.FieldPE || fields[1].String() != value_objects.FieldTotalMV {
		t.Fatalf("输出字段顺序不对: %v", fields)
	}
}

// TestNewSortSpec_RejectsUnknownField 确认排序字段走的是同一份白名单。
//
// 排序字段同样会被拼进 ORDER BY 的列名位置，注入面与筛选字段完全相同。
// 复用同一个封闭枚举意味着不会出现「筛选那边补了字段、排序这边忘了补」的不一致。
func TestNewSortSpec_RejectsUnknownField(t *testing.T) {
	if _, err := value_objects.NewSortSpec("pe; DROP TABLE stocks--", "desc"); err == nil {
		t.Fatal("非白名单的排序字段必须被拒绝")
	}
	if _, err := value_objects.NewSortSpec(value_objects.FieldPE, "desc; --"); err == nil {
		t.Fatal("非法排序方向必须被拒绝")
	}
	// 空字段名是合法输入，语义是「不指定排序」，由执行层回落到默认。
	s, err := value_objects.NewSortSpec("", "")
	if err != nil {
		t.Fatalf("空排序字段应当被接受为「不指定」: %v", err)
	}
	if !s.IsZero() {
		t.Fatal("空排序字段应当得到零值 SortSpec")
	}
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func mustCriterion(t *testing.T, field, op string, values ...string) value_objects.Criterion {
	t.Helper()
	c, err := value_objects.NewCriterion(field, op, values)
	if err != nil {
		t.Fatalf("构造筛选条件失败 (%s %s %v): %v", field, op, values, err)
	}
	return c
}
