package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"

	agent_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/repositories"
	screening_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/screening/repositories"
	stock_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/db/migrations"
)

// migrationDir 是 embed.FS 内部的相对路径。脚本就在包根目录下，用 "." 表示。
const migrationDir = "."

// setupGoose 把 goose 指向内嵌的脚本集合。
//
// 每次调用都重设一遍是刻意的：goose 的这些设置是包级全局状态，
// 若某处被别的组件改过（测试里尤其常见），不重设就会跑到错误的目录去。
func setupGoose(log *zap.Logger) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("schema_migrations")
	goose.SetLogger(&gooseZapLogger{log: log})
	if err := goose.SetDialect("mysql"); err != nil {
		return fmt.Errorf("设置 goose 方言失败: %w", err)
	}
	return nil
}

// sqlDB 取出底层 *sql.DB。goose 不认识 GORM，只接受标准库连接。
func (c *Connections) sqlDB() (*sql.DB, error) {
	db, err := c.MySQL.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层数据库连接失败: %w", err)
	}
	return db, nil
}

// MigrateUp 执行全部未应用的迁移。
//
// 用 goose 而不是 GORM 的 AutoMigrate：AutoMigrate 只会加列、不会删列也不会改类型，
// 表结构因此会在「代码以为的样子」和「数据库实际的样子」之间静默漂移，
// 而漂移只有在某天读到 NULL 或截断数据时才暴露。显式迁移脚本还带来两样东西：
// 可评审的变更历史，以及可回滚的 Down。
func (c *Connections) MigrateUp(ctx context.Context, log *zap.Logger) error {
	if err := setupGoose(log); err != nil {
		return err
	}
	db, err := c.sqlDB()
	if err != nil {
		return err
	}

	before, _ := goose.GetDBVersionContext(ctx, db)
	if err := goose.UpContext(ctx, db, migrationDir); err != nil {
		return fmt.Errorf("执行数据库迁移失败: %w", err)
	}
	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("读取迁移版本失败: %w", err)
	}

	if after == before {
		log.Info("数据库表结构已是最新", zap.Int64("version", after))
	} else {
		log.Info("数据库迁移完成", zap.Int64("from", before), zap.Int64("to", after))
	}
	return nil
}

// bootMigrationLock 是启动期迁移用的建议锁名称。
//
// 用 MySQL 的具名锁而不是某张表里的一行：具名锁在持有它的**连接**断开时自动释放。
// 一个副本在迁移途中被 kill -9，锁会随连接一起消失；换成锁表的话，
// 那一行会永远留在那儿，后续所有副本都启动不了，而这通常发生在最不该发生的时候。
const bootMigrationLock = "trading_agents_schema_migration"

// bootMigrationLockTimeout 是等待锁的上限。
//
// 它要大于「一次最慢的迁移」而不是「一次典型的迁移」：滚动发布时后启动的副本
// 要等先启动的那个把表改完，而一次加索引的迁移在大表上跑几分钟是正常的。
// 超时之后启动失败，这是对的——带着一个还没改完的表结构开始服务请求更糟。
const bootMigrationLockTimeout = 5 * time.Minute

// MigrateOnBoot 在服务启动时把表结构推到最新。
//
// ===========================================================================
// 为什么敢在启动时自动迁移
// ===========================================================================
//
// 因为「代码和它需要的表结构必须同时到位」这件事，只有进程自己最清楚。
// 交给一个独立的运维步骤，就等于引入了一个人工同步点：忘了跑、跑错了顺序、
// 或者在滚动发布中途跑，都会让新代码撞上旧表结构，而那类故障的第一现场
// 往往是一条看不懂的 SQL 报错。
//
// ===========================================================================
// 多副本同时启动怎么办
// ===========================================================================
//
// 这是这套做法唯一真正的风险，而**进程内的 sync.Once 对它毫无帮助**——
// 它只保证一个进程里跑一次，N 个副本就是 N 个进程，N 个 sync.Once。
// 三个副本同时执行同一条 CREATE TABLE，得到的是两个报错和一个成功，
// 或者更糟：两条 ALTER 交错执行。
//
// 所以这里用数据库自己的具名锁把并发收敛掉：拿到锁的那个副本跑迁移，
// 其余的排队等待，等到之后发现已经是最新版本，直接往下走。
// 保证来自数据库，跨进程、跨机器都成立——这正是 sync.Once 给不了的。
func (c *Connections) MigrateOnBoot(ctx context.Context, log *zap.Logger) error {
	db, err := c.sqlDB()
	if err != nil {
		return err
	}

	// 锁必须在同一条连接上获取与释放，否则 RELEASE_LOCK 会作用在别的连接上
	// （而它会安静地返回 NULL，什么也不做）。连接池默认不保证这一点，
	// 所以这里显式取一条连接并全程持有。
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取迁移锁的连接失败: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var acquired sql.NullInt64
	err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)",
		bootMigrationLock, int(bootMigrationLockTimeout.Seconds())).Scan(&acquired)
	if err != nil {
		return fmt.Errorf("获取迁移锁失败: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		// 等满了还没拿到。不硬着头皮往下跑：此刻另一个副本正在改表，
		// 带着一个不确定的表结构开始服务请求，比启动失败危险得多。
		return fmt.Errorf("等待数据库迁移锁超过 %s，另一个副本可能仍在迁移", bootMigrationLockTimeout)
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT RELEASE_LOCK(?)", bootMigrationLock); err != nil {
			// 释放失败不影响正确性：连接关闭时锁会自动释放。
			log.Warn("释放数据库迁移锁失败，连接关闭时会自动释放", zap.Error(err))
		}
	}()

	return c.MigrateUp(ctx, log)
}

// MigrateDown 回滚最近一次迁移。仅供开发与应急使用。
func (c *Connections) MigrateDown(ctx context.Context, log *zap.Logger) error {
	if err := setupGoose(log); err != nil {
		return err
	}
	db, err := c.sqlDB()
	if err != nil {
		return err
	}
	if err := goose.DownContext(ctx, db, migrationDir); err != nil {
		return fmt.Errorf("回滚迁移失败: %w", err)
	}
	return nil
}

// MigrateTo 迁移到指定版本，可升可降。
func (c *Connections) MigrateTo(ctx context.Context, version int64, log *zap.Logger) error {
	if err := setupGoose(log); err != nil {
		return err
	}
	db, err := c.sqlDB()
	if err != nil {
		return err
	}
	if err := goose.UpToContext(ctx, db, migrationDir, version); err != nil {
		return fmt.Errorf("迁移到版本 %d 失败: %w", version, err)
	}
	return nil
}

// MigrationStatus 打印各迁移脚本的应用情况。
func (c *Connections) MigrationStatus(ctx context.Context, log *zap.Logger) error {
	if err := setupGoose(log); err != nil {
		return err
	}
	db, err := c.sqlDB()
	if err != nil {
		return err
	}
	if err := goose.StatusContext(ctx, db, migrationDir); err != nil {
		return fmt.Errorf("查询迁移状态失败: %w", err)
	}
	return nil
}

// MigrationVersion 返回当前数据库版本。
func (c *Connections) MigrationVersion(ctx context.Context) (int64, error) {
	if err := setupGoose(zap.NewNop()); err != nil {
		return 0, err
	}
	db, err := c.sqlDB()
	if err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, db)
}

// EnsureMongoIndexes 建立 Mongo 侧索引。
//
// Mongo 不走 goose：它没有表结构可迁移，索引是幂等声明——
// createIndex 对已存在且定义相同的索引是空操作，每次启动跑一遍即可。
//
// 索引由各上下文自己声明：行情/资讯的自然键唯一索引属于 stock，
// 指标快照的 (symbol, period, trade_date) 唯一索引属于 agent。
// 这些唯一索引不只是查询优化——它们是同步任务反复拉取重叠窗口时
// 能保持幂等的唯一保证。
func (c *Connections) EnsureMongoIndexes(ctx context.Context, log *zap.Logger) error {
	if c.Mongo == nil {
		log.Warn("未启用 MongoDB，跳过索引创建；行情与指标相关功能将不可用")
		return nil
	}
	if err := stock_repo.EnsureIndexes(ctx, c.Mongo); err != nil {
		return fmt.Errorf("创建 stock 索引失败: %w", err)
	}
	if err := agent_repo.NewIndicatorRepository(c.Mongo).EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("创建 agent 指标索引失败: %w", err)
	}
	// 选股筛选依赖以 trade_date 打头的索引：没有它，「取最新一个交易日的横截面」
	// 会退化成全集合扫描。
	if err := screening_repo.EnsureScreeningIndexes(ctx, c.Mongo); err != nil {
		return fmt.Errorf("创建 screening 索引失败: %w", err)
	}
	log.Info("MongoDB 索引已就绪")
	return nil
}

// gooseZapLogger 把 goose 的输出并进 zap，避免服务里出现两套日志格式。
type gooseZapLogger struct{ log *zap.Logger }

func (l *gooseZapLogger) Fatalf(format string, v ...any) {
	// 不真的 os.Exit：迁移失败要沿调用栈返回，由 main 决定退出码与清理动作。
	l.log.Error("goose: " + fmt.Sprintf(format, v...))
}

func (l *gooseZapLogger) Printf(format string, v ...any) {
	l.log.Info("goose: " + fmt.Sprintf(format, v...))
}
