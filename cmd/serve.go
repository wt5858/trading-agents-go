package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/di/injectors"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

var (
	schedulerTick    time.Duration
	disableScheduler bool
)

// ServeCmd 启动服务：HTTP 接口、队列消费者、兜底巡检与调度循环，全都在这一个进程里。
var ServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "启动服务（HTTP + 队列消费者 + 调度循环）",
	Long: `启动服务。

本进程承担四件事：提供 HTTP 接口、消费消息队列（分析任务、定时任务、领域事件）、
运行兜底巡检（捡回卡住的任务、补齐批次结算）、以及定时任务的调度循环。

启动时会自动把数据库表结构推到最新，多副本同时启动由数据库具名锁串行化。`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWithInfra(func(ctx context.Context, cfg *config.Config, log *zap.Logger) error {
			// 初始管理员先于一切创建，且只装配身份上下文：
			// 它不需要 LLM 路由，也不该因为消息队列连不上而失败。
			userService, err := injectors.CreateUserService(ctx, cfg, log)
			if err != nil {
				return err
			}
			// 没配口令就不建号。这不是降级，是唯一安全的默认行为：自动创建一个
			// 口令来自默认值的管理员，等于在每一个「忘了配」的部署上开一个后门。
			// 代价是空库首次启动必须显式给 TA_AUTH_BOOTSTRAP_ADMIN_PASSWORD，
			// 这条 Warn 就是告诉运维该配什么。
			if cfg.Auth.BootstrapAdminPassword == "" {
				log.Warn("未配置 auth.bootstrap_admin_password，跳过初始管理员创建",
					zap.String("hint", "空库首次启动请设置 TA_AUTH_BOOTSTRAP_ADMIN_PASSWORD"))
			} else if err := userService.EnsureBootstrapAdmin(ctx,
				cfg.Auth.BootstrapAdmin, cfg.Auth.BootstrapAdminPassword); err != nil {
				return fmt.Errorf("初始化管理员账号失败: %w", err)
			}

			srv, err := injectors.CreateHTTPServer(ctx, cfg, log)
			if err != nil {
				return err
			}

			// 服务在独立 goroutine 里跑，主 goroutine 专职等待退出信号。
			// 这是进程生命周期管理，不是业务扇出，因此不走 concurrency helper。
			errCh := make(chan error, 1)
			go func() { errCh <- srv.Start() }()

			if !disableConsumers {
				stopBackground, err := startBackground(ctx, cfg, log)
				if err != nil {
					return err
				}
				defer stopBackground()
			} else {
				log.Info("已禁用队列消费者与巡检，本副本只提供 HTTP 接口")
			}

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				log.Info("收到退出信号，开始优雅停机")
				// 用 Background 而不是已取消的 ctx：优雅停机要等在途请求跑完，
				// 拿一个已取消的 context 去关，等于直接掐断它们。
				if err := srv.Shutdown(context.Background()); err != nil {
					log.Error("优雅停机失败", zap.Error(err))
				}
				log.Info("HTTP 服务已退出")
				return nil
			}
		})
	},
}

var disableConsumers bool

// startBackground 挂上消费者并拉起常驻循环，返回一个等待它们收场的函数。
//
// 消费者必须在任何循环启动之前挂好。反过来的话，调度循环可能在消费者就位之前
// 就发出了第一批消息——它们不会丢（队列是持久化的），但会在队列里白等一轮，
// 而排查时看到的现象是「第一次触发总是慢一拍」。
func startBackground(ctx context.Context, cfg *config.Config, log *zap.Logger) (func(), error) {
	runners, err := injectors.CreateWorkerRunners(ctx, cfg, log)
	if err != nil {
		return nil, err
	}

	// 启动时清理上一轮被强杀留下的僵死同步记录。
	// 不清理的话，那些记录会一直占着 running_key 唯一索引，同类同步再也起不来。
	if _, err := runners.Sync.RecoverStaleRuns(ctx); err != nil {
		log.Warn("清理僵死同步记录失败", zap.Error(err))
	}
	// 同理先跑一轮分析任务的停滞巡检。上一次若是被强杀停掉的，那批停在 running
	// 的任务已经没有人会给它们写结局了——等一整个巡检周期才捡起来毫无必要，
	// 而重启恰恰是最常留下它们的时刻。
	if n, err := runners.Analysis.RecoverStale(ctx); err != nil {
		log.Warn("清理卡住的分析任务失败", zap.Error(err))
	} else if n > 0 {
		log.Info("启动时处置了卡住的分析任务", zap.Int("count", n))
	}

	domainEvents, err := injectors.CreateDomainEventHandlers(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if err := domainEvents.StartSubscribe(); err != nil {
		return nil, err
	}

	amqpHandlers, err := injectors.CreateAmqpHandlers(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if err := amqpHandlers.Start(); err != nil {
		return nil, err
	}

	type namedLoop struct {
		name string
		run  func(context.Context) error
	}
	loops := []namedLoop{{name: "analysis-sweeps", run: runners.Analysis.Run}}
	if !disableScheduler {
		tick := schedulerTick
		loops = append(loops, namedLoop{
			name: "scheduler",
			run:  func(ctx context.Context) error { return runners.Scheduler.Loop(ctx, tick) },
		})
	}

	log.Info("后台消费者与巡检已就绪",
		zap.Int("analysis_max_attempts", cfg.Queue.MaxAttempts),
		zap.Bool("scheduler_enabled", !disableScheduler),
		zap.Duration("scheduler_tick", schedulerTick))

	// 循环走并发 helper 而不是裸 goroutine：任一循环异常退出时，另一个要能被
	// 一并收敛，且错误不会被丢掉。它们在后台跑，由 done 通道让停机路径能等到收场。
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := concurrency.ForEach(ctx, loops, len(loops), func(ctx context.Context, l namedLoop) error {
			if runErr := l.run(ctx); runErr != nil && ctx.Err() == nil {
				log.Error("循环异常退出", zap.String("loop", l.name), zap.Error(runErr))
				return runErr
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			log.Error("后台循环异常终止", zap.Error(err))
		}
	}()

	return func() {
		<-done
		log.Info("后台消费者与巡检已停止")
	}, nil
}

func init() {
	ServeCmd.Flags().DurationVar(&schedulerTick, "scheduler-tick", 30*time.Second,
		"调度器巡检间隔；它同时是定时任务的触发精度上限")
	ServeCmd.Flags().BoolVar(&disableScheduler, "no-scheduler", false,
		"不跑调度循环（多副本部署时通常不需要关，抢占本身是安全的）")
	ServeCmd.Flags().BoolVar(&disableConsumers, "no-consumers", false,
		"只提供 HTTP 接口，不消费队列也不跑巡检；用于横向扩容纯 API 副本")
	RootCmd.AddCommand(ServeCmd)
}
