// 本测试刻意放在外部测试包 entities_test 而不是 entities。
//
// 这不是风格选择，而是测试内容的一部分：外部包看到的东西和
// domain_services / application / repositories 看到的完全一样。
// 如果测试写在包内，它就能调用 newCriterion、能调用 setSpec，
// 于是「子实体只能经由根产生」这条规则在测试里根本无从验证——
// 测试会站在一个真实调用方永远到不了的位置上。
package entities_test

import (
	"strconv"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

func newTestTemplate(t *testing.T) *entities.ScreeningTemplate {
	t.Helper()
	name, err := value_objects.NewTemplateName("低估值蓝筹")
	if err != nil {
		t.Fatalf("构造模板名失败: %v", err)
	}
	tpl, err := entities.NewScreeningTemplate(7, name, "PE 低、ROE 高的大盘股")
	if err != nil {
		t.Fatalf("创建选股模板失败: %v", err)
	}
	// 清掉创建事件，让后续断言只看本次操作抛出的事件。
	tpl.GetAllPendingEvents()
	return tpl
}

func criterion(t *testing.T, field, op string, values ...string) value_objects.Criterion {
	t.Helper()
	c, err := value_objects.NewCriterion(field, op, values)
	if err != nil {
		t.Fatalf("构造筛选条件失败 (%s %s %v): %v", field, op, values, err)
	}
	return c
}

// numericCriteria 造 n 条互不重复的数值条件。
//
// 靠「同一个字段配不同的阈值」是造不出多条的（它们同键，会被判重拦下），
// 所以这里轮换字段与比较符：字段清单里的数值字段有十几个，
// 每个字段又有多个序关系比较符，组合起来足够造出上限以外的条数。
func numericCriteria(t *testing.T, n int) []value_objects.Criterion {
	t.Helper()
	fields := []string{
		value_objects.FieldPE, value_objects.FieldPB, value_objects.FieldROE,
		value_objects.FieldNetMargin, value_objects.FieldGrossMargin,
		value_objects.FieldDebtRatio, value_objects.FieldEPS,
		value_objects.FieldTotalMV, value_objects.FieldCircMV,
		value_objects.FieldClose, value_objects.FieldOpen, value_objects.FieldHigh,
		value_objects.FieldLow, value_objects.FieldChangePct, value_objects.FieldTurnover,
		value_objects.FieldVolume, value_objects.FieldAmount,
	}
	ops := []string{"gt", "lt", "gte", "lte"}

	out := make([]value_objects.Criterion, 0, n)
	for i := 0; i < n; i++ {
		field := fields[i%len(fields)]
		op := ops[(i/len(fields))%len(ops)]
		out = append(out, criterion(t, field, op, strconv.Itoa(i+1)))
	}
	return out
}

func hasEvent(events []domain_event.DomainEvent, name string) bool {
	for _, e := range events {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// assertDense 断言 SortOrder 是 0..n-1 的稠密连续序列，且与切片下标一致。
//
// 同时校验两件事是有意义的：序号本身连续、以及序号与实际存放顺序一致。
// 只校验前者的话，一个「序号对但顺序反了」的实现照样通过，
// 而前端表格的列序来自切片顺序，不是来自那个数字。
func assertDense(t *testing.T, tpl *entities.ScreeningTemplate) {
	t.Helper()
	for i, c := range tpl.Criteria {
		if c.SortOrder != i {
			t.Fatalf("排序必须稠密连续：第 %d 条的 SortOrder 是 %d", i, c.SortOrder)
		}
	}
	if err := tpl.Validate(); err != nil {
		t.Fatalf("聚合自检应通过，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 不变式一：同一模板内 (字段, 比较符) 不得重复
// ---------------------------------------------------------------------------

// TestAddCriterion_RejectsDuplicateFieldOperator 守住判重。
//
// 没有这条规则，「pe > 10」和「pe > 20」会同时留在模板里，而它们在查询里
// 是 AND 关系——后者恒覆盖前者，前者是纯噪音。用户改了看得见的那一条，
// 却发现结果没变（另一条更严的还在），且界面上完全看不出问题在哪。
func TestAddCriterion_RejectsDuplicateFieldOperator(t *testing.T) {
	tpl := newTestTemplate(t)

	if _, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "gt", "10")); err != nil {
		t.Fatalf("首次添加应当成功: %v", err)
	}
	tpl.GetAllPendingEvents()

	_, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "gt", "20"))
	if err == nil {
		t.Fatal("同一 (字段, 比较符) 重复添加必须被拒绝")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeAlreadyExists {
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeAlreadyExists, code)
	}
	if tpl.CriterionCount() != 1 {
		t.Fatalf("被拒绝的添加不该改变条件数，实际 %d", tpl.CriterionCount())
	}
	// 被拒绝的操作不该抛出事件：下游会据此作废缓存的选股结果，
	// 凭空多一次失效通知意味着一次没有必要的重算。
	if hasEvent(tpl.GetAllPendingEvents(), domain_events.OnTemplateCriteriaChangedEventName) {
		t.Fatal("重复添加被拒绝时不该抛出条件变更事件")
	}
}

// TestAddCriterion_AllowsSameFieldDifferentOperator 是判重规则的另一半。
//
// 「pe > 10 且 pe < 30」是一个区间表达，两条条件字段相同、比较符不同，
// 必须允许并存。判重键若把字段单独当键，这个最常见的用法就废了。
func TestAddCriterion_AllowsSameFieldDifferentOperator(t *testing.T) {
	tpl := newTestTemplate(t)

	if _, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "gt", "10")); err != nil {
		t.Fatalf("添加下限条件失败: %v", err)
	}
	if _, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "lt", "30")); err != nil {
		t.Fatalf("同字段不同比较符应当允许并存: %v", err)
	}
	if tpl.CriterionCount() != 2 {
		t.Fatalf("条件数应为 2，实际 %d", tpl.CriterionCount())
	}
	assertDense(t, tpl)
}

// TestReplaceCriteria_RejectsDuplicate 确认整份替换这条路径上判重同样成立。
//
// 编辑模板的表单走的是 ReplaceCriteria，如果只有 AddCriterion 判重，
// 用户通过编辑表单就能把重复条件塞进模板——防线必须覆盖全部写入路径。
func TestReplaceCriteria_RejectsDuplicate(t *testing.T) {
	tpl := newTestTemplate(t)
	err := tpl.ReplaceCriteria([]value_objects.Criterion{
		criterion(t, value_objects.FieldROE, "gt", "15"),
		criterion(t, value_objects.FieldROE, "gt", "20"),
	})
	if err == nil {
		t.Fatal("整份替换里的重复条件必须被拒绝")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeAlreadyExists {
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeAlreadyExists, code)
	}
}

// TestValidate_RejectsDuplicateSmuggledIn 确认写库前的最后一道关也查判重。
//
// 这不是不信任 AddCriterion，而是因为 Criteria 切片本身是导出的：
// 包外能往里 append。一旦破损状态写进库，后续每一次加载都会把它当成既成事实。
func TestValidate_RejectsDuplicateSmuggledIn(t *testing.T) {
	tpl := newTestTemplate(t)
	if _, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "gt", "10")); err != nil {
		t.Fatalf("添加条件失败: %v", err)
	}
	// 把根自己造的那一条复制一份塞进去：它带着 attached 凭据，
	// 所以只能被判重规则拦下，而不是被「不是根造的」拦下。
	tpl.Criteria = append(tpl.Criteria, tpl.Criteria[0])
	if err := tpl.Validate(); err == nil {
		t.Fatal("聚合自检必须拦下重复条件")
	}
}

// ---------------------------------------------------------------------------
// 不变式二：条件只能经由聚合根产生
// ---------------------------------------------------------------------------

// TestCriterion_CannotBeCreatedOutsideRoot 是本上下文最重要的一条结构性测试。
//
// # 编译期的部分（无法写成断言，只能写成注释）
//
// 下面这些在包外都**编译不过**，这正是规则的第一道防线：
//
//	entities.newCriterion(...)         // 构造函数不导出
//	c.setSpec(spec, time.Now())        // 修改方法不导出
//	c.setSortOrder(3, time.Now())      // 同上
//	c.bindTemplate(1)                  // 同上
//
// 于是包外唯一能做的就是用字面量造一个 Criterion 再 append 进 Criteria 切片。
// 编译器拦不住这一步（字段是导出的），所以必须有运行期的第二道防线——
// 就是本测试验证的这一条：那样造出来的条件拿不到 attached 凭据
// （不导出的字段在包外既不能用字面量赋值，也不能赋值访问），Validate 能揪出它。
//
// 没有这道防线，一条绕过了全部不变式判定的条件会被仓储原样写进库，
// 从此每一次加载都把它当成合法数据。
func TestCriterion_CannotBeCreatedOutsideRoot(t *testing.T) {
	tpl := newTestTemplate(t)
	if _, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "lt", "20")); err != nil {
		t.Fatalf("经由根添加条件应当成功: %v", err)
	}
	if err := tpl.Validate(); err != nil {
		t.Fatalf("只经由根产生的条件应当通过自检: %v", err)
	}

	// 绕过根：包外自己 new 一个子实体塞进聚合。
	smuggled := &entities.Criterion{
		TemplateID: tpl.ID,
		Spec:       criterion(t, value_objects.FieldROE, "gt", "15"),
		SortOrder:  1,
	}
	tpl.Criteria = append(tpl.Criteria, smuggled)

	err := tpl.Validate()
	if err == nil {
		t.Fatal("绕过聚合根塞进来的条件必须被自检揪出")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeInternal {
		// 归为 Internal 而不是 Invalid：这不是用户输入的问题，
		// 是调用方代码写错了，用户那边什么都做不了。
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInternal, code)
	}
}

// TestCriteria_AreReadOnlyOutsideRoot 确认包外拿到子实体指针也只能读。
//
// 这条测试的主体同样是「编译不过的那些行」，这里能断言的是它的正面：
// 读访问器全部可用，而它们都不返回可写引用——Spec 是值对象，
// Values() 返回的是拷贝，包外改它不会影响模板里的那一份。
func TestCriteria_AreReadOnlyOutsideRoot(t *testing.T) {
	tpl := newTestTemplate(t)
	c, err := tpl.AddCriterion(criterion(t, value_objects.FieldIndustry, "in", "银行", "保险"))
	if err != nil {
		t.Fatalf("添加条件失败: %v", err)
	}

	values := c.Spec.Values()
	values[0] = "篡改"
	if got := c.Spec.Values()[0]; got != "银行" {
		t.Fatalf("值对象必须不可变，拷贝被改后原值变成了 %q", got)
	}
	if c.Field().String() != value_objects.FieldIndustry {
		t.Fatalf("字段访问器不对: %q", c.Field().String())
	}
	if c.Key() != value_objects.FieldIndustry+":in" {
		t.Fatalf("判重键不对: %q", c.Key())
	}
}

// ---------------------------------------------------------------------------
// 不变式三：条数上限与「至少一条」
// ---------------------------------------------------------------------------

// TestAddCriterion_EnforcesMaxCount 守住条数上限。
func TestAddCriterion_EnforcesMaxCount(t *testing.T) {
	tpl := newTestTemplate(t)
	if err := tpl.ReplaceCriteria(numericCriteria(t, entities.MaxCriteriaPerTemplate)); err != nil {
		t.Fatalf("填满到上限应当成功: %v", err)
	}
	tpl.GetAllPendingEvents()

	_, err := tpl.AddCriterion(criterion(t, value_objects.FieldIndustry, "eq", "银行"))
	if err == nil {
		t.Fatal("超过条数上限必须被拒绝")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeQuotaExceeded {
		t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeQuotaExceeded, code)
	}
	if tpl.CriterionCount() != entities.MaxCriteriaPerTemplate {
		t.Fatalf("被拒绝的添加不该改变条件数，实际 %d", tpl.CriterionCount())
	}
}

// TestAddCriterion_ChecksDuplicateBeforeQuota 确认两条判定的先后顺序。
//
// 顺序不是随意的：一个已经满员的模板里重复添加已有的条件，
// 若先判上限，用户会收到「已达上限」这种驴唇不对马嘴的提示——
// 他真正的问题是那条条件已经存在，删掉别的条件也解决不了。
func TestAddCriterion_ChecksDuplicateBeforeQuota(t *testing.T) {
	tpl := newTestTemplate(t)
	full := numericCriteria(t, entities.MaxCriteriaPerTemplate)
	if err := tpl.ReplaceCriteria(full); err != nil {
		t.Fatalf("填满到上限应当成功: %v", err)
	}

	// 用一条已经存在的 (字段, 比较符) 去撞。
	_, err := tpl.AddCriterion(full[0])
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeAlreadyExists {
		t.Fatalf("满员时的重复添加应报「已存在」而不是「超上限」，实际 %s", code)
	}
}

// TestValidate_RejectsEmptyTemplate 守住「至少一条条件」。
//
// 一个零条件的模板执行起来等于「返回全市场」——那既不是任何人的意图，
// 也是这个接口最容易被误用成全表导出的路径。它在写库前被拦下，
// 于是库里永远不会存在一个不可执行的模板。
func TestValidate_RejectsEmptyTemplate(t *testing.T) {
	tpl := newTestTemplate(t)
	if err := tpl.Validate(); err == nil {
		t.Fatal("零条件的模板必须无法通过自检（也就无法落库）")
	}
	if err := tpl.ReplaceCriteria(nil); err == nil {
		t.Fatal("用空列表整份替换必须被拒绝")
	}
}

// TestRemoveCriterion_RefusesToRemoveLastOne 是上一条的写路径版本。
//
// 与其让模板以一个不可执行的状态留在库里、等到用户点执行时才报错，
// 不如在删除那一刻就说清楚：不再需要它请直接删掉整个模板。
func TestRemoveCriterion_RefusesToRemoveLastOne(t *testing.T) {
	tpl := newTestTemplate(t)
	c, err := tpl.AddCriterion(criterion(t, value_objects.FieldPE, "lt", "20"))
	if err != nil {
		t.Fatalf("添加条件失败: %v", err)
	}
	if err := tpl.RemoveCriterion(c.Key()); err == nil {
		t.Fatal("删除最后一条条件必须被拒绝")
	}
	if tpl.CriterionCount() != 1 {
		t.Fatalf("被拒绝的删除不该改变条件数，实际 %d", tpl.CriterionCount())
	}
}

// ---------------------------------------------------------------------------
// 不变式四：SortOrder 稠密连续
// ---------------------------------------------------------------------------

// TestRemoveCriterion_Resequences 确认删除之后立刻重排序号。
//
// 「删完留个洞、下次加的时候再补」会让洞在前端表现为条件顺序跳变，
// 也会让结果表格的列序对不上用户填写的顺序。
func TestRemoveCriterion_Resequences(t *testing.T) {
	tpl := newTestTemplate(t)
	if err := tpl.ReplaceCriteria(numericCriteria(t, 4)); err != nil {
		t.Fatalf("批量设置条件失败: %v", err)
	}
	assertDense(t, tpl)

	middle := tpl.Criteria[1].Key()
	if err := tpl.RemoveCriterion(middle); err != nil {
		t.Fatalf("删除中间一条应当成功: %v", err)
	}
	if tpl.CriterionCount() != 3 {
		t.Fatalf("条件数应为 3，实际 %d", tpl.CriterionCount())
	}
	assertDense(t, tpl)
}

// TestReplaceCriteria_KeepsMatchingChildren 确认整份替换会复用能对上的旧子实体。
//
// 口径没变的条件必须保留原来的主键与创建时间：否则「把 pe < 20 改成 pe < 15」
// 在库里就成了先 DELETE 再 INSERT，这条条件的 created_at 被刷新，
// 用户在审计视图里会看到一条「刚刚新建」的条件——而他明明只是改了个数字。
func TestReplaceCriteria_KeepsMatchingChildren(t *testing.T) {
	tpl := newTestTemplate(t)
	if err := tpl.ReplaceCriteria([]value_objects.Criterion{
		criterion(t, value_objects.FieldPE, "lt", "20"),
		criterion(t, value_objects.FieldROE, "gt", "15"),
	}); err != nil {
		t.Fatalf("初次设置条件失败: %v", err)
	}
	// 模拟落库之后：给子实体安上主键。这是仓储的活，测试里借 Rehydrate 路径
	// 达到同样的状态过于绕，直接用根提供的回填入口。
	tpl.AssignPersistedID(99)
	if err := tpl.AssignPersistedCriterionIDs([]uint64{101, 102}); err != nil {
		t.Fatalf("回填条件主键失败: %v", err)
	}
	original := tpl.Criteria[0]
	originalCreatedAt := original.CreatedAt

	// 只改第一条的阈值，第二条整个换掉。
	if err := tpl.ReplaceCriteria([]value_objects.Criterion{
		criterion(t, value_objects.FieldPE, "lt", "15"),
		criterion(t, value_objects.FieldTotalMV, "gt", "1000000"),
	}); err != nil {
		t.Fatalf("整份替换失败: %v", err)
	}

	kept := tpl.Criteria[0]
	if kept.ID != 101 {
		t.Fatalf("口径未变的条件应当复用原实体（主键 101），实际 %d", kept.ID)
	}
	if !kept.CreatedAt.Equal(originalCreatedAt) {
		t.Fatal("复用的条件不该被刷新创建时间")
	}
	if kept.Spec.Values()[0] != "15" {
		t.Fatalf("复用的条件应当拿到新取值，实际 %v", kept.Spec.Values())
	}
	// 换掉的那条是新实体，等着仓储 INSERT。
	if tpl.Criteria[1].ID != 0 {
		t.Fatalf("新增的条件主键应为 0，实际 %d", tpl.Criteria[1].ID)
	}
	if len(tpl.PendingCriteria()) != 1 {
		t.Fatalf("待插入的条件应有 1 条，实际 %d 条", len(tpl.PendingCriteria()))
	}
	assertDense(t, tpl)
}

// ---------------------------------------------------------------------------
// 归属与可见性
// ---------------------------------------------------------------------------

// TestTemplate_Visibility 确认「公开只意味着别人能读」。
func TestTemplate_Visibility(t *testing.T) {
	tpl := newTestTemplate(t)
	const other = uint64(999)

	if tpl.ReadableBy(other) {
		t.Fatal("私有模板不该被他人读取")
	}
	if !tpl.OwnedBy(7) || !tpl.ReadableBy(7) {
		t.Fatal("属主对自己的模板应当既可读也可写")
	}

	tpl.SetPublic(true)
	if !tpl.ReadableBy(other) {
		t.Fatal("公开模板应当可被他人读取")
	}
	if tpl.OwnedBy(other) {
		t.Fatal("公开不改变归属：他人依然不能改写")
	}
}

// TestSetPublic_RaisesEventOnlyOnChange 确认重复设置同一可见性不发事件。
//
// 可见性变化是一次授权变更，下游（审计、内容审核）会据此处理。
// 重复提交同一份表单不该让审计日志里多出一条什么都没发生的记录。
func TestSetPublic_RaisesEventOnlyOnChange(t *testing.T) {
	tpl := newTestTemplate(t)
	tpl.AssignPersistedID(5)

	tpl.SetPublic(true)
	if !hasEvent(tpl.GetAllPendingEvents(), domain_events.OnTemplateVisibilityChangedEventName) {
		t.Fatal("首次公开应当抛出可见性变更事件")
	}
	tpl.SetPublic(true)
	if hasEvent(tpl.GetAllPendingEvents(), domain_events.OnTemplateVisibilityChangedEventName) {
		t.Fatal("重复设置同一可见性不该抛出事件")
	}
}

// TestNewScreeningTemplate_RequiresOwnerAndName 守住构造期的两条基本不变式。
func TestNewScreeningTemplate_RequiresOwnerAndName(t *testing.T) {
	name, err := value_objects.NewTemplateName("低估值蓝筹")
	if err != nil {
		t.Fatalf("构造模板名失败: %v", err)
	}
	if _, err := entities.NewScreeningTemplate(0, name, ""); err == nil {
		t.Fatal("无属主的模板必须被拒绝")
	}
	if _, err := entities.NewScreeningTemplate(1, value_objects.TemplateName{}, ""); err == nil {
		t.Fatal("无名模板必须被拒绝")
	}
}

// TestNewScreeningTemplate_RaisesCreatedEvent 确认创建事件被登记。
func TestNewScreeningTemplate_RaisesCreatedEvent(t *testing.T) {
	name, _ := value_objects.NewTemplateName("低估值蓝筹")
	tpl, err := entities.NewScreeningTemplate(7, name, "")
	if err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	if !hasEvent(tpl.GetAllPendingEvents(), domain_events.OnTemplateCreatedEventName) {
		t.Fatal("新建模板应当抛出创建事件")
	}
	// 默认排序与条数必须在构造时就确定：让用户保存完就能在详情里看到
	// 实际生效的排序，而不是一个空着的下拉框。
	if tpl.Sort.IsZero() {
		t.Fatal("新建模板应当带上默认排序")
	}
	if tpl.Limit != value_objects.DefaultResultLimit {
		t.Fatalf("新建模板的默认条数应为 %d，实际 %d", value_objects.DefaultResultLimit, tpl.Limit)
	}
}

// TestSetLimit_ClampsToBounds 确认条数边界收敛复用值对象层的同一个函数。
func TestSetLimit_ClampsToBounds(t *testing.T) {
	tpl := newTestTemplate(t)
	tpl.SetLimit(100000)
	if tpl.Limit != value_objects.MaxResultLimit {
		t.Fatalf("超限条数应收敛到 %d，实际 %d", value_objects.MaxResultLimit, tpl.Limit)
	}
	tpl.SetLimit(-1)
	if tpl.Limit != value_objects.DefaultResultLimit {
		t.Fatalf("非法条数应回落到 %d，实际 %d", value_objects.DefaultResultLimit, tpl.Limit)
	}
}

// TestUpdateCriterion_RefusesFieldSwap 确认不能就地更换条件的字段或比较符。
//
// 就地换字段等价于「删一条、加一条」，那会绕过判重——
// 用户可以把一条条件改成一个模板里已经存在的 (字段, 比较符) 组合。
// 想换字段请走 RemoveCriterion + AddCriterion，那条路上判重是完整的。
func TestUpdateCriterion_RefusesFieldSwap(t *testing.T) {
	tpl := newTestTemplate(t)
	if err := tpl.ReplaceCriteria([]value_objects.Criterion{
		criterion(t, value_objects.FieldPE, "lt", "20"),
		criterion(t, value_objects.FieldROE, "gt", "15"),
	}); err != nil {
		t.Fatalf("设置条件失败: %v", err)
	}
	peKey := tpl.Criteria[0].Key()

	// 合法：只改取值。
	if err := tpl.UpdateCriterion(peKey, criterion(t, value_objects.FieldPE, "lt", "15")); err != nil {
		t.Fatalf("就地改取值应当成功: %v", err)
	}
	if tpl.Criteria[0].Spec.Values()[0] != "15" {
		t.Fatalf("取值应当被更新，实际 %v", tpl.Criteria[0].Spec.Values())
	}

	// 非法：把 pe 那一条换成已经存在的 roe 条件。
	if err := tpl.UpdateCriterion(peKey, criterion(t, value_objects.FieldROE, "gt", "20")); err == nil {
		t.Fatal("就地更换字段/比较符必须被拒绝")
	}
	assertDense(t, tpl)
}
