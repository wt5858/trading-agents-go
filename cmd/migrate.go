package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/db"
)

// MigrateCmd 管理数据库表结构迁移。
//
// 独立成命令而不是塞进服务启动流程：迁移是需要人确认的动作。
// 让它随服务自动跑，意味着一次滚动发布会有 N 个副本同时改表结构，
// 而且出问题时没有任何人为介入的时机。
var MigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "管理数据库表结构迁移",
}

// withMySQL 只连 MySQL 就把活干完。
//
// 不走 di.Build：那会连上 Mongo、Redis 和消息队列，让一次表结构变更平白多出
// 三个可能失败的依赖——而「迁移跑不了」的原因是「RabbitMQ 没起来」，
// 大概是运维最不想在凌晨看到的一条报错。
func withMySQL(fn func(ctx context.Context, conns *db.Connections, log *zap.Logger) error) error {
	cfg, log, err := bootstrap()
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	ctx := context.Background()
	conns, err := db.OpenMySQLOnly(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer func() { _ = conns.Close(ctx) }()

	if err := fn(ctx, conns, log); err != nil {
		log.Error("迁移失败", zap.Error(err))
		return err
	}
	return nil
}

var migrateUpCmd = &cobra.Command{
	Use:   "up",
	Short: "应用全部未执行的迁移",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withMySQL(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			return c.MigrateUp(ctx, log)
		})
	},
}

var migrateDownCmd = &cobra.Command{
	Use:   "down",
	Short: "回滚最近一次迁移",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withMySQL(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			return c.MigrateDown(ctx, log)
		})
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看各迁移脚本的应用情况",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withMySQL(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			return c.MigrationStatus(ctx, log)
		})
	},
}

var migrateVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "打印当前数据库版本",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withMySQL(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			v, err := c.MigrationVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("当前数据库版本: %d\n", v)
			return nil
		})
	},
}

var migrateUpToCmd = &cobra.Command{
	Use:   "up-to <version>",
	Short: "迁移到指定版本",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		version, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("版本号必须是数字: %s", args[0])
		}
		return withMySQL(func(ctx context.Context, c *db.Connections, log *zap.Logger) error {
			return c.MigrateTo(ctx, version, log)
		})
	},
}

func init() {
	MigrateCmd.AddCommand(migrateUpCmd, migrateDownCmd, migrateStatusCmd, migrateVersionCmd, migrateUpToCmd)
	RootCmd.AddCommand(MigrateCmd)
}
