// Package entities 承载选股筛选上下文的聚合。
//
// 结构与 watchlist 上下文同构，它是本项目「聚合根 ↔ 子实体」的参考实现：
//
//	ScreeningTemplate（聚合根，有仓储）
//	  └── Criterion（子实体，无仓储，只能经由根产生与修改）
//
// 全部业务不变式都住在这里，domain_services 不重复判定，仓储也不重复判定
// （仓储只把其中一条——模板名唯一——落成数据库的唯一索引，因为那一条在并发下
// 内存判断给不出保证）。
package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MaxCriteriaPerTemplate 是单个模板的条件数上限。
//
// 它转引 value_objects.MaxCriteria 而不是另写一个数：临时筛选与保存成模板的筛选
// 是同一件事的两种保存方式，两边给不同的上限会让用户遇到「能筛出来却存不下」
// 这种无从解释的状况。上限的理由见那个常量的注释。
//
// 这里保留一个本层的名字，是因为**它在本层是一条业务不变式**
// （任何入口——HTTP、导入、将来的 OpenAPI——都绕不过聚合，也就绕不过这个数），
// 而在值对象层它只是一个边界常量。同一个数，两层各自的职责。
const MaxCriteriaPerTemplate = value_objects.MaxCriteria

// ScreeningTemplate 是选股筛选上下文的**聚合根**：一套可复用的筛选条件。
//
// # 子实体只能经由根触达
//
// 对 Criterion 的一切访问与修改都必须走本类型的方法：
// AddCriterion / RemoveCriterion / ReplaceCriteria / UpdateCriterion / CriterionByKey。
// 包外既造不出一个 Criterion（构造函数不导出），也改不动一个（修改方法不导出）。
// 这不是风格洁癖，是下面三条不变式能够成立的前提——它们全都需要
// 「看得见全部兄弟节点」才能判定：
//
//   - 同一模板内 (字段, 比较符) 不得重复；
//   - 条件数不超过 MaxCriteriaPerTemplate；
//   - SortOrder 恒为 0..n-1 的稠密连续序列（增、删、重排之后都成立）。
//
// 第四条「至少要有一条条件」由 Validate 守，它在仓储写库前被调用——
// 一个零条件的模板执行起来等于「返回全市场」，那既不是任何人的意图，
// 也是本接口最容易被误用成全表导出的路径。
//
// # 模板名唯一由数据库守
//
// 「同一用户下模板名唯一」是跨聚合实例的规则，内存里的这一个聚合看不见
// 用户的其它模板，因此它在这里没有对应的判断。它由 (user_id, name) 唯一索引
// 在写入语句里保证——先查再插是 TOCTOU，挡不住并发的两次创建。
type ScreeningTemplate struct {
	domain_event.EventRecorder

	// ID 为 0 表示尚未落库，仓储据此在 Save 里选择 INSERT 还是 UPDATE。
	ID          uint64
	UserID      uint64
	Name        value_objects.TemplateName
	Description string

	// Criteria 是子实体集合，随根一起加载、随根一起保存。
	//
	// 切片本身导出是为了让读路径（DTO 投影、接口层渲染）不必绕一层拷贝；
	// 但其中的元素只能读。想增删元素请用 AddCriterion / RemoveCriterion——
	// 直接 append 进来的元素会因为缺少 attached 凭据而被 Validate 拒绝，
	// 仓储在写库前会调用它。
	Criteria []*Criterion

	// Sort / Limit 是模板的执行参数，不是筛选条件，所以随根存在根那一行上。
	Sort  value_objects.SortSpec
	Limit int

	// IsPublic 让模板可以被其他用户只读地使用（选股策略广场）。
	// 它只影响「谁能读」，不影响「谁能改」——修改永远只有属主能做。
	IsPublic bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewScreeningTemplate 是模板进入系统的唯一入口。
//
// 新建出来的模板一条条件都没有，因此**还不能保存**（Validate 会拒绝）。
// 这是刻意的：构造器不收条件，是为了让「加条件」永远走 AddCriterion 那一条
// 带全部不变式判定的路径，而不是在构造器里再复制一份判定逻辑。
func NewScreeningTemplate(
	userID uint64,
	name value_objects.TemplateName,
	description string,
) (*ScreeningTemplate, error) {
	if userID == 0 {
		return nil, custom_errors.Invalid("选股模板必须归属于一个用户")
	}
	if name.IsZero() {
		return nil, custom_errors.Invalid("模板名不能为空")
	}
	now := time.Now()
	t := &ScreeningTemplate{
		UserID:      userID,
		Name:        name,
		Description: value_objects.NormalizeDescription(description),
		// 非 nil 空切片：让「新建的空模板」和「还没加载子实体的模板」在
		// 序列化时都是 []，前端不必区分 null 与 []。
		Criteria:  make([]*Criterion, 0, 8),
		Sort:      value_objects.DefaultSortSpec(),
		Limit:     value_objects.DefaultResultLimit,
		CreatedAt: now,
		UpdatedAt: now,
	}
	t.AddDomainEvent(domain_events.NewOnTemplateCreated(userID, name.String()))
	return t, nil
}

// RehydrateScreeningTemplate 从持久化数据重建聚合根，不做任何校验。
//
// 理由与 watchlist.RehydrateWatchlistGroup 一致：落库的行是既成事实。
// 具体到这里——若某天把 MaxCriteriaPerTemplate 从 20 收紧到 10，
// 重建时再校验一次会让所有存量的复杂模板**整个查不出来**，
// 用户连删掉几条条件以符合新规则的机会都没有。校验属于写入路径。
//
// 注意这条「重建不校验」只针对聚合级不变式。字段名那一层仍然要查白名单，
// 因为它是注入边界，见 value_objects.RehydrateFieldName 的长注释。
func RehydrateScreeningTemplate(
	id, userID uint64,
	name value_objects.TemplateName,
	description string,
	sort value_objects.SortSpec,
	limit int,
	isPublic bool,
	createdAt, updatedAt time.Time,
	criteriaCapacity int,
) *ScreeningTemplate {
	if criteriaCapacity < 0 {
		criteriaCapacity = 0
	}
	return &ScreeningTemplate{
		ID:          id,
		UserID:      userID,
		Name:        name,
		Description: description,
		Criteria:    make([]*Criterion, 0, criteriaCapacity),
		Sort:        sort,
		Limit:       limit,
		IsPublic:    isPublic,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}
}

// RehydrateCriterion 是持久化重建路径上挂载子实体的唯一入口。
//
// 它同样开在根上——「子实体只能经由根产生」这条规则对读路径一视同仁。
// 与 AddCriterion 的区别只有一条：它不跑任何不变式判定，因为它重建的是既成事实，
// 顺序与数量由数据库里的行决定。命名上带 Rehydrate 前缀是为了让它在
// 代码审查里一眼可辨——业务代码里出现它就是错的。
func (t *ScreeningTemplate) RehydrateCriterion(
	id uint64,
	spec value_objects.Criterion,
	sortOrder int,
	createdAt, updatedAt time.Time,
) *Criterion {
	c := newCriterion(t.ID, spec, sortOrder, createdAt)
	c.ID = id
	c.UpdatedAt = updatedAt
	t.Criteria = append(t.Criteria, c)
	return c
}

// ---------------------------------------------------------------------------
// 根自身的行为
// ---------------------------------------------------------------------------

// Rename 改模板名。
//
// 改成原名直接返回成功而不是报错：重复提交同一份表单不该是一个错误。
// 与别的模板重名这条规则在这里判不了（看不见别的模板），由唯一索引兜底。
func (t *ScreeningTemplate) Rename(name value_objects.TemplateName) error {
	if name.IsZero() {
		return custom_errors.Invalid("模板名不能为空")
	}
	if t.Name.Equal(name) {
		return nil
	}
	t.Name = name
	t.UpdatedAt = time.Now()
	return nil
}

func (t *ScreeningTemplate) UpdateDescription(desc string) {
	normalized := value_objects.NormalizeDescription(desc)
	if t.Description == normalized {
		return
	}
	t.Description = normalized
	t.UpdatedAt = time.Now()
}

// SetSort 设置排序规则。零值排序会被归一成默认排序而不是留空：
// 「没有排序」在执行时必然要回落到某个确定顺序，把这个回落放在写入点，
// 用户保存完就能在模板详情里看到实际生效的排序，而不是一个空着的下拉框。
func (t *ScreeningTemplate) SetSort(sort value_objects.SortSpec) {
	resolved := sort.OrDefault()
	if t.Sort.Field().Equal(resolved.Field()) && t.Sort.Direction() == resolved.Direction() {
		return
	}
	t.Sort = resolved
	t.UpdatedAt = time.Now()
}

// SetLimit 设置返回条数，边界收敛复用值对象层的同一个函数。
func (t *ScreeningTemplate) SetLimit(limit int) {
	clamped := value_objects.ClampLimit(limit)
	if t.Limit == clamped {
		return
	}
	t.Limit = clamped
	t.UpdatedAt = time.Now()
}

// SetPublic 公开或收回模板。
func (t *ScreeningTemplate) SetPublic(isPublic bool) {
	if t.IsPublic == isPublic {
		return
	}
	t.IsPublic = isPublic
	t.UpdatedAt = time.Now()
	t.AddDomainEvent(domain_events.NewOnTemplateVisibilityChanged(t.UserID, t.ID, isPublic))
}

func (t *ScreeningTemplate) OwnedBy(userID uint64) bool { return t.UserID == userID }

// ReadableBy 判定谁能读这个模板：属主，或任何人（当它被公开时）。
//
// 做成实体方法而不是让 domain_service 各自写 `if t.IsPublic || t.UserID == op.UserID`：
// 可见性规则变化（比如将来要支持「分享给指定用户」）时只应该改这一处。
func (t *ScreeningTemplate) ReadableBy(userID uint64) bool {
	return t.IsPublic || t.OwnedBy(userID)
}

func (t *ScreeningTemplate) CriterionCount() int { return len(t.Criteria) }

// Specs 把全部条件投影成值对象切片，是执行筛选的入口数据。
//
// 它的存在让 domain_service 不必遍历 t.Criteria 去摸子实体——
// 交出去的是一组不可变的规则拷贝，执行器拿到它既改不动模板，也不需要
// 知道「条件」在模板里还有主键和排序位置这回事。
func (t *ScreeningTemplate) Specs() []value_objects.Criterion {
	out := make([]value_objects.Criterion, 0, len(t.Criteria))
	for _, c := range t.Criteria {
		out = append(out, c.Spec)
	}
	return out
}

// ---------------------------------------------------------------------------
// 经由根操作子实体
// ---------------------------------------------------------------------------

// AddCriterion 往模板里加一条筛选条件，是条件进入系统的**唯一领域入口**。
//
// 三条不变式在这里一次性判完，顺序是刻意的：
//  1. 规则本身合法（由值对象在它自己的构造点保证，这里只挡零值）；
//  2. 同一 (字段, 比较符) 不重复——先判重复再判上限，否则一个已经满员的模板里
//     重复添加已有的条件，用户会收到「已达上限」这种驴唇不对马嘴的提示；
//  3. 未超上限。
//
// 新条件排在末尾（SortOrder = 当前数量），天然保持序列稠密连续。
//
// 返回新建的子实体是为了让调用方读它（比如回显），不是为了让它改。
// 修改方法都不导出，包外拿到指针也改不动。
func (t *ScreeningTemplate) AddCriterion(spec value_objects.Criterion) (*Criterion, error) {
	if spec.IsZero() {
		return nil, custom_errors.Invalid("筛选条件不完整")
	}
	if existing, ok := t.CriterionByKey(spec.Key()); ok {
		return nil, custom_errors.AlreadyExists(
			"模板「%s」中已存在同一字段与比较符的条件: %s",
			t.Name.String(), existing.Describe())
	}
	if len(t.Criteria) >= MaxCriteriaPerTemplate {
		return nil, custom_errors.QuotaExceeded(
			"模板「%s」最多容纳 %d 条筛选条件，当前已有 %d 条",
			t.Name.String(), MaxCriteriaPerTemplate, len(t.Criteria))
	}

	now := time.Now()
	c := newCriterion(t.ID, spec, len(t.Criteria), now)
	t.Criteria = append(t.Criteria, c)
	t.UpdatedAt = now
	t.raiseCriteriaChanged()
	return c, nil
}

// RemoveCriterion 按 (字段, 比较符) 删掉一条条件。
//
// 删除之后立刻重排序号：SortOrder 的稠密连续是一条不变式，
// 「删完留个洞、下次加的时候再补」会让洞在前端表现为条件顺序跳变。
//
// 删掉最后一条会被拒绝：一个零条件的模板是无法执行的（执行等于返回全市场），
// 与其让它以一个坏状态存在库里、等到用户点执行时才报错，不如在这里就说清楚。
// 想清空条件的用户应该删掉整个模板，或者用 ReplaceCriteria 换一批。
func (t *ScreeningTemplate) RemoveCriterion(key string) error {
	idx := t.indexOf(key)
	if idx < 0 {
		return custom_errors.NotFound("模板「%s」中不存在该筛选条件", t.Name.String())
	}
	if len(t.Criteria) == 1 {
		return custom_errors.Invalid(
			"模板「%s」至少要保留一条筛选条件；不再需要它请直接删除模板", t.Name.String())
	}

	now := time.Now()
	t.Criteria = append(t.Criteria[:idx], t.Criteria[idx+1:]...)
	t.resequence(now)
	t.UpdatedAt = now
	t.raiseCriteriaChanged()
	return nil
}

// UpdateCriterion 就地改一条已有条件的取值（字段与比较符不变）。
//
// 它开在根上而不是子实体上：包外根本拿不到能改动 Criterion 的方法，
// 所有写入都必须先经过根，根才有机会维护 UpdatedAt 与事件。
//
// 允许改值但不允许改字段/比较符，是因为后者等价于「删一条、加一条」——
// 那会绕过判重（改成一个已存在的 (字段, 比较符) 组合），
// 想换字段的用户走 RemoveCriterion + AddCriterion，那条路上判重是完整的。
func (t *ScreeningTemplate) UpdateCriterion(key string, spec value_objects.Criterion) error {
	if spec.IsZero() {
		return custom_errors.Invalid("筛选条件不完整")
	}
	c, ok := t.CriterionByKey(key)
	if !ok {
		return custom_errors.NotFound("模板「%s」中不存在该筛选条件", t.Name.String())
	}
	if spec.Key() != key {
		return custom_errors.Invalid(
			"不能就地更换条件的字段或比较符，请先删除原条件再新增")
	}
	now := time.Now()
	c.setSpec(spec, now)
	t.UpdatedAt = now
	t.raiseCriteriaChanged()
	return nil
}

// ReplaceCriteria 用一组新条件整体替换模板现有的条件。
//
// # 为什么需要它，而不是让调用方循环 Remove + Add
//
// 编辑模板的表单一次提交整份条件列表。逐条增删的话，中间态会反复经过
// 「零条件」和「超上限」这些非法状态，于是要么校验必须被临时关掉
// （那就等于没有），要么调用方得自己算出一个不触雷的增删顺序——
// 那是把聚合的不变式知识泄漏到了调用方手里。
//
// 整体替换让不变式只在**替换完成后的终态**上判定一次，中间态根本不存在。
//
// # 为什么保留能对上的旧子实体
//
// 按 (字段, 比较符) 对齐：口径没变的条件保留原来的主键与创建时间。
// 这样「把 pe < 20 改成 pe < 15」在库里是一次 UPDATE，而不是先 DELETE 再 INSERT。
// 后者会让这条条件的 created_at 被刷新，用户在审计视图里会看到一条
// 「刚刚新建」的条件——而他明明只是改了个数字。
func (t *ScreeningTemplate) ReplaceCriteria(specs []value_objects.Criterion) error {
	if len(specs) == 0 {
		return custom_errors.Invalid("模板至少需要一条筛选条件")
	}
	if len(specs) > MaxCriteriaPerTemplate {
		return custom_errors.QuotaExceeded(
			"模板最多容纳 %d 条筛选条件，本次提交 %d 条", MaxCriteriaPerTemplate, len(specs))
	}

	now := time.Now()
	seen := make(map[string]struct{}, len(specs))
	next := make([]*Criterion, 0, len(specs))
	for _, spec := range specs {
		if spec.IsZero() {
			return custom_errors.Invalid("筛选条件不完整")
		}
		key := spec.Key()
		if _, dup := seen[key]; dup {
			return custom_errors.AlreadyExists("筛选条件重复: %s", spec.Describe())
		}
		seen[key] = struct{}{}

		if existing, ok := t.CriterionByKey(key); ok {
			// 口径能对上就复用原实体，只更新取值。
			existing.setSpec(spec, now)
			next = append(next, existing)
			continue
		}
		next = append(next, newCriterion(t.ID, spec, len(next), now))
	}

	t.Criteria = next
	// 复用下来的旧实体位置可能变了，统一重排一次，稠密连续因此是构造出来的。
	t.resequence(now)
	t.UpdatedAt = now
	t.raiseCriteriaChanged()
	return nil
}

// CriterionByKey 按 (字段, 比较符) 查子实体。这是包外读取单条条件的唯一入口。
func (t *ScreeningTemplate) CriterionByKey(key string) (*Criterion, bool) {
	idx := t.indexOf(key)
	if idx < 0 {
		return nil, false
	}
	return t.Criteria[idx], true
}

// ---------------------------------------------------------------------------
// 完整性自检
// ---------------------------------------------------------------------------

// Validate 在写库之前自检整个聚合，由仓储的 Save 调用。
//
// # 为什么需要它
//
// 「子实体只能经由根产生与修改」这条规则的绝大部分由包可见性守住了：
// newCriterion 与 setXxx 都不导出。唯一的缺口是 Criteria 切片本身导出——
// 包外可以 append 一个自己 new 出来的 Criterion 进去。编译器拦不住这一步，
// 但那样造出来的子实体拿不到 attached 凭据（不导出的字段包外赋不了值），
// 于是这里能把它揪出来。
//
// 它同时复查另外几条不变式。这不是不信任 AddCriterion——它们在那里已经判过了；
// 而是因为这是聚合离开内存、变成持久化事实之前的最后一道关：
// 一旦写进去，破损的状态就会被后续的每一次加载当成既成事实接受。
func (t *ScreeningTemplate) Validate() error {
	if t.UserID == 0 {
		return custom_errors.Invalid("选股模板必须归属于一个用户")
	}
	if t.Name.IsZero() {
		return custom_errors.Invalid("模板名不能为空")
	}
	// 「至少一条条件」在这里守：它是「这个模板是否可执行」的定义，
	// 而一个不可执行的模板没有存在的理由。
	if len(t.Criteria) == 0 {
		return custom_errors.Invalid("模板「%s」至少需要一条筛选条件", t.Name.String())
	}
	if len(t.Criteria) > MaxCriteriaPerTemplate {
		return custom_errors.QuotaExceeded(
			"模板「%s」最多容纳 %d 条筛选条件，当前 %d 条",
			t.Name.String(), MaxCriteriaPerTemplate, len(t.Criteria))
	}

	seen := make(map[string]struct{}, len(t.Criteria))
	for i, c := range t.Criteria {
		if c == nil {
			return custom_errors.Internal("模板「%s」的第 %d 条条件为空", t.Name.String(), i)
		}
		if !c.isAttached() {
			return custom_errors.Internal(
				"筛选条件「%s」不是由聚合根创建的：条件只能经由 ScreeningTemplate.AddCriterion 产生",
				c.Describe())
		}
		if c.TemplateID != t.ID {
			return custom_errors.Internal(
				"筛选条件「%s」归属模板(id=%d) 与当前模板(id=%d) 不一致",
				c.Describe(), c.TemplateID, t.ID)
		}
		if c.Spec.IsZero() {
			// 零值条件只可能来自一条无法识别的持久化记录（字段不在白名单里）。
			// 让它在写回时失败，而不是让它继续留在库里被下一次执行消费。
			return custom_errors.Invalid(
				"模板「%s」的第 %d 条条件字段无法识别，请删除后重新添加", t.Name.String(), i)
		}
		key := c.Key()
		if _, dup := seen[key]; dup {
			return custom_errors.AlreadyExists(
				"模板「%s」中条件重复: %s", t.Name.String(), c.Describe())
		}
		seen[key] = struct{}{}
		if c.SortOrder != i {
			return custom_errors.Internal(
				"模板「%s」的条件排序不连续：第 %d 条的 SortOrder 为 %d",
				t.Name.String(), i, c.SortOrder)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// resequence 把 SortOrder 重新刷成 0..n-1。
// 只有真正变了的那几条会被打上新的 UpdatedAt（见 setSortOrder），
// 仓储据此只 UPDATE 发生了变化的行。
func (t *ScreeningTemplate) resequence(now time.Time) {
	for i, c := range t.Criteria {
		c.setSortOrder(i, now)
	}
}

// indexOf 线性查找。单模板条件数上限 20，线性扫比维护一个需要跟着增删同步的 map
// 更划算，也让聚合保持「纯数据 + 方法」的形状，便于直接从 DTO 重建。
func (t *ScreeningTemplate) indexOf(key string) int {
	if key == "" {
		return -1
	}
	for i, c := range t.Criteria {
		if c.Key() == key {
			return i
		}
	}
	return -1
}

// raiseCriteriaChanged 抛出条件变更事件。
//
// 新建模板（ID == 0）时不抛：此刻聚合还没落库，事件里带不出可用的 TemplateID，
// 而「模板创建了，附带这些条件」这件事 OnTemplateCreated 已经说过了。
func (t *ScreeningTemplate) raiseCriteriaChanged() {
	if t.ID == 0 {
		return
	}
	t.AddDomainEvent(domain_events.NewOnTemplateCriteriaChanged(
		t.UserID, t.ID, t.Name.String(), len(t.Criteria)))
}

// ---------------------------------------------------------------------------
// 供仓储回填主键（仍然经由根）
// ---------------------------------------------------------------------------

// AssignPersistedID 供仓储在 INSERT 之后回填自增主键。
//
// 它必须开在根上：新建模板时子实体的 template_id 还是 0，回填只发生一次，
// 且必须同时刷到根和全部子实体上，否则紧接着的子实体 INSERT 会写进
// template_id = 0 这条黑洞记录里。让仓储自己去遍历 Criteria 改 TemplateID 是不行的——
// 那是包外代码在修改子实体。
func (t *ScreeningTemplate) AssignPersistedID(id uint64) {
	t.ID = id
	for _, c := range t.Criteria {
		c.bindTemplate(id)
	}
}

// PendingCriteria 返回尚未落库的子实体（ID == 0），按它们在模板内的顺序。
//
// 它是仓储做子实体 diff 的输入之一：仓储据此知道「这几行要 INSERT」，
// 而不需要自己去猜。返回的仍然是只读视图——包外拿到指针也调不动任何修改方法。
func (t *ScreeningTemplate) PendingCriteria() []*Criterion {
	out := make([]*Criterion, 0, len(t.Criteria))
	for _, c := range t.Criteria {
		if c.ID == 0 {
			out = append(out, c)
		}
	}
	return out
}

// AssignPersistedCriterionIDs 在子实体批量 INSERT 之后回填它们的自增主键。
//
// ids 必须与 PendingCriteria() 的返回顺序一一对应——批量 INSERT 的主键
// 就是按这个顺序生成的。长度对不上说明仓储与聚合对「哪些是新行」的理解出现了分歧，
// 那种情况下继续回填只会把主键错配到别的行上，不如立刻失败。
func (t *ScreeningTemplate) AssignPersistedCriterionIDs(ids []uint64) error {
	pending := t.PendingCriteria()
	if len(pending) != len(ids) {
		return custom_errors.Internal(
			"筛选条件主键回填数量不匹配：待回填 %d 条，实际拿到 %d 个主键",
			len(pending), len(ids))
	}
	for i, c := range pending {
		c.ID = ids[i]
	}
	return nil
}
