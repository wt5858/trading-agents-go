package repositories

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ScreeningTemplateRepository 持久化 ScreeningTemplate 聚合——**根和它的全部子实体**。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
//
// 它碰两张表（screening_templates 和 screening_criteria），这与「一个仓储只碰一张表」
// 的常见说法并不冲突：真正的规则是**一个仓储只服务一个聚合**。
// 这两张表属于同一个聚合，它们之间的一致性正是这个聚合存在的理由，
// 因此它们必须在同一个事务里写——反过来，本仓储也绝不会去碰任何别的聚合的表
// （stocks / quotes 属于股票上下文，由 StockScreener 只读地访问）。
type ScreeningTemplateRepository struct {
	db *gorm.DB
}

func NewScreeningTemplateRepository(db *gorm.DB) *ScreeningTemplateRepository {
	return &ScreeningTemplateRepository{db: db}
}

// ---------------------------------------------------------------------------
// 写入：整聚合保存
// ---------------------------------------------------------------------------

// Save 保存整个聚合：根 + 全部子实体，一个事务。
//
// # 契约：调用方只交出根
//
// 签名里只有 *entities.ScreeningTemplate。没有 SaveCriterion，没有
// AddCriterion(ctx, templateID, ...)，没有 DeleteCriterion——子实体级别的
// 持久化 API 在本层压根不存在。调用方永远是「把整个聚合交出去」，
// 至于其中哪一条要 INSERT、哪一条要 UPDATE、哪一条已经被移除要 DELETE，
// 是本方法的**实现细节**，对上层完全不可见。
//
// 这条契约买到的是：上层永远不可能写出「改了一条条件却没同步模板的 updated_at」
// 或者「删了条件忘了重排序号」这类破坏聚合一致性的代码——上层根本没有那样的 API 可用。
//
// # 子实体 diff 怎么做
//
// 事务里先把库中现存的条件行捞出来（带 FOR UPDATE 行锁），与内存中的
// t.Criteria 按主键对齐，分成三组：
//
//	新增：内存里 ID == 0 的（只可能由 AddCriterion / ReplaceCriteria 产生）
//	更新：ID 能对上、但可变列不同的（取值改了、排序变了）
//	删除：库里有、内存里没有的（RemoveCriterion / ReplaceCriteria 移走的）
//
// 三组各发**一条**语句（批量 DELETE / 批量 INSERT / 批量 upsert），
// 加上开头的那次 SELECT 和根自身的一条语句，整个 Save 是**常数条 SQL**，
// 与条件数量无关。绝不逐条 UPDATE。
//
// # 为什么先写根、后写子实体
//
// 两个原因，都不是随便排的：
//   - 新建模板时，子实体的 template_id 要等根的自增主键回填之后才知道；
//   - 并发时，两个 Save 都先在根那一行上排队（INSERT 或 UPDATE 都会锁住它），
//     于是它们取子实体行锁的顺序天然一致，不存在交叉加锁的死锁。
func (repo *ScreeningTemplateRepository) Save(ctx context.Context, t *entities.ScreeningTemplate) error {
	if t == nil {
		return custom_errors.Invalid("待保存的选股模板为空")
	}
	// 聚合自检放在最前面：它能挡住「绕过根塞进来的条件」、零条件模板和排序断裂
	// 这类已经在内存里破损的状态。一旦写进库，破损就变成了后续每次加载都会接受的既成事实。
	if err := t.Validate(); err != nil {
		return err
	}

	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		isNew := t.ID == 0
		if err := repo.saveRoot(tx, t, isNew); err != nil {
			return err
		}
		return repo.saveChildren(tx, t, isNew)
	})
	if err != nil {
		return repo.translateSave(err, t)
	}
	return nil
}

// saveRoot 写根那一行。
func (repo *ScreeningTemplateRepository) saveRoot(tx *gorm.DB, t *entities.ScreeningTemplate, isNew bool) error {
	dto := dtos.FromDomainTemplate(t)
	if isNew {
		if err := tx.Create(dto).Error; err != nil {
			return err
		}
		// 回填必须经由根：它要同时刷到根和全部子实体的 template_id 上。
		t.AssignPersistedID(dto.ID)
		return nil
	}

	// user_id 进 WHERE 而不是只用 id：这样「改别人的模板」在数据层就是 0 行，
	// 上层拿到的是 NotFound 而不是一次成功的越权写入。归属判定在领域服务里
	// 已经做过一次，这里是纵深防御——真正的并发保证只有写在语句里才成立。
	res := tx.Model(&dtos.ScreeningTemplateDto{}).
		Where("id = ? AND user_id = ?", dto.ID, dto.UserID).
		Updates(map[string]any{
			"name":           dto.Name,
			"description":    dto.Description,
			"sort_field":     dto.SortField,
			"sort_direction": dto.SortDirection,
			"result_limit":   dto.ResultLimit,
			"is_public":      dto.IsPublic,
			"updated_at":     dto.UpdatedAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// 0 行有歧义：行可能不存在（或不属于这个用户），也可能是值压根没变
	// （只改了某条条件的取值，而那次修改把 updated_at 刷成了同一毫秒）。
	// 只在这一条分支上多查一次，正常路径仍然只有一条语句。
	return repo.assertTemplateExists(tx, dto.ID, dto.UserID)
}

// saveChildren 是子实体的三路 diff，也就是「整聚合保存」的全部实现细节。
func (repo *ScreeningTemplateRepository) saveChildren(tx *gorm.DB, t *entities.ScreeningTemplate, isNew bool) error {
	var existing []dtos.ScreeningCriterionDto
	if !isNew {
		// FOR UPDATE 锁住本模板现存的条件行。
		//
		// 「先 SELECT 出来，再据此决定删哪几行」如果不加锁就是 TOCTOU：
		// 两个并发的 Save 会看到同一份现状，各自算出一份 diff，后提交的那份
		// 会把对方刚插入的行当成「库里多出来的」而删掉。行锁让第二个事务
		// 必须等第一个提交后再读，从而读到的是真正的当前状态。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("template_id = ?", t.ID).
			Find(&existing).Error
		if err != nil {
			return err
		}
	}

	existingByID := make(map[uint64]dtos.ScreeningCriterionDto, len(existing))
	for _, row := range existing {
		existingByID[row.ID] = row
	}

	// 已落库的那部分：对齐主键，只挑出可变列真的变了的行。
	desired := dtos.FromDomainCriteria(t.ID, t.Criteria)
	keep := make(map[uint64]struct{}, len(desired))
	toUpdate := make([]*dtos.ScreeningCriterionDto, 0, len(desired))
	for _, d := range desired {
		if d.ID == 0 {
			continue // 新增的走下面 PendingCriteria 那条路
		}
		current, ok := existingByID[d.ID]
		if !ok {
			// 内存里有一条自认为已落库的条件，库里却没有。
			// 只可能是别人在我们持有这份聚合期间把它删掉了（或者把整个模板删了）。
			// 继续写下去会凭空复活一条用户已经删掉的记录，必须让调用方重来。
			return custom_errors.Conflict(
				"选股模板(id=%d) 已被并发修改（条件 %s %s 已不存在），请重新加载后重试",
				t.ID, d.Field, d.Operator)
		}
		keep[d.ID] = struct{}{}
		if !d.SameAs(current) {
			toUpdate = append(toUpdate, d)
		}
	}

	// 删除：库里有、内存里没有的。遍历 existing 切片而不是 map，
	// 是为了让生成的 IN 列表顺序稳定，慢查询日志与线上复现才对得上。
	toDelete := make([]uint64, 0, len(existing))
	for _, row := range existing {
		if _, ok := keep[row.ID]; !ok {
			toDelete = append(toDelete, row.ID)
		}
	}

	// 顺序要紧：DELETE 必须先于 INSERT。
	// 「把 pe > 10 删掉、又在同一次提交里加一条 pe > 20」会产生一条待删的旧行
	// 和一条待插的新行，它们的 (template_id, field, operator) 相同，
	// 先插就会撞上唯一索引。
	if len(toDelete) > 0 {
		err := tx.Where("template_id = ? AND id IN ?", t.ID, toDelete).
			Delete(&dtos.ScreeningCriterionDto{}).Error
		if err != nil {
			return err
		}
	}

	if pending := t.PendingCriteria(); len(pending) > 0 {
		rows := dtos.FromDomainCriteria(t.ID, pending)
		// 一条多值 INSERT，不是 len(pending) 条。
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		ids := make([]uint64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		// 主键回填同样经由根：子实体的任何字段都不由本层直接写。
		if err := t.AssignPersistedCriterionIDs(ids); err != nil {
			return err
		}
	}

	if len(toUpdate) > 0 {
		// 一条 INSERT ... ON DUPLICATE KEY UPDATE 覆盖全部改动行。
		//
		// 为什么不是 for 里逐行 UPDATE：每行的 values_json 和 sort_order 都不同，
		// 没法合并成一条带统一 SET 的语句；而重排一次条件顺序恰恰会让
		// 每一行的 sort_order 都变化。upsert 的批量形式是在保留「逐行不同的值」
		// 的前提下把它压成一条语句的办法。
		//
		// 这些行的主键都已存在（上面刚核对过），因此必然走 UPDATE 分支，
		// 不会凭空插入。冲突列只认主键，业务唯一键的冲突仍然要如实报错。
		err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"values_json", "sort_order", "updated_at"}),
		}).Create(&toUpdate).Error
		if err != nil {
			return err
		}
	}
	return nil
}

// Delete 删除整个聚合：模板连同它的全部筛选条件，一个事务。
//
// 「删模板顺带删条件」不是级联，而是这个聚合的定义——子实体的生命周期
// 完全从属于根，根没了它们就不该存在。所以它必须和根的删除在同一个事务里，
// 否则会留下一堆 template_id 指向空气的孤儿行，而且它们还占着唯一索引的位置。
//
// 不用外键的 ON DELETE CASCADE：数据库级联是**看不见**的，读这段代码的人
// 不会知道还有一张表被清空了。既然本层已经持有事务，把这一步写成一条显式语句
// 成本为零，收益是它出现在代码里。
//
// userID 进 WHERE：越权删除在数据层就是 0 行，上层因此拿到 NotFound 而不是成功。
func (repo *ScreeningTemplateRepository) Delete(ctx context.Context, id, userID uint64) error {
	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先删根：它顺便充当了存在性与归属的判定。判定不成立时整个事务回滚，
		// 子实体那条 DELETE 压根不会执行。
		res := tx.Where("id = ? AND user_id = ?", id, userID).
			Delete(&dtos.ScreeningTemplateDto{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 不存在与不属于你，对外是同一个答案：返回 Forbidden 等于确认
			// 「这个 id 确实存在」，会让模板 ID 空间变成可探测的信道。
			return custom_errors.NotFound("选股模板(id=%d) 不存在", id)
		}
		return tx.Where("template_id = ?", id).Delete(&dtos.ScreeningCriterionDto{}).Error
	})
	if err != nil {
		return translatef(err, "选股模板(id=%d)", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 读取：加载整聚合
// ---------------------------------------------------------------------------

// FindByID 加载完整聚合：根 + 它的全部子实体。
//
// **两条查询**，与条件数量无关：一条取模板，一条取它的条件。
// 绝不是「先取模板，再对每条条件查一次」——那是 N+1。
//
// 没有「只加载根、不加载条件」的重载，这是刻意的：那样的半个聚合一旦流到
// Save 手上，会被如实理解为「用户清空了这个模板的条件」，
// 一次读取变成一次破坏（实际会被 Validate 的「至少一条」拦下，但那是靠运气）。
//
// 两条查询不包在事务里：这是只读路径，最坏情况是渲染出一份晚了一个操作的模板，
// 而为一次详情查询付出事务与快照的代价并不划算。真正需要一致视图的是写路径，
// Save 会在它自己的事务里带锁重新读一次子实体。
func (repo *ScreeningTemplateRepository) FindByID(ctx context.Context, id uint64) (*entities.ScreeningTemplate, error) {
	var row dtos.ScreeningTemplateDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return nil, translatef(err, "选股模板(id=%d)", id)
	}
	criteriaRows, err := repo.loadCriteria(ctx, []uint64{id})
	if err != nil {
		return nil, err
	}
	return row.ToDomain(criteriaRows), nil
}

// ListByUser 列出用户的全部模板，每个都带齐条件。
//
// # 它为什么不是 N+1
//
// 天真的写法是「查出模板列表，然后对每个模板调一次 FindByID」，那是
// 1 + 模板数 次查询。这里改成：一条查询取全部模板，拿到它们的 id 之后
// 用一条 WHERE template_id IN (...) 取回**所有**模板的全部条件，
// 最后在内存里按 template_id 缝回去。总共两条查询，与模板数无关。
func (repo *ScreeningTemplateRepository) ListByUser(ctx context.Context, userID uint64) ([]*entities.ScreeningTemplate, error) {
	var rows []dtos.ScreeningTemplateDto
	err := repo.db.WithContext(ctx).
		Where("user_id = ?", userID).
		// 走 uk_screening_templates_user_name 的 user_id 前缀定位；
		// 按更新时间倒序让最近编辑的模板排在最前，补一个 id 做 tie-break，
		// 否则同一毫秒更新的两个模板每次刷新的先后可能不同。
		Order("updated_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "用户(id=%d) 的选股模板列表", userID)
	}
	return repo.hydrate(ctx, rows)
}

// ListPublic 列出公开模板（选股策略广场），分页。
//
// 这条路径要分页而 ListByUser 不用：一个用户的模板是个位数到几十个的量级，
// 而公开模板是全站共享的池子，没有任何自然上界。
func (repo *ScreeningTemplateRepository) ListPublic(
	ctx context.Context, page shared_vo.Page,
) ([]*entities.ScreeningTemplate, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.ScreeningTemplateDto{}).Where("is_public = ?", true)
	// Session 把链式条件固化成可复用的基准查询：GORM 的 *gorm.DB 在执行过 Count
	// 这类终结方法后再复用会串条件，这是官方给的正确复用姿势。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translate(err, "公开选股模板")
	}
	if total == 0 {
		return []*entities.ScreeningTemplate{}, 0, nil
	}

	var rows []dtos.ScreeningTemplateDto
	err := q.Order("updated_at DESC, id DESC").
		Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translate(err, "公开选股模板")
	}
	templates, err := repo.hydrate(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return templates, total, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// hydrate 是「一批模板行 → 一批完整聚合」的公共后半段：取条件、缝合。
// 抽出来是因为 ListByUser 与 ListPublic 只有前半段的 WHERE 不同。
func (repo *ScreeningTemplateRepository) hydrate(
	ctx context.Context, rows []dtos.ScreeningTemplateDto,
) ([]*entities.ScreeningTemplate, error) {
	if len(rows) == 0 {
		return []*entities.ScreeningTemplate{}, nil
	}
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	criteriaRows, err := repo.loadCriteria(ctx, ids)
	if err != nil {
		return nil, err
	}
	return dtos.ToDomainTemplates(rows, criteriaRows), nil
}

// loadCriteria 一次取回若干模板的全部条件，是「不出现 N+1」的那条查询。
//
// 排序交给数据库而不是内存：idx_screening_criteria_template_sort 正是
// (template_id, sort_order)，这个 ORDER BY 直接吃索引，不需要额外的排序开销。
func (repo *ScreeningTemplateRepository) loadCriteria(ctx context.Context, templateIDs []uint64) ([]dtos.ScreeningCriterionDto, error) {
	var rows []dtos.ScreeningCriterionDto
	err := repo.db.WithContext(ctx).
		Where("template_id IN ?", templateIDs).
		Order("template_id ASC, sort_order ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "选股模板(共 %d 个) 的筛选条件", len(templateIDs))
	}
	return rows, nil
}

// assertTemplateExists 把一次条件 UPDATE 的「0 行」收敛成一个确定的答案。
func (repo *ScreeningTemplateRepository) assertTemplateExists(tx *gorm.DB, id, userID uint64) error {
	var n int64
	err := tx.Model(&dtos.ScreeningTemplateDto{}).
		Where("id = ? AND user_id = ?", id, userID).Count(&n).Error
	if err != nil {
		return err
	}
	if n == 0 {
		return custom_errors.NotFound("选股模板(id=%d) 不存在", id)
	}
	// 行在、值没变：当成成功。重复提交同一份表单不该是一个错误。
	return nil
}

// translateSave 把事务里冒出来的技术错误翻译成对用户有意义的话。
//
// 唯一键冲突在这里分两种，它们对应两条完全不同的不变式，也对应两句
// 完全不同的提示。分辨的依据是索引名（见 violatedIndex）。
//
// 注意这里**没有**「先查有没有重名再插」：那是 TOCTOU。两个并发的创建请求
// 会双双查到「不重名」，然后双双插入。唯一索引才是这条不变式的真正保证，
// 本方法只负责把它的 1062 翻译成 AlreadyExists。
func (repo *ScreeningTemplateRepository) translateSave(err error, t *entities.ScreeningTemplate) error {
	var de *custom_errors.Error
	if errors.As(err, &de) {
		return de
	}
	if isDuplicateKey(err) {
		switch violatedIndex(err) {
		case idxTemplateUserName:
			return custom_errors.AlreadyExists("已存在同名的选股模板「%s」", t.Name.String()).Wrap(err)
		case idxCriterionTemplateFK:
			return custom_errors.AlreadyExists("模板「%s」中存在重复的筛选条件", t.Name.String()).Wrap(err)
		}
	}
	return translatef(err, "选股模板「%s」", t.Name.String())
}
