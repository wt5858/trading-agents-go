package config

import (
	"testing"

	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
)

// TestWriteTimeoutOutlastsAnalysis 钉住 HTTP 写超时与分析时长上限的大小关系。
//
// 这两个数分别住在 config 与 constants 里，谁也看不见谁，而它们之间有一条硬约束：
// 写超时必须大于一次分析跑满的时间，否则 net/http 会在分析还没结束时砍断
// /analysis/tasks/{id}/progress 这条 SSE 长连接。
//
// 这个约束以前只写在 setDefaults 的注释里，而值是 10m、分析上限是 30m——
// 注释说的和值做的正好相反。加这条测试是为了让下次有人调整任一侧时立刻知道。
func TestWriteTimeoutOutlastsAnalysis(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载默认配置失败: %v", err)
	}

	if cfg.HTTP.WriteTimeout <= constants.AnalysisMaxRuntime {
		t.Errorf(
			"http.write_timeout = %v，必须大于 AnalysisMaxRuntime = %v；"+
				"否则跑满的分析其 SSE 进度流会被服务端中途砍断",
			cfg.HTTP.WriteTimeout, constants.AnalysisMaxRuntime,
		)
	}
}
