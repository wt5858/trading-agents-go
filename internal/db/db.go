// Package db 负责建立并持有基础设施连接：MySQL、MongoDB、Redis。
//
// 本包只做连接与建表，不含任何业务语义——仓储实现住在各自的限界上下文里。
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/config"
)

// Connections 汇总全部基础设施句柄，由组装根持有、进程退出时统一关闭。
type Connections struct {
	MySQL *gorm.DB
	Mongo *mongo.Database
	Redis *redis.Client

	mongoClient *mongo.Client
}

// Open 建立全部连接。任一失败即返回错误——带着半条腿启动的服务，
// 会在第一个请求打进来时才暴露问题，比启动失败难排查得多。
func Open(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Connections, error) {
	gdb, err := openMySQL(cfg, log)
	if err != nil {
		return nil, err
	}

	conns := &Connections{MySQL: gdb}

	if cfg.Mongo.Enabled {
		client, database, err := openMongo(ctx, cfg.Mongo, cfg.Log, log)
		if err != nil {
			_ = conns.Close(ctx)
			return nil, err
		}
		conns.mongoClient = client
		conns.Mongo = database
	}

	rdb, err := openRedis(ctx, cfg.Redis, cfg.Log, log)
	if err != nil {
		_ = conns.Close(ctx)
		return nil, err
	}
	conns.Redis = rdb

	return conns, nil
}

// OpenMySQLOnly 只建立 MySQL 连接，供迁移命令使用。
//
// 迁移不该因为 Redis 或 Mongo 不可用而失败：那是两个与表结构毫不相干的依赖，
// 把它们拉进来只会增加一次表结构变更的失败面。
func OpenMySQLOnly(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Connections, error) {
	gdb, err := openMySQL(cfg, log)
	if err != nil {
		return nil, err
	}
	return &Connections{MySQL: gdb}, nil
}

func (c *Connections) Close(ctx context.Context) error {
	var firstErr error
	if c.Redis != nil {
		if err := c.Redis.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.mongoClient != nil {
		if err := c.mongoClient.Disconnect(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if c.MySQL != nil {
		if sqlDB, err := c.MySQL.DB(); err == nil {
			if err := sqlDB.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// openMySQL 收整个 *config.Config 而不只是 config.MySQL：日志开关归 config.Log 管，
// 而它要落到 GORM 的日志器上。
func openMySQL(cfg *config.Config, log *zap.Logger) (*gorm.DB, error) {
	gdb, err := gorm.Open(mysql.Open(cfg.MySQL.DSN()), &gorm.Config{
		Logger: newGormLogger(cfg.Log, log),
		// 业务表统一用单数复数由 DTO 的 TableName() 决定，关掉 GORM 的自动复数化，
		// 免得表名来源出现两处。
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 MySQL 连接池失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MySQL.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MySQL.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.MySQL.ConnMaxLife)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("MySQL 探活失败: %w", err)
	}
	return gdb, nil
}

func openMongo(ctx context.Context, cfg config.Mongo, logCfg config.Log, log *zap.Logger) (*mongo.Client, *mongo.Database, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 注册表必须在建连接时挂上：编解码器是按 Client 生效的，
	// 漏挂不会报错，只会让 decimal 字段静默写成空文档。详见 bson_decimal.go。
	//
	// 命令监视器同理是按 Client 生效的，而且无论 log.sql 开不开都要挂上：
	// 关掉的只是成功命令那条日志，失败仍然要有人报。
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetRegistry(newDecimalRegistry()).
		SetMonitor(newMongoMonitor(logCfg, log))
	client, err := mongo.Connect(connectCtx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("连接 MongoDB 失败: %w", err)
	}
	if err := client.Ping(connectCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, fmt.Errorf("MongoDB 探活失败: %w", err)
	}
	return client, client.Database(cfg.Database), nil
}

func openRedis(ctx context.Context, cfg config.Redis, logCfg config.Log, log *zap.Logger) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})
	// 在 Ping 之前挂钩子，探活本身也就跟着有日志了。
	client.AddHook(newRedisHook(logCfg, log))

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连接 Redis 失败 (%s): %w", cfg.Addr, err)
	}
	return client, nil
}

// 三个数据源的命令日志（GORM logger、Redis hook、Mongo monitor）在 logging.go。
