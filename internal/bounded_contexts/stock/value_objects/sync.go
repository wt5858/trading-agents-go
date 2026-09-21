package value_objects

import (
	"strings"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// SyncKind 是同步作业的类型。
type SyncKind string

const (
	SyncStockList  SyncKind = "stock_list"
	SyncQuotes     SyncKind = "quotes"
	SyncKlines     SyncKind = "klines"
	SyncFinancials SyncKind = "financials"
	SyncNews       SyncKind = "news"
)

// NewSyncKind 解析同步类型。未知类型直接报错而不是静默退化：
// 定时任务的 payload 里写错一个字，静默降级会让运维以为同步在跑、实际什么都没同步。
func NewSyncKind(s string) (SyncKind, error) {
	switch SyncKind(strings.ToLower(strings.TrimSpace(s))) {
	case SyncStockList:
		return SyncStockList, nil
	case SyncQuotes:
		return SyncQuotes, nil
	case SyncKlines:
		return SyncKlines, nil
	case SyncFinancials:
		return SyncFinancials, nil
	case SyncNews:
		return SyncNews, nil
	}
	return "", custom_errors.Invalid("未知的同步类型: %s", s)
}

func (k SyncKind) String() string { return string(k) }

func (k SyncKind) Valid() bool {
	switch k {
	case SyncStockList, SyncQuotes, SyncKlines, SyncFinancials, SyncNews:
		return true
	}
	return false
}

// PerSymbol 表示该类型是否需要逐标的调用外部数据源。
// 只有 stock_list 是一次批量拉取，其余都要对上千只标的扇出——
// 这个区分决定了要不要走有界并发与分片落库。
func (k SyncKind) PerSymbol() bool { return k != SyncStockList }

// DisplayName 用于运维界面展示。
func (k SyncKind) DisplayName() string {
	switch k {
	case SyncStockList:
		return "股票列表"
	case SyncQuotes:
		return "行情快照"
	case SyncKlines:
		return "历史K线"
	case SyncFinancials:
		return "财务数据"
	case SyncNews:
		return "资讯"
	}
	return string(k)
}

// SyncStatus 是同步运行的状态。
type SyncStatus string

const (
	SyncRunning   SyncStatus = "running"
	SyncSucceeded SyncStatus = "succeeded"
	// SyncPartial 是一个真实的业务结果，不是错误的委婉说法。
	// 同步 5000 只标的、12 只失败，这是一次「成功但有已知缺口」的运行；
	// 把它归为 failed 会让运维很快学会无视这个信号，真正的全量失败反而被淹没。
	SyncPartial  SyncStatus = "partial"
	SyncFailed   SyncStatus = "failed"
	SyncCanceled SyncStatus = "canceled"
)

func (s SyncStatus) Terminal() bool {
	return s == SyncSucceeded || s == SyncPartial || s == SyncFailed || s == SyncCanceled
}

func (s SyncStatus) String() string { return string(s) }

// SyncStats 是一次同步的计数统计。
//
// SuccessRate 是乘除派生值，按规则必须「算一次、落库、读路径只读存量」：
// 读时重算看起来无害，但分片提交后 Total 与 Succeeded 可能来自不同时刻的快照，
// 重算出的比率会和当时实际写进库里的那个值对不上。
type SyncStats struct {
	Total       int             `json:"total"`
	Succeeded   int             `json:"succeeded"`
	Failed      int             `json:"failed"`
	Skipped     int             `json:"skipped"`
	SuccessRate decimal.Decimal `json:"successRate"` // 0-100，两位小数
}

// NewSyncStats 构造统计并一次性算出成功率。
func NewSyncStats(total, succeeded, failed, skipped int) SyncStats {
	st := SyncStats{Total: total, Succeeded: succeeded, Failed: failed, Skipped: skipped}
	st.SuccessRate = computeSuccessRate(total, succeeded)
	return st
}

// RehydrateSyncStats 从库里读回，不重算成功率——存量值才是当时的事实。
func RehydrateSyncStats(total, succeeded, failed, skipped int, successRate decimal.Decimal) SyncStats {
	return SyncStats{
		Total: total, Succeeded: succeeded, Failed: failed,
		Skipped: skipped, SuccessRate: successRate,
	}
}

// Plus 返回累加后的新统计值（值对象不可变），成功率随之重算一次并固化。
func (s SyncStats) Plus(succeeded, failed, skipped int) SyncStats {
	return NewSyncStats(
		s.Total,
		s.Succeeded+succeeded,
		s.Failed+failed,
		s.Skipped+skipped,
	)
}

// WithTotal 设定总数，用于扇出前已知标的数量的场景。
func (s SyncStats) WithTotal(total int) SyncStats {
	return NewSyncStats(total, s.Succeeded, s.Failed, s.Skipped)
}

func (s SyncStats) Processed() int { return s.Succeeded + s.Failed + s.Skipped }

// AllFailed 判定是否颗粒无收。Total 为 0 不算失败——
// 「今天没有需要同步的标的」和「全都同步失败了」是两回事。
func (s SyncStats) AllFailed() bool { return s.Total > 0 && s.Succeeded == 0 && s.Failed > 0 }

func (s SyncStats) HasFailure() bool { return s.Failed > 0 }

func computeSuccessRate(total, succeeded int) decimal.Decimal {
	if total <= 0 {
		return decimal.Zero
	}
	// 原先这里要手写 int64(rate*100+0.5)/100 来做四舍五入，正是因为 float64
	// 的 Round 行为不可靠。decimal 的 Round 就是十进制四舍五入，
	// 两位小数的口径直接对齐 decimal(5,2) 列定义。
	return decimalx.RoundPercent(
		decimal.NewFromInt(int64(succeeded)).
			Div(decimal.NewFromInt(int64(total))).
			Mul(decimal.NewFromInt(100)),
	)
}
