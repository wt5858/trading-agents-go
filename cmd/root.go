// Package cmd 是命令行入口层：解析参数、装配、接管进程生命周期。
//
// 它不包含任何业务逻辑。判断「这个子命令该干什么」在这里，判断「该怎么干」
// 一律在 internal/ 里——本包里出现一个 if 去决定某条业务规则，就是越界了。
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/config"
	"github.com/wt5858/trading-agents-go/internal/di/providers"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

var configFile string

var RootCmd = &cobra.Command{
	Use:   "trading-agents",
	Short: "多智能体股票分析平台",
	Long: `多智能体股票分析平台。

本平台仅用于学习与研究，不构成投资建议。分析结论由大模型生成，可能包含事实性错误。
过往表现不代表未来收益，投资有风险，可能损失本金。`,
	// 子命令跑到一半失败时，再刷一遍用法只会把真正的错误顶出屏幕。
	SilenceUsage: true,
	// 错误由 Execute 统一打印，交给 cobra 打会多出一行 "Error: " 前缀，
	// 而那一行会和我们自己的结构化日志混在一起。
	SilenceErrors: true,
}

func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	RootCmd.PersistentFlags().StringVar(&configFile, "config", "",
		"配置文件路径，留空则按 ./configs/config.yaml 查找")
}

// bootstrap 加载配置并初始化日志。
//
// 不用 cobra.OnInitialize：那个钩子没有返回值，失败时只能 log.Fatal，
// 而 log.Fatal 会跳过所有 defer——连日志缓冲都刷不出去，
// 于是「为什么起不来」这个问题最需要的那几行日志恰好丢了。
func bootstrap() (*config.Config, *zap.Logger, error) {
	cfg, err := config.Load(configFile)
	if err != nil {
		return nil, nil, fmt.Errorf("加载配置失败: %w", err)
	}
	log, err := logger.Init(cfg.Log)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化日志失败: %w", err)
	}
	return cfg, log, nil
}

// runWithInfra 是常驻子命令共用的骨架：配置 -> 日志 -> 退出信号 -> 干活 -> 收尾。
//
// 装配本身交给各子命令自己调用注入器——它们需要的东西不一样，
// 而那个差别正是切成几个注入器的全部意义（见 internal/di/injectors）。
// 本函数只负责两件每个子命令都一样、且都容易写错的事：退出信号的接管顺序，
// 以及基础设施连接的关闭时机。
//
// 表结构迁移不在这里：它发生在第一次建立数据库连接的时候，
// 由注入器链路上的 NewSingletonConnections 负责（见 providers/infra_provider.go）。
// 放在那里而不是这里，是因为「连上了数据库」和「表结构就绪」之间不该有任何
// 别的代码有机会插进来用那条连接。
func runWithInfra(fn func(ctx context.Context, cfg *config.Config, log *zap.Logger) error) error {
	cfg, log, err := bootstrap()
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 收尾用 context.Background() 而不是上面那个 ctx：停机时 ctx 已经被取消了，
	// 拿它去关连接等于要求每个 Close 都在一个已取消的 context 下完成——
	// 那不是优雅停机，那是拔电源。
	defer providers.CloseInfra(context.Background(), log)

	if err := fn(ctx, cfg, log); err != nil {
		log.Error("进程异常退出", zap.Error(err))
		return err
	}
	return nil
}
