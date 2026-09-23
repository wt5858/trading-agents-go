package providers

import (
	"testing"
	"time"

	scheduling_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// defaultJobs 这张表的每一个字段都是「写错了不报错，只是什么都不发生」的类型：
// payload 的键名拼错 -> 任务每天准时跑、每天准时失败；cron 写错一位 -> 挪到了
// 开盘前；kind 写错 -> 调度器捞得到但没有运行器认领。这些都不会在编译期暴露，
// 也不会在启动时暴露——EnsureJob 只校验 cron 与 kind，payload 是不透明的。
// 所以这张表需要被当成代码来测。

// TestDefaultJobsPayloadMatchesRunner 钉住 payload 的键名与取值。
//
// 键名是 MarketSyncRunner.Run 里读的那两个（kind / market），取值必须分别过得了
// stock 上下文的 SyncKind 与 Market 校验——这两步正是 RunSync 开头做的事，
// 在这里先做一遍，等于把「任务跑起来才发现参数不对」提前到了单元测试。
func TestDefaultJobsPayloadMatchesRunner(t *testing.T) {
	for _, in := range defaultJobs {
		t.Run(in.Name, func(t *testing.T) {
			payload, err := scheduling_vo.NewJobPayload(in.Payload)
			if err != nil {
				t.Fatalf("payload 非法: %v", err)
			}
			kind := payload.Str("kind")
			market := payload.Str("market")
			if kind == "" || market == "" {
				t.Fatalf("payload 必须含 kind 与 market，实际 %v", in.Payload)
			}
			if _, err := stock_vo.NewSyncKind(kind); err != nil {
				t.Fatalf("同步类型 %q 不被 stock 上下文接受: %v", kind, err)
			}
			if !shared_vo.ParseMarket(market).Valid() {
				t.Fatalf("市场 %q 非法", market)
			}
		})
	}
}

// TestDefaultJobsAreSchedulable cron 必须可解析且在未来真的会触发。
//
// 「不会再触发」的表达式（例如把年份写死）不是语法错误，落库之后是静默失效：
// NextRunAt 算不出来，任务再也不会被 ClaimDue 捞到。
func TestDefaultJobsAreSchedulable(t *testing.T) {
	now := time.Now()
	for _, in := range defaultJobs {
		t.Run(in.Name, func(t *testing.T) {
			spec, err := scheduling_vo.NewCronExpression(in.Cron)
			if err != nil {
				t.Fatalf("cron %q 非法: %v", in.Cron, err)
			}
			next := spec.Next(now)
			if next.IsZero() {
				t.Fatalf("cron %q 在未来不会再触发", in.Cron)
			}
			// 一周内必须至少触发一次：默认任务全是「每个交易日」，
			// 算出一个月后才跑的下次触发说明表达式的语义跑偏了。
			if next.After(now.Add(7 * 24 * time.Hour)) {
				t.Fatalf("cron %q 的下次触发在 %s，离现在超过一周", in.Cron, next)
			}
			if _, err := scheduling_vo.NewJobKind(in.Kind); err != nil {
				t.Fatalf("任务种类 %q 非法: %v", in.Kind, err)
			}
		})
	}
}

// TestDefaultJobsNamesAreUnique 名字是 uk_jobs_name，也是 EnsureJob 的幂等键。
//
// 表里出现两条同名任务时，第二条播不进去（唯一键冲突被当成「已存在」咽掉），
// 现象是「我明明在表里写了它，环境里却没有」。
func TestDefaultJobsNamesAreUnique(t *testing.T) {
	seen := make(map[string]bool, len(defaultJobs))
	for _, in := range defaultJobs {
		if in.Name == "" {
			t.Fatal("任务名不能为空")
		}
		if seen[in.Name] {
			t.Fatalf("任务名重复: %s", in.Name)
		}
		seen[in.Name] = true
	}
}

// TestKlineJobTimeoutCoversPerSymbolPath 日线任务的超时必须扛得住降级路径。
//
// 有批量按日端点时这条任务只要几分钟，但那是最好的情况。没有时它退回逐标的：
// A 股约 5900 只，EastmoneyRPS 默认 2 次/秒，下限就是约 50 分钟。
// 超时给成一般任务那个 600 秒，这条任务在降级路径上永远跑不完，
// 而且每次都是跑了十分钟才被掐——既没数据，又白打了一千多次上游请求。
func TestKlineJobTimeoutCoversPerSymbolPath(t *testing.T) {
	const cnSymbols = 5900
	const eastmoneyRPS = 2.0
	worstCase := time.Duration(cnSymbols/eastmoneyRPS) * time.Second

	for _, in := range defaultJobs {
		if in.Payload["kind"] != "klines" {
			continue
		}
		if got := time.Duration(in.TimeoutSeconds) * time.Second; got < worstCase {
			t.Fatalf("%s 超时 %s，逐标的路径最坏要 %s", in.Name, got, worstCase)
		}
		return
	}
	t.Fatal("默认任务里没有日线同步，K 线不会被自动同步")
}
