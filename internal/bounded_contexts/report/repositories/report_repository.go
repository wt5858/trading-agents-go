package repositories

import (
	"context"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ReportRepository 持久化 Report 聚合。它只碰 analysis_reports 一张表。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
//
// 本仓储里没有任何 Transaction 调用，这不是疏漏而是结论：下面每个方法都是
// 单条语句，单条语句本身就是原子的。为了「看起来更安全」把一条 INSERT 包进事务，
// 只会平白拉长锁持有时间。真正需要原子性的地方（重复生成、越权删除）都被写成了
// 语句自身的约束或谓词，那比事务更强——它在并发下也成立。
type ReportRepository struct {
	db *gorm.DB
}

func NewReportRepository(db *gorm.DB) *ReportRepository {
	return &ReportRepository{db: db}
}

// Create 落库一份新报告。
//
// # 为什么是裸 INSERT，而不是 ON CONFLICT DO NOTHING
//
// 分析任务的 Save 用了 DO NOTHING，因为「用户重复点了提交」是无害的，语义就该是 no-op。
// 报告不同：报告由领域事件触发，同一个 task_id 出现第二次插入，说明这次分析被消费了两遍。
// 那可能只是一次无害的事件重放，也可能是队列的可见性超时在让两个 worker 跑同一个任务——
// 后者是必须被看见的 bug。把它吞成 no-op，就等于把这条唯一能发现问题的信号也关掉了。
//
// 所以这里让唯一键冲突如实浮上去（translate 会翻成 AlreadyExists），
// 由事件处理器去决定「重放是良性的」——那是投递语义的问题，不是持久化的问题。
//
// # 为什么没有「先查 task_id 存不存在」
//
// 查完再插是 TOCTOU：两次重放并发进来，两边都查到「不存在」，然后都插。
// 唯一索引才是真正的保证，先查一次只是多一次往返，挡不住任何东西。
func (repo *ReportRepository) Create(ctx context.Context, r *entities.Report) error {
	if err := repo.db.WithContext(ctx).Create(dtos.FromDomainReport(r)).Error; err != nil {
		return translatef(err, "分析任务(id=%s) 的报告", r.TaskID)
	}
	return nil
}

func (repo *ReportRepository) FindByID(ctx context.Context, id string) (*entities.Report, error) {
	var dto dtos.ReportDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		return nil, translatef(err, "分析报告(id=%s)", id)
	}
	return dto.ToDomain(), nil
}

// ListByUser 列出某用户的报告，走 idx_reports_user_created。
//
// 排序用 created_at DESC, id DESC：报告 ID 不保证单调，只按 created_at 排，
// 同毫秒生成的一批报告（批量分析收尾时很常见）会在翻页时重复出现，补 id 做 tie-break。
//
// # 关于把 sections 一起读出来
//
// 列表页并不展示章节正文，这里却把整列 JSON 读了回来，是一个明知的代价。
// 换来的是「仓储只吐完整聚合」这条边界不被破坏——一旦开始为列表页返回缺字段的
// 半个 Report，调用方就再也无法判断手里这个聚合能不能信。
// 真到了成为瓶颈的那天，正确的解法是给列表单独建投影，而不是让聚合变得半真半假。
func (repo *ReportRepository) ListByUser(ctx context.Context, userID uint64, page shared_vo.Page) ([]*entities.Report, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.ReportDto{}).Where("user_id = ?", userID)
	// Session 固化条件，让 Count 与 Find 复用同一份 where 而不互相污染。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 报告列表", userID)
	}
	if total == 0 {
		return []*entities.Report{}, 0, nil
	}

	var rows []dtos.ReportDto
	err := q.Order("created_at DESC, id DESC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 报告列表", userID)
	}
	return dtos.ToDomainReports(rows), total, nil
}

// Delete 删除一份报告。
//
// ownerID 非 0 时，归属被写进 WHERE 而不是先查后删。
//
// 先 SELECT 出报告、在内存里比对 user_id、再 DELETE，是一个 check-then-act：
// 两步之间报告可能已经被别的请求删掉甚至被重新生成，删除会打在一条本不该删的行上。
// 写成谓词之后，检查与删除是同一个原子操作，零行即表示「这个 id 不属于你或根本不存在」——
// 这两种情况本来就该给出同一个回答。
//
// ownerID 传 0 表示不限制归属，仅供管理员路径使用。
func (repo *ReportRepository) Delete(ctx context.Context, id string, ownerID uint64) error {
	q := repo.db.WithContext(ctx).Where("id = ?", id)
	if ownerID != 0 {
		q = q.Where("user_id = ?", ownerID)
	}

	res := q.Delete(&dtos.ReportDto{})
	if res.Error != nil {
		return translatef(res.Error, "分析报告(id=%s)", id)
	}
	if res.RowsAffected == 0 {
		// 统一返回 NotFound，不区分「不存在」与「不是你的」：
		// Forbidden 等于确认「这个 ID 存在」，会把报告 ID 空间变成可探测的信道。
		return custom_errors.NotFound("分析报告(id=%s) 不存在", id)
	}
	return nil
}
