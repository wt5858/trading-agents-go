package repositories

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

// ===========================================================================
// 单次触发只会被一个副本抢到
// ===========================================================================
//
// 这是本上下文唯一真正难的性质，因此分两层来测：
//
//  1. TestClaimProtocol_OnlyOneReplicaWins —— 纯内存，验证**协议本身**。
//     它把 ClaimDue 里那条 CAS 语句的语义（`WHERE id=? AND next_run_at=?`
//     命中 1 行才算赢）在内存里精确复刻一遍，并让两个抢占者在同一时刻真正并发。
//     这一层不需要数据库，因此它在 CI 上每次都会跑。
//
//  2. TestClaimDue_LiveMySQL —— 打到真库，验证**仓储的实现**确实遵循该协议。
//     它需要一个可写的 MySQL，因此默认跳过；设置 TA_TEST_MYSQL_DSN 即可启用。
//     之所以没用 sqlmock 替代：sqlmock 是按预期语句顺序回放的桩，
//     它只能证明「我们发出了这条 SQL」，证明不了「数据库会让其中一条命中 0 行」——
//     而后者恰恰是本设计全部保证的来源。用它来测这个性质是自欺。

// ---------------------------------------------------------------------------
// 一、协议层
// ---------------------------------------------------------------------------

// fakeJobRow 是 scheduled_jobs 表里一行的内存替身。
//
// 它只需要建模一件事：**数据库对同一行的 UPDATE 是串行的**。
// 这里用一把互斥锁表达这条保证——真实数据库靠的是行锁，机制不同，语义相同。
type fakeJobRow struct {
	mu        sync.Mutex
	id        string
	nextRunAt time.Time
}

// snapshot 对应 ClaimDue 的第一步：SELECT 出候选。
// 它**不提供任何保证**——两个副本会读到一模一样的值，这正是问题所在。
func (r *fakeJobRow) snapshot() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nextRunAt
}

// conditionalUpdate 精确复刻 ClaimDue 的第三步：
//
//	UPDATE scheduled_jobs SET next_run_at = ? WHERE id = ? AND next_run_at = ?
//
// 返回 RowsAffected。只有返回 1 才代表「这个副本赢下了这一次触发」。
func (r *fakeJobRow) conditionalUpdate(id string, expected, next time.Time) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.id != id || !r.nextRunAt.Equal(expected) {
		return 0
	}
	r.nextRunAt = next
	return 1
}

// gate 是一个 N 方屏障：所有参与者都到齐之后才一起放行。
//
// 有了它，「两个副本都读到了旧值，然后才各自去写」这个竞态不再依赖调度运气，
// 而是每次运行都必然发生。没有屏障的话，测试可能碰巧串行执行而永远通过，
// 那种测试对这个性质毫无价值。
type gate struct {
	mu      sync.Mutex
	parties int
	arrived int
	open    chan struct{}
}

func newGate(parties int) *gate {
	return &gate{parties: parties, open: make(chan struct{})}
}

func (g *gate) wait() {
	g.mu.Lock()
	g.arrived++
	if g.arrived == g.parties {
		close(g.open)
	}
	g.mu.Unlock()
	<-g.open
}

func TestClaimProtocol_OnlyOneReplicaWins(t *testing.T) {
	const replicas = 2

	spec, err := value_objects.NewCronExpression("* * * * *")
	if err != nil {
		t.Fatalf("构造 cron 表达式失败: %v", err)
	}

	// 这一次触发的计划时刻已经过去，两个副本都会把它判成到期。
	scheduledFor := time.Now().Truncate(time.Minute).Add(-time.Minute)
	now := scheduledFor.Add(time.Second)

	row := &fakeJobRow{id: "job_race", nextRunAt: scheduledFor}
	barrier := newGate(replicas)

	// 每个副本跑一遍 ClaimDue 的三步：读候选 -> 让聚合推进 -> CAS。
	// 用 concurrency.Settle 而不是裸 goroutine：这是本服务唯一被认可的扇出方式，
	// 测试也不例外。Settle 的容忍语义在这里正合适——「输了」不是错误。
	claim := func(ctx context.Context, replica int) (bool, error) {
		// 第一步：读候选。此刻两个副本会看到同一个 next_run_at。
		expected := row.snapshot()

		// 第二步：让聚合决定「下次是什么时候」。这是抢占的内存半场，
		// 单独的覆盖见 entities/scheduled_job_test.go。
		job := &entities.ScheduledJob{
			ID:        row.id,
			Kind:      value_objects.JobKindMarketSync,
			Cron:      spec,
			Status:    value_objects.JobStatusEnabled,
			NextRunAt: expected,
		}
		if _, err := job.ClaimOccurrence(now); err != nil {
			return false, err
		}

		// 屏障：确保两个副本都读完、都算完，然后同时去写。
		barrier.wait()

		// 第三步：CAS。RowsAffected == 1 是赢下这一次触发的唯一证据。
		return row.conditionalUpdate(row.id, expected, job.NextRunAt) == 1, nil
	}

	outcomes, err := concurrency.Settle(context.Background(),
		[]int{0, 1}, replicas, claim)
	if err != nil {
		t.Fatalf("扇出失败: %v", err)
	}

	winners := 0
	for i, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("副本 %d 抢占过程出错: %v", i, o.Err)
		}
		if o.Value {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("同一次触发必须恰好被一个副本抢到，实际 %d 个", winners)
	}

	// 输的那个副本不该留下任何痕迹：行里的 next_run_at 只被推进了一次。
	if !row.snapshot().After(now) {
		t.Fatalf("抢占成功后 next_run_at 必须推进到 now 之后，实际 %s", row.snapshot())
	}
}

// TestClaimProtocol_LosersCannotResurrectTheOccurrence 补一个反向断言：
// 输掉的副本即便拿着旧的 expected 值重试，也永远不可能再赢。
// 这正是「把检查写进 WHERE 谓词」相对于 check-then-act 的全部价值。
func TestClaimProtocol_LosersCannotResurrectTheOccurrence(t *testing.T) {
	scheduledFor := time.Now().Add(-time.Minute)
	row := &fakeJobRow{id: "job_x", nextRunAt: scheduledFor}

	first := row.conditionalUpdate("job_x", scheduledFor, scheduledFor.Add(time.Minute))
	if first != 1 {
		t.Fatalf("首个抢占者应当命中 1 行，实际 %d", first)
	}
	for i := 0; i < 5; i++ {
		if again := row.conditionalUpdate("job_x", scheduledFor, scheduledFor.Add(time.Minute)); again != 0 {
			t.Fatalf("持旧值重试必须永远命中 0 行，第 %d 次实际 %d", i+1, again)
		}
	}
}

// ---------------------------------------------------------------------------
// 二、仓储层（需要真库）
// ---------------------------------------------------------------------------

// TestClaimDue_LiveMySQL 用真实的 MySQL 验证 ClaimDue 的实现遵循上面的协议。
//
// 默认跳过。启用方式：
//
//	TA_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/ta_test?parseTime=true&loc=Local' \
//	  go test ./internal/bounded_contexts/scheduling/...
//
// 必须是真库而不是任何桩：本设计的保证来自「数据库对同一行的 UPDATE 串行执行」
// 这条数据库自身的性质，任何模拟层都只是在重复我们自己的假设。
func TestClaimDue_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的抢占测试")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&dtos.ScheduledJobDto{}, &dtos.JobExecutionDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	repo := NewScheduledJobRepository(db)
	ctx := context.Background()

	spec, err := value_objects.NewCronExpression("* * * * *")
	if err != nil {
		t.Fatalf("构造 cron 失败: %v", err)
	}
	job, err := entities.Schedule(
		"job_live_race", "并发抢占测试", value_objects.JobKindMarketSync,
		spec, value_objects.JobPayload{}, 3, time.Minute, 1,
	)
	if err != nil {
		t.Fatalf("构造任务失败: %v", err)
	}
	// 把计划触发时刻拨到过去，让它立刻到期。
	job.NextRunAt = time.Now().Add(-time.Minute)

	_ = repo.Delete(ctx, job.ID)
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), job.ID) })

	now := time.Now()
	// 两个「副本」同时跑一轮 sweep。
	outcomes, err := concurrency.Settle(ctx, []int{0, 1}, 2,
		func(ctx context.Context, _ int) (int, error) {
			claimed, err := repo.ClaimDue(ctx, now, 10)
			return len(claimed), err
		})
	if err != nil {
		t.Fatalf("扇出失败: %v", err)
	}

	total := 0
	for i, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("副本 %d 抢占失败: %v", i, o.Err)
		}
		total += o.Value
	}
	if total != 1 {
		t.Fatalf("同一次触发必须恰好被抢到一次，实际 %d 次", total)
	}

	// 再扫一轮：next_run_at 已经推进到未来，谁都不该再抢到。
	again, err := repo.ClaimDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("二次抢占出错: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("已抢占过的触发不该被再次捞出，实际 %d 条", len(again))
	}
}
