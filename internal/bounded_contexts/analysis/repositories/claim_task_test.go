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

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

// ===========================================================================
// 同一个任务只会被一个消费者认领
// ===========================================================================
//
// 这是迁到消息队列之后唯一真正难的性质：投递是至少一次的，消息可能在前一个
// 消费者还在跑的时候被重投，而一次分析是十几轮 LLM 调用——跑两遍就是账单付两遍。
//
// 分两层测，与 scheduling 上下文的 claim_due_test.go 同构：
//
//  1. TestTaskClaimProtocol_* —— 纯内存，验证**协议本身**：
//     `WHERE id=? AND status='queued'` 命中 1 行才算赢。不需要数据库，CI 每次都跑。
//  2. TestClaimTask_LiveMySQL —— 打真库，验证**实现**确实遵循该协议。
//     默认跳过，设置 TA_TEST_MYSQL_DSN 启用。
//
// 不用 sqlmock 替代第二层：它只能证明「我们发出了这条 SQL」，证明不了
// 「数据库会让其中一条命中 0 行」——而后者才是全部保证的来源。

// ---------------------------------------------------------------------------
// 一、协议层
// ---------------------------------------------------------------------------

// fakeTaskRow 是 analysis_tasks 表里一行的内存替身。
// 它只建模一件事：数据库对同一行的 UPDATE 是串行的。这里用互斥锁表达，
// 真实数据库靠行锁——机制不同，语义相同。
type fakeTaskRow struct {
	mu     sync.Mutex
	id     string
	status string
}

// snapshot 对应 ClaimTask 的第一步：SELECT 出候选。
// 它**不提供任何保证**——两个消费者会读到一模一样的 queued，这正是问题所在。
func (r *fakeTaskRow) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// conditionalUpdate 复刻 ClaimTask 的第三步：
//
//	UPDATE analysis_tasks SET status='running' WHERE id=? AND status='queued'
//
// 返回 RowsAffected。只有 1 才代表这个消费者赢下了这个任务。
func (r *fakeTaskRow) conditionalUpdate(id, expected, next string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.id != id || r.status != expected {
		return 0
	}
	r.status = next
	return 1
}

// gate 是一个 N 方屏障：所有参与者到齐后才一起放行。
// 有了它，「两个消费者都读到 queued，然后才各自去写」这个竞态不再依赖调度运气，
// 而是每次运行都必然发生。
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

func TestTaskClaimProtocol_OnlyOneConsumerWins(t *testing.T) {
	const consumers = 2

	row := &fakeTaskRow{id: "task_race", status: value_objects.StatusQueued.String()}
	barrier := newGate(consumers)

	// 每个消费者跑一遍 ClaimTask 的三步：读候选 -> 让聚合迁移 -> 条件写。
	// 用 concurrency.Settle 而不是裸 goroutine：这是本服务唯一被认可的扇出方式，
	// 测试也不例外。「输了」不是错误，正合 Settle 的容忍语义。
	claim := func(ctx context.Context, consumer int) (bool, error) {
		// 第一步：读候选。此刻两个消费者都会看到 queued。
		expected := row.snapshot()

		// 第二步：让聚合完成迁移。这是认领的内存半场，它本身不提供任何互斥。
		task := &entities.Task{ID: row.id, Status: value_objects.Status(expected)}
		if err := task.Start(); err != nil {
			return false, err
		}

		// 屏障：确保两个消费者都读完、都算完，然后同时去写。
		barrier.wait()

		// 第三步：条件写。RowsAffected == 1 是赢下这个任务的唯一证据。
		return row.conditionalUpdate(row.id, expected, task.Status.String()) == 1, nil
	}

	outcomes, err := concurrency.Settle(context.Background(), []int{0, 1}, consumers, claim)
	if err != nil {
		t.Fatalf("扇出失败: %v", err)
	}

	winners := 0
	for i, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("消费者 %d 认领过程出错: %v", i, o.Err)
		}
		if o.Value {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("同一个任务必须恰好被一个消费者认领，实际 %d 个", winners)
	}
	if got := row.snapshot(); got != value_objects.StatusRunning.String() {
		t.Fatalf("认领成功后状态应为 running，实际 %s", got)
	}
}

// 输掉的消费者即便拿着旧的 queued 重试，也永远不可能再赢。
// 这正是「把检查写进 WHERE 谓词」相对于 check-then-act 的全部价值。
func TestTaskClaimProtocol_LosersCannotReclaim(t *testing.T) {
	queued := value_objects.StatusQueued.String()
	running := value_objects.StatusRunning.String()
	row := &fakeTaskRow{id: "task_x", status: queued}

	if first := row.conditionalUpdate("task_x", queued, running); first != 1 {
		t.Fatalf("首个认领者应当命中 1 行，实际 %d", first)
	}
	for i := 0; i < 5; i++ {
		if again := row.conditionalUpdate("task_x", queued, running); again != 0 {
			t.Fatalf("持旧值重试必须永远命中 0 行，第 %d 次实际 %d", i+1, again)
		}
	}
}

// ---------------------------------------------------------------------------
// 二、仓储层（需要真库）
// ---------------------------------------------------------------------------

// TestClaimTask_LiveMySQL 用真实 MySQL 验证 ClaimTask 的实现遵循上面的协议。
//
// 默认跳过。启用方式：
//
//	TA_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:33061)/ta_test?parseTime=true&loc=Local' \
//	  go test ./internal/bounded_contexts/analysis/...
func TestClaimTask_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的认领测试")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&dtos.TaskDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	repo := NewTaskRepository(db)
	ctx := context.Background()

	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	tradeDate, err := shared_vo.NewTradeDate(time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("构造交易日失败: %v", err)
	}
	req, err := value_objects.NewRequest(code, tradeDate, value_objects.DepthQuick, nil, "")
	if err != nil {
		t.Fatalf("构造分析请求失败: %v", err)
	}
	task, err := entities.NewTask("task_live_claim_race", 1, req)
	if err != nil {
		t.Fatalf("构造任务失败: %v", err)
	}

	db.Where("id = ?", task.ID).Delete(&dtos.TaskDto{})
	if err := repo.Save(ctx, task); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	t.Cleanup(func() { db.Where("id = ?", task.ID).Delete(&dtos.TaskDto{}) })

	// 两个「消费者」同时认领同一个任务。
	outcomes, err := concurrency.Settle(ctx, []int{0, 1}, 2,
		func(ctx context.Context, _ int) (bool, error) {
			claimed, err := repo.ClaimTask(ctx, task.ID)
			return claimed != nil, err
		})
	if err != nil {
		t.Fatalf("扇出失败: %v", err)
	}

	winners := 0
	for i, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("消费者 %d 认领失败: %v", i, o.Err)
		}
		if o.Value {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("同一个任务必须恰好被认领一次，实际 %d 次", winners)
	}

	// 再认领一轮：状态已是 running，谁都不该再抢到。
	again, err := repo.ClaimTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("二次认领出错: %v", err)
	}
	if again != nil {
		t.Fatalf("已被认领的任务不该被再次认领到")
	}

	// 认领只发生一次，因此 attempts 必须恰好是 1——它是用户可见的「已尝试 N 次」。
	var row dtos.TaskDto
	if err := db.Where("id = ?", task.ID).First(&row).Error; err != nil {
		t.Fatalf("回读任务失败: %v", err)
	}
	if row.Attempts != 1 {
		t.Fatalf("认领一次后 attempts 应为 1，实际 %d", row.Attempts)
	}
	if row.Status != value_objects.StatusRunning.String() {
		t.Fatalf("认领后状态应为 running，实际 %s", row.Status)
	}
}

// TestListStale_LiveMySQL 验证停滞巡检的谓词。
//
// 重点是**两个阈值不能串**：一个刚认领、正在健康执行的任务，绝不能因为
// queued 的宽限期已过就被捞出来判死——那等于杀掉一个正在跑的付费任务，
// 恰恰是这套机制要避免的事。
//
// 默认跳过，启用方式同上。
func TestListStale_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的停滞巡检测试")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&dtos.TaskDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	repo := NewTaskRepository(db)
	ctx := context.Background()

	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	tradeDate, err := shared_vo.NewTradeDate(time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("构造交易日失败: %v", err)
	}
	req, err := value_objects.NewRequest(code, tradeDate, value_objects.DepthQuick, nil, "")
	if err != nil {
		t.Fatalf("构造分析请求失败: %v", err)
	}

	now := time.Now()
	// 三条任务，覆盖「该捞」「该捞」「绝不能捞」。
	fixtures := []struct {
		id             string
		status         value_objects.Status
		stateChangedAt time.Time
		wantStale      bool
		why            string
	}{
		{"task_stale_queued", value_objects.StatusQueued, now.Add(-time.Hour), true,
			"排队一小时没人动，消息八成丢了"},
		{"task_stale_running", value_objects.StatusRunning, now.Add(-time.Hour), true,
			"跑了一小时还没结局，消费者八成死了"},
		{"task_healthy_running", value_objects.StatusRunning, now.Add(-time.Minute), false,
			"刚认领一分钟，正在健康执行——捞它就是杀掉一个正在跑的付费任务"},
	}

	ids := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		task, err := entities.NewTask(f.id, 1, req)
		if err != nil {
			t.Fatalf("构造任务 %s 失败: %v", f.id, err)
		}
		ids = append(ids, f.id)
		db.Where("id = ?", f.id).Delete(&dtos.TaskDto{})
		if err := repo.Save(ctx, task); err != nil {
			t.Fatalf("写入任务 %s 失败: %v", f.id, err)
		}
		// 直接改列而不走聚合：这里要造的是「已经卡了很久」的既成事实，
		// 而聚合的迁移方法一定会把时间戳刷成现在。
		if err := db.Model(&dtos.TaskDto{}).Where("id = ?", f.id).
			Updates(map[string]any{
				"status":           f.status.String(),
				"state_changed_at": f.stateChangedAt,
			}).Error; err != nil {
			t.Fatalf("构造停滞状态失败 %s: %v", f.id, err)
		}
	}
	t.Cleanup(func() { db.Where("id IN ?", ids).Delete(&dtos.TaskDto{}) })

	// 与生产一致的两个宽限期。
	stale, err := repo.ListStale(ctx,
		now.Add(-10*time.Minute), // queuedBefore
		now.Add(-30*time.Minute), // runningBefore
		50)
	if err != nil {
		t.Fatalf("查询卡住的任务失败: %v", err)
	}

	got := make(map[string]bool, len(stale))
	for _, s := range stale {
		got[s.ID] = true
	}
	for _, f := range fixtures {
		if got[f.id] != f.wantStale {
			t.Fatalf("任务 %s 期望 stale=%v 实际 %v（%s）", f.id, f.wantStale, got[f.id], f.why)
		}
	}
}
