package config

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
)

// 两个 TTL 都必须活得比一次分析长。
//
// 这条约束曾经被违反过，而且是静默违反的：它们原先共用 queue.visibility_timeout
// （15 分钟，为 Redis 队列的可见性超时而设），比一次分析的上限（30 分钟）还短。
// 后果没有任何报错：
//
//	进度 TTL 偏短    长任务跑到一半，前端的进度条凭空变空
//	凭据 TTL 偏短    并发凭据提前过期，名额被放出去，实际并发悄悄超过配置上限
//
// 两个都只在「任务跑得久」时才发作，也就是只在最该稳的时候发作。
// 把它写成断言，免得下一个调小这两个值的人重新踩一遍。
func TestQueueTTLsOutliveAnalysisMaxRuntime(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载默认配置失败: %v", err)
	}

	if cfg.Queue.ProgressTTL <= constants.AnalysisMaxRuntime {
		t.Fatalf("queue.progress_ttl(%s) 必须大于 AnalysisMaxRuntime(%s)，"+
			"否则长任务跑到一半前端的进度条就空了",
			cfg.Queue.ProgressTTL, constants.AnalysisMaxRuntime)
	}

	// 凭据要覆盖的不是一次执行，而是一个任务从提交到终态的全过程——
	// 包含最多 MaxAttempts 次各自跑满的重试。提前过期会把名额放出去，
	// 让实际并发悄悄超过配置上限。
	minSlotTTL := constants.AnalysisMaxRuntime * time.Duration(cfg.Queue.MaxAttempts)
	if cfg.Queue.SlotTTL < minSlotTTL {
		t.Fatalf("queue.slot_ttl(%s) 至少要覆盖 MaxAttempts(%d) 次跑满的重试，即 %s",
			cfg.Queue.SlotTTL, cfg.Queue.MaxAttempts, minSlotTTL)
	}
}
