package value_objects

import (
	"time"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// RunTrace 是一次已经结束的分析的完整轨迹，读路径专用。
//
// 它是值对象而不是重新拼出来的 AnalysisContext：一次跑完的分析没有生命周期了，
// 没有任何不变式需要守，读者要的只是「当时按什么顺序发生了什么、各花了多少」。
// 回一个带锁、带待发布事件、带一整份行情素材的聚合给读路径，
// 只会让调用方误以为手里这份还能接着跑。
type RunTrace struct {
	RunID     string
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Depth     analysis_vo.Depth
	// RequestedModel 是请求里指定的模型，可能是空串（走默认）或一个别名。
	// 每位成员实际落到哪个模型上在 Turns[i].Model 里。
	RequestedModel string

	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration

	// Turns 按 Seq 升序，也就是真实完成顺序。
	Turns []TurnRecord
	// Decision 连同归属一起读回来，读路径不再自行推导「结论是谁的」。
	Decision SettledDecision
	Usage    Usage

	Failed     bool
	FailReason string
}

func (t RunTrace) IsZero() bool { return t.RunID == "" }

// SettledDecision 是收敛之后的终局决策，连同「它是谁给的」。
//
// 两样东西必须一起产出、一起落库。分开算是一个真实踩过的坑：
// 结论走的是「风控经理无效就回落交易员」，而归属如果另按「风控经理有没有发言」
// 判定，两者就会在一种很常见的情况下分叉——风控经理正常写了报告，
// 但末尾的结构化块格式坏了（ParseDecision 退化成 undecided）。
// 此时结论其实来自交易员，归属却写着风控经理终裁。
type SettledDecision struct {
	Decision analysis_vo.Decision
	// DecidedBy 为空表示没有任何成员给出过方向。
	DecidedBy AgentKind
}

// CachedTurn 是一次发言里可以被缓存复用的部分。
//
// 它刻意不含 Usage：消耗回答的是「这次运行花了多少」，而命中缓存意味着没花。
// 把它一起缓存下来，成本统计会把同一笔钱重复计入每一次命中。
type CachedTurn struct {
	Content    string
	ToolRounds int
	Truncated  bool
	// Model 是当初产出这份内容的模型，随缓存一起带回来，让轨迹如实记录。
	Model string
}

// RunSummary 是运行轨迹去掉全部发言正文之后的骨架。
//
// 回测要扫的是几百上千次运行，而每份轨迹里十四段报告正文加起来有几十 KB。
// 用 RunTrace 扫一遍等于把几十 MB 的正文读进内存，而评分只用到
// 「哪只票、哪一天、系统建议了什么」这三样。
// 因此读路径分成两个值对象，各自对应一次投影查询——
// 这不是 CQRS 的读写分离，是同一个仓储上的两种投影。
type RunSummary struct {
	RunID     string
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Decision  analysis_vo.Decision
	Failed    bool
}

// TurnsOfPhase 取某一阶段的发言，顺序不变。
// 多空辩论的决策链就是 TurnsOfPhase(PhaseDebate) 加上 TurnsOfPhase(PhaseRisk)。
func (t RunTrace) TurnsOfPhase(p Phase) []TurnRecord {
	out := make([]TurnRecord, 0, len(t.Turns))
	for _, r := range t.Turns {
		if r.Phase == p {
			out = append(out, r)
		}
	}
	return out
}
