package repositories

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// purgeChunkSize 是清理过期通知时单条 DELETE 的行数上限。见 PurgeOlderThan。
const purgeChunkSize = 1000

// NotificationRepository 持久化 Notification 聚合。它只碰 notifications 一张表。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
//
// ===========================================================================
// 本仓储的全部设计都围绕一件事：**不做 check-then-act**
// ===========================================================================
//
// 通知的写入路径有两个天然的并发场景，它们都会让「先查再改」失效：
//
//	投递：同一个领域事件被至少一次投递，两次重放可能同时到达。
//	      「先查有没有这条通知，没有就插」的两边都会查到「没有」，然后双双插入。
//	已读：用户在手机和网页同时点开同一条通知。「先读出来看是不是未读，是就改」
//	      的两边都会读到「未读」，然后双双写入，后写的那次覆盖了真正的首次已读时刻。
//
// 因此本层的每个写方法要么是一条带完整谓词的语句，要么是一次依赖唯一索引的裸插入。
// 判定与动作永远在同一条语句里，这比事务更强——它在并发下也成立。
type NotificationRepository struct {
	db *gorm.DB
}

func NewNotificationRepository(db *gorm.DB) *NotificationRepository {
	return &NotificationRepository{db: db}
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// Create 落库一条通知。重复投递时返回 custom_errors.AlreadyExists。
//
// # 为什么是裸 INSERT，没有「先查 dedupe_key 存不存在」
//
// 查完再插是 TOCTOU：两次事件重放并发进来，两边都查到「不存在」，然后都插，
// 用户收到两条一模一样的通知。那次查询挡不住任何东西，只是多一次往返。
// (user_id, dedupe_key) 唯一索引才是真正的保证——它由数据库在行级别串行执行，
// 无论有多少个副本、多少个 worker 同时投递，第二条必然撞键。
//
// # 为什么不是 INSERT ... ON DUPLICATE KEY UPDATE / DO NOTHING
//
// DO NOTHING 会把冲突吞成「成功且什么都没发生」，调用方再也分不清
// 「这是首次投递」还是「这是第 N 次重放」。本上下文需要这个区分：
// 将来接入 App 推送时，只有首次投递才该触发推送，重放不该让用户的手机再响一次。
// 所以这里让冲突如实浮上去，由事件处理器决定「重放是良性的」——
// 那是投递语义的问题，不是持久化的问题。
//
// # 为什么没有事务
//
// 单条 INSERT 本身就是原子的。为了「看起来更安全」把它包进事务，
// 只会平白拉长锁持有时间。
func (repo *NotificationRepository) Create(ctx context.Context, n *entities.Notification) error {
	if n == nil {
		return custom_errors.Invalid("待保存的通知为空")
	}
	dto := dtos.FromDomainNotification(n)
	if err := repo.db.WithContext(ctx).Create(dto).Error; err != nil {
		return translatef(err, "用户(id=%d) 的通知(去重键=%s)", n.UserID, n.DedupeKey.String())
	}
	// 回填自增主键：调用方（事件处理器、接口层）拿它做日志关联与响应体。
	n.ID = dto.ID
	return nil
}

// MarkRead 把一条通知标记为已读。
//
// ===========================================================================
// 不变式写成 WHERE 谓词
// ===========================================================================
//
// 这里刻意不是「加载聚合 → 调 n.MarkRead() → 保存」。那条路径有两个洞：
//
//	归属：加载出来在内存里比对 user_id，是一次 check-then-act。两步之间这条通知
//	      可能已经被删掉，UPDATE 会打在一条本不该动的行上。写进 WHERE 之后，
//	      「改别人的通知」在数据层就是 0 行，越权写入根本不可能发生。
//	幂等：`read_at IS NULL` 让这条语句只在「当前确实未读」时才生效。
//	      两个客户端同时点开同一条通知，只有一个能命中 1 行，另一个命中 0 行，
//	      首次已读时刻因此不会被第二次点击覆盖。
//
// 聚合里的 Notification.MarkRead 并没有因此变成摆设：它是这条规则的**陈述**，
// 保证任何在内存里操作聚合的代码（将来的批量操作、测试）也遵守同一语义；
// 而这里是它在并发下的**执行**。两者说的是同一件事，只有一个真相。
//
// # 0 行的歧义在事务内消解
//
// 0 行可能是「不存在/不是你的」，也可能是「已经读过了」，两者要给出的回答完全不同
// （NotFound 与 Conflict）。多出来的那次读放在**同一个事务**里，是为了让它读到的
// 与 UPDATE 看到的是同一份快照——放在事务外的话，两次之间行可能又被删了，
// 于是用户会收到一个连自己都自相矛盾的错误。正常路径（命中 1 行）不会走到这里。
func (repo *NotificationRepository) MarkRead(ctx context.Context, id, userID uint64, readAt time.Time) error {
	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&dtos.NotificationDto{}).
			Where("id = ? AND user_id = ? AND read_at IS NULL", id, userID).
			Update("read_at", readAt)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}
		return repo.diagnoseMarkRead(tx, id, userID)
	})
	if err != nil {
		return translatef(err, "通知(id=%d)", id)
	}
	return nil
}

// diagnoseMarkRead 把一次条件 UPDATE 的「0 行」收敛成一个确定的答案。
func (repo *NotificationRepository) diagnoseMarkRead(tx *gorm.DB, id, userID uint64) error {
	var row dtos.NotificationDto
	// user_id 同样进 WHERE：不能因为要诊断就把别人的行读出来——
	// 那等于用错误消息确认了「这个 id 存在」。
	err := tx.Where("id = ? AND user_id = ?", id, userID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 「不存在」与「不是你的」给同一个回答：返回 Forbidden 等于确认这个 ID 存在，
			// 会把通知 ID 空间变成可探测的信道。
			return custom_errors.NotFound("通知(id=%d) 不存在", id)
		}
		return err
	}
	// 行在、且已读。语义与聚合里 MarkRead 的判断完全一致。
	return custom_errors.Conflict("通知已是已读状态")
}

// MarkAllRead 把某个用户的全部未读通知标记为已读，返回实际影响的条数。
//
// 一条 UPDATE，不是「先查出未读列表再逐条标记」——后者是 N+1，
// 一个攒了两百条未读的用户会打出两百次往返，而且中途新到的通知还会被漏掉。
//
// `read_at IS NULL` 在这里有两重作用：它让语句只碰真正需要改的行（已读行不会被
// 重写 read_at，首次已读时刻得以保全），也让重复点击「全部已读」天然幂等。
//
// 0 行不是错误：没有未读时点「全部已读」，用户期望的结果就是「现在没有未读了」，
// 而这个状态已经达成。返回条数让接口层可以如实回答「本次标记了几条」。
func (repo *NotificationRepository) MarkAllRead(ctx context.Context, userID uint64, readAt time.Time) (int64, error) {
	res := repo.db.WithContext(ctx).Model(&dtos.NotificationDto{}).
		Where("user_id = ? AND read_at IS NULL", userID).
		Update("read_at", readAt)
	if res.Error != nil {
		return 0, translatef(res.Error, "用户(id=%d) 的未读通知", userID)
	}
	return res.RowsAffected, nil
}

// Delete 删除一条通知。归属是 WHERE 谓词，不是先查后删。
//
// 理由与 MarkRead 相同：先 SELECT 出来、在内存里比对 user_id、再 DELETE，
// 两步之间这条通知可能已经被另一个请求删掉，DELETE 会打在一条本不该删的行上。
// 写成谓词之后检查与删除是同一个原子操作，0 行即表示「不存在或不属于你」——
// 这两种情况本来就该给出同一个回答。
func (repo *NotificationRepository) Delete(ctx context.Context, id, userID uint64) error {
	res := repo.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, userID).
		Delete(&dtos.NotificationDto{})
	if res.Error != nil {
		return translatef(res.Error, "通知(id=%d)", id)
	}
	if res.RowsAffected == 0 {
		return custom_errors.NotFound("通知(id=%d) 不存在", id)
	}
	return nil
}

// PurgeOlderThan 清理 cutoff 之前创建的通知，返回删除条数。
//
// # 为什么连未读的一起删
//
// 通知是有保质期的：一条三个月前的「分析完成」即使没被读过，也不会再有人去读，
// 留着只会让未读红点永远消不掉，反而把真正需要关注的新通知淹掉。
// 保留期由调用方（清理类定时任务）给出，本层不替它决定多久算过期。
//
// # 为什么要分批
//
// 一条不带 LIMIT 的 DELETE 在这张表上是真实的生产事故来源：它可能一次删掉几百万行，
// 持有大量行锁直到提交，把 undo log 撑大，并且让同时在写入通知的事件处理器全部排队。
// 分批之后每条语句的工作量有上限，批与批之间锁会释放，正常写入得以穿插进行。
//
// 这里的循环不违反「不在循环里查询」——那条规则针对的是按集合元素逐个往返的 N+1。
// 本方法的往返次数取决于待删数据量而不是某个集合的长度，且每次往返都删掉一整批，
// 这正是把一次无界操作切成有界操作的标准做法。
func (repo *NotificationRepository) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		// 每一批都重新检查 ctx：清理任务可能跑很久，进程退出时要能立刻停下，
		// 而不是把剩下的批次全部跑完。已经删掉的批次不会回滚，也不需要回滚——
		// 清理是幂等的，下一轮会从剩下的行继续。
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res := repo.db.WithContext(ctx).
			Where("created_at < ?", cutoff).
			Limit(purgeChunkSize).
			Delete(&dtos.NotificationDto{})
		if res.Error != nil {
			return total, translatef(res.Error, "%s 之前的过期通知", cutoff.Format(time.RFC3339))
		}
		total += res.RowsAffected
		// 不足一批说明已经删完了。
		if res.RowsAffected < purgeChunkSize {
			return total, nil
		}
	}
}

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// ListByUser 分页列出某用户的通知。status 为零值表示不限已读状态。
//
// user_id 是查询条件而不是事后过滤：通知是个人数据，连管理员也没有「看别人通知」
// 这个用例，因此这里根本没有一个能查到别人数据的参数组合。
//
// 已读筛选翻译成 read_at IS [NOT] NULL，而不是去比对一个 read 布尔列：
// 库里只有 read_at 一个真相来源，见 dtos.NotificationDto。
func (repo *NotificationRepository) ListByUser(
	ctx context.Context,
	userID uint64,
	status value_objects.ReadStatus,
	page shared_vo.Page,
) ([]*entities.Notification, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.NotificationDto{}).Where("user_id = ?", userID)
	switch status {
	case value_objects.StatusUnread:
		q = q.Where("read_at IS NULL")
	case value_objects.StatusRead:
		q = q.Where("read_at IS NOT NULL")
	}
	// Session 固化条件，让 Count 与 Find 复用同一份 where 而不互相污染。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 的通知列表", userID)
	}
	if total == 0 {
		return []*entities.Notification{}, 0, nil
	}

	var rows []dtos.NotificationDto
	// 补 id 做 tie-break：同一批事件（批量分析收尾）会在同一毫秒产出多条通知，
	// 只按 created_at 排的话，翻页时它们的先后可能变化，第 2 页会重复出现第 1 页的行。
	err := q.Order("created_at DESC, id DESC").
		Offset(page.Offset()).Limit(page.Limit()).
		Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 的通知列表", userID)
	}
	return dtos.ToDomainNotifications(rows), total, nil
}

// CountUnread 统计未读条数，即前端那个红点上的数字。
//
// 这是全站最高频的查询之一（每次进首页、每次轮询都会打一次），因此它必须只走索引：
// WHERE user_id = ? AND read_at IS NULL 正好命中 idx_notifications_user_read_created
// 的前两列，不需要回表读任何一行数据。
//
// 不做「查出未读列表再 len()」：那会把一个索引上的计数变成一次全字段读取，
// 而调用方只想要一个数字。
func (repo *NotificationRepository) CountUnread(ctx context.Context, userID uint64) (int64, error) {
	var n int64
	err := repo.db.WithContext(ctx).Model(&dtos.NotificationDto{}).
		Where("user_id = ? AND read_at IS NULL", userID).
		Count(&n).Error
	if err != nil {
		return 0, translatef(err, "用户(id=%d) 的未读通知数", userID)
	}
	return n, nil
}
